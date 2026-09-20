package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-conductor/internal/db"
	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

// schedule is the single decision function. It is pure with respect to DB
// state: recompute slots, advance batches, dispatch eligible runs, fire
// anchors only when truly idle.
func (c *Conductor) schedule(ctx context.Context) error {
	s := c.Settings()
	now := c.Now()

	// Everything below the dispatch guard still runs while paused: a pause
	// must not freeze the daemon, only the queue. The old code returned
	// immediately when paused, which also skipped quota resolution and left
	// the pause timer unevaluated.
	c.evaluatePauseTimer(now)
	if err := c.resumeQuotaReady(now); err != nil {
		return err
	}
	c.reapStuckCancels(now)

	// FIFO: oldest active batch first. ListBatches is newest-first for the UI.
	batches, err := c.store.ListActiveBatches()
	if err != nil {
		return err
	}
	busyExt := c.externalBusy()
	// Keep the single "why is nothing running?" record honest, then decide
	// whether dispatch is actually held.
	c.updateSystemHolds(s, now, busyExt)
	if c.PauseState().GatesDispatch() {
		return nil // hold dispatch only; everything above still ran
	}
	// One lookup for the whole pass instead of a GetBatch per active run.
	implicitBatch := map[string]bool{}
	for _, b := range batches {
		implicitBatch[b.ID] = b.Implicit
	}

	for _, b := range batches {
		// Recompute slot usage before each batch: dispatches mutate state.
		active, err := c.store.ListRunsByStates(activeStates()...)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		total := 0
		queueActive := 0 // active runs belonging to implicit queue batches
		for _, r := range active {
			counts[r.Provider]++
			total++
			implicit, known := implicitBatch[r.BatchID]
			if !known {
				// Active run whose batch is no longer QUEUED/RUNNING.
				if rb, _ := c.store.GetBatch(r.BatchID); rb != nil {
					implicit = rb.Implicit
					implicitBatch[r.BatchID] = implicit
				}
			}
			if implicit {
				queueActive++
			}
		}
		st := &slotState{counts: counts, total: total, queueActive: queueActive, busyExt: busyExt, s: s, now: now, batchImplicit: b.Implicit}
		if err := c.advanceBatch(ctx, b, st); err != nil {
			c.log.Errorf("advance batch %s: %v", b.ID, err)
		}
	}

	// Window anchor: only when genuinely idle.
	c.maybeAnchor(ctx, s, now, busyExt)
	return nil
}

type slotState struct {
	counts        map[string]int
	total         int
	queueActive   int // active runs from implicit queue batches
	busyExt       map[string]*model.ProviderSession
	s             Settings
	now           time.Time
	batchImplicit bool
}

func (st *slotState) canStart(provider string, isParallel bool) (bool, string) {
	s := st.s
	// Normal queue (implicit batches) is deliberately serial: one at a time.
	if st.batchImplicit && st.queueActive >= 1 {
		return false, "queue is busy"
	}
	if st.total >= s.MaxTotal {
		return false, "total run limit"
	}
	lim := s.MaxClaude
	if provider == model.ProviderCodex {
		lim = s.MaxCodex
	}
	if st.counts[provider] >= lim {
		return false, "provider run limit"
	}
	// External busy policy: FIFO blocks everything; parallel blocks only same provider.
	if ext, busy := st.busyExt[provider]; busy {
		return false, fmt.Sprintf("Waiting — %s is active outside Agent Conductor (%s)", providerTitle(provider), short(ext.Cwd))
	}
	if !isParallel && s.FIFOBlockOnExternal {
		for p, ext := range st.busyExt {
			if ext != nil {
				return false, fmt.Sprintf("Waiting — %s is active outside Agent Conductor", providerTitle(p))
			}
		}
	}
	return true, ""
}

func providerTitle(p string) string {
	if p == model.ProviderClaude {
		return "Claude Code"
	}
	if p == model.ProviderCodex {
		return "Codex"
	}
	return p
}

func short(path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	return path
}

func activeStates() []model.RunState {
	return []model.RunState{
		model.StatePreparing, model.StateStarting, model.StateRunning,
		model.StateWaitingForProvider, model.StateVerifying, model.StateIntegrating,
		model.StateCancelRequested, model.StateAutoResuming,
	}
}

// resumeQuotaReady moves WAITING_FOR_QUOTA runs to AUTO_RESUMING when every
// blocking window has reset.
func (c *Conductor) resumeQuotaReady(now time.Time) error {
	waiting, err := c.store.ListRunsByStates(model.StateWaitingForQuota)
	if err != nil {
		return err
	}
	for _, r := range waiting {
		if r.NextEligibleAt == nil {
			continue // unknown reset: wait for provider data or user action
		}
		if r.NextEligibleAt.After(now) {
			continue
		}
		blocked, _, _, _ := c.guardian.Blocked(r.Provider, now)
		if blocked {
			// windows moved (e.g. weekly still down) — recompute below in continueRun path
			ne, ht, mr := c.nextEligible(r.Provider, now)
			if mr || !ht {
				continue
			}
			r.NextEligibleAt = &ne
			c.store.UpdateRun(r)
			continue
		}
		c.continueRun(r.ID, "quota window reset")
	}
	return nil
}

// cancelWatchdog is how long a run may sit in CANCEL_REQUESTED with no live
// execution before it is forced to CANCELLED.
const cancelWatchdog = 60 * time.Second

// reapStuckCancels frees slots held by cancellations that will never complete.
//
// CANCEL_REQUESTED counts as an active state, so a run that no adapter is
// executing held a slot indefinitely: nothing would ever emit TurnInterrupted
// for it. Two such runs with the default MaxTotal of 2 killed the queue until
// the daemon restarted.
func (c *Conductor) reapStuckCancels(now time.Time) {
	stuck, err := c.store.ListRunsByStates(model.StateCancelRequested)
	if err != nil {
		return
	}
	for _, r := range stuck {
		c.mu.Lock()
		_, live := c.cancels[r.ID]
		c.mu.Unlock()
		if live {
			continue
		}
		since := r.CancelRequestedAt
		if since.IsZero() {
			since = r.FinishedAt
		}
		if since.IsZero() {
			// Pre-watchdog rows carry no timestamp; adopt one and reap on the
			// next pass rather than cancelling something mid-flight.
			r.CancelRequestedAt = now
			c.store.UpdateRun(r)
			continue
		}
		if now.Sub(since) < cancelWatchdog {
			continue
		}
		reason := "cancellation had no live execution after 60s"
		c.store.AddRunEvent(r.ID, model.EventRunCancelled, quoteJSON(reason))
		if c.transition(r.ID, model.StateCancelled, nil, func(x *model.Run) {
			x.FinalOutcome = "CANCELLED"
			x.FinishedAt = now
		}) {
			c.log.Infof("run %s forced to CANCELLED: %s", r.ID, reason)
			c.reviveBatch(r.BatchID)
			c.emitRun(r.ID)
		}
	}
}

// nextEligible computes provider next eligibility, falling back to the
// user-configured Claude weekly reset when provider data lacks timestamps.
func (c *Conductor) nextEligible(provider string, now time.Time) (time.Time, bool, bool) {
	_, ne, ht, missing := c.guardian.Blocked(provider, now)
	if missing && provider == model.ProviderClaude {
		s := c.Settings()
		if s.ClaudeWeeklyResetUTC != "" {
			for _, spec := range strings.Split(s.ClaudeWeeklyResetUTC, ",") {
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(spec)); err == nil && t.After(now) {
					// user-configured override: provider data wins once available
					return t, true, false
				}
			}
		}
	}
	return ne, ht, missing
}

// continueRun resumes the same native session after a wait.
func (c *Conductor) continueRun(runID, why string) {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return
	}
	// Pick a legal target rather than asking for one and giving up when the
	// transition is refused: that left runs wedged in their previous state.
	target := model.StateAutoResuming
	if !model.CanTransition(r.State, target) {
		target = model.StateRunning
		if !model.CanTransition(r.State, target) {
			c.log.Errorf("continue %s: no legal transition out of %s", runID, r.State)
			return
		}
	}
	prompt := model.ContinueAfterQuota
	if !r.MeaningfulProgress {
		// Never meaningfully began: retry the original effective prompt.
		orig, err := c.decryptPrompt(r)
		if err == nil && orig != "" {
			prompt = orig
		}
	}
	c.store.AddRunEvent(r.ID, model.EventAutoResumeStarted, quoteJSON(why))
	if err := c.store.TransitionRun(r.ID, target, nil, func(x *model.Run) {
		x.ActiveNS = r.ActiveNS
	}); err != nil {
		c.log.Errorf("continue %s: %v", runID, err)
		return
	}
	// Re-read: launchExecution derives the dispatch state from what it is given.
	fresh, err := c.store.GetRun(runID)
	if err != nil || fresh == nil {
		return
	}
	c.launchExecution(fresh, prompt, true)
	c.emitRun(r.ID)
}

// ---- crash recovery ----

func (c *Conductor) recoverOnStartup() {
	runs, err := c.store.ListRunsByStates(activeStates()...)
	if err != nil {
		return
	}
	for _, r := range runs {
		// A cancellation that was in flight when the daemon stopped has no
		// execution to interrupt any more.
		if r.State == model.StateCancelRequested {
			c.store.AddRunEvent(r.ID, model.EventRunCancelled, quoteJSON("daemon restarted during cancellation"))
			c.transition(r.ID, model.StateCancelled, nil, func(x *model.Run) {
				x.FinalOutcome = "CANCELLED"
				x.FinishedAt = c.Now()
			})
			c.emitRun(r.ID)
			continue
		}

		// "Survives restarts" should be a fact, not a marketing claim: when the
		// native session is known and the adapter can reattach, continue it
		// instead of dumping the run on the user.
		if r.ProviderSessionID != "" {
			if adapter, ok := c.registry.Get(r.Provider); ok && providers.CanResume(adapter) {
				c.log.Infof("run %s: reconnecting to %s session %s after daemon restart", r.ID, r.Provider, r.ProviderSessionID)
				c.store.AddRunEvent(r.ID, model.EventOrphaned, quoteJSON("daemon restarted; reattaching to the existing session"))
				// ORPHANED is the truthful intermediate state, and the one the
				// resume path transitions out of.
				if c.transition(r.ID, model.StateOrphaned, nil, nil) {
					c.emitRun(r.ID)
					c.continueRun(r.ID, "daemon restarted")
					continue
				}
			}
		}

		reason := "daemon restarted while run was active"
		c.store.AddRunEvent(r.ID, model.EventOrphaned, quoteJSON(reason))
		c.transition(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
			x.ErrorCode = "RESTART"
			x.ErrorMsg = reason
			x.FinishedAt = c.Now()
		})
		c.emitRun(r.ID)
	}
	// Quota-waiting runs keep their persisted deadlines; loop will resume them.
	qw, _ := c.store.ListRunsByStates(model.StateWaitingForQuota)
	for _, r := range qw {
		if r.ProviderSessionID == "" {
			// No session yet: never dispatched — safe to requeue from scratch.
			c.transition(r.ID, model.StateQueued, nil, nil)
		}
		c.emitRun(r.ID)
	}
}

// ---- helpers ----

func quoteJSON(s string) string {
	return `{"reason":"` + strings.ReplaceAll(s, `"`, `\"`) + `"}`
}

func (c *Conductor) decryptPrompt(r *model.Run) (string, error) {
	if r.PromptCipherStore == "" {
		return "", fmt.Errorf("no stored prompt")
	}
	return c.vault.Open(r.PromptCipherStore)
}

// latestSecretBlock returns the current decrypted secret block (if any).
func (c *Conductor) latestSecretBlock() (string, int64) {
	v, err := c.store.LatestSecretVersion()
	if err != nil || v == nil {
		return "", 0
	}
	plain, err := c.vault.Open(v.Cipher())
	if err != nil {
		return "", 0
	}
	return plain, v.ID
}

// CountRunStates returns aggregate queue info for snapshots.
func (c *Conductor) CountRunStates() map[string]int {
	out := map[string]int{}
	runs, err := c.store.ListRunsByStates(allStatesList()...)
	if err != nil {
		return out
	}
	for _, r := range runs {
		out[string(r.State)]++
	}
	return out
}

func allStatesList() []model.RunState {
	return []model.RunState{
		model.StateDraft, model.StateQueued, model.StateWaitingForSlot, model.StateWaitingForExternal,
		model.StatePreparing, model.StateStarting, model.StateRunning, model.StateWaitingForProvider,
		model.StateWaitingForQuota, model.StateAutoResuming, model.StateVerifying, model.StateCompleted,
		model.StateBlocked, model.StateNeedsAttention, model.StateFailed, model.StateCancelRequested,
		model.StateCancelled, model.StateOrphaned, model.StateSkipped, model.StateIntegrating, model.StateAwaitingIntegration,
		model.StatePaused,
	}
}

var _ = db.Store{}

// scheduleDebug exposes schedule for tests.
func (c *Conductor) scheduleDebug() error { return c.schedule(context.Background()) }
