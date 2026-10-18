package scheduler

import (
	"context"
	"encoding/json"
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
	c.updateSystemHolds(s, now)
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
		st := &slotState{counts: counts, total: total, queueActive: queueActive, s: s, now: now, batchImplicit: b.Implicit}
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
	// Only managed runs consume slots. External work is informational and may
	// run concurrently, including on the same provider.
	return true, ""
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
		blocked, ne, hasTime, missing := c.quotaBlocked(r.Provider, now)
		if blocked {
			ne, hasTime, missing = c.nextEligible(r.Provider, now)
			if !hasTime || missing {
				continue
			}
			if r.NextEligibleAt == nil || !r.NextEligibleAt.Equal(ne) {
				c.transition(r.ID, r.State, nil, func(x *model.Run) { x.NextEligibleAt = &ne })
			}
			if ne.After(now) {
				continue
			}
		}
		if r.NextEligibleAt == nil {
			if blocked || len(c.guardian.Windows(r.Provider)) == 0 {
				continue
			}
		} else if r.NextEligibleAt.After(now) {
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

// quotaBlocked installs a user-supplied reset only for an unknown weekly
// window. Persist the deadline while it is still in the future, so crossing
// it (or restarting after it) actually unblocks the queue. Fresh provider
// observations replace this estimate and always win over the override.
func (c *Conductor) quotaBlocked(provider string, now time.Time) (bool, time.Time, bool, bool) {
	if provider == model.ProviderClaude {
		var override time.Time
		for _, spec := range strings.Split(c.Settings().ClaudeWeeklyResetUTC, ",") {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(spec)); err == nil && t.After(now) && (override.IsZero() || t.Before(override)) {
				override = t
			}
		}
		if !override.IsZero() {
			ws := c.guardian.Windows(provider)
			changed := false
			for i := range ws {
				w := &ws[i]
				weekly := containsAny(strings.ToLower(w.ID+" "+w.Name), "week", "seven_day", "7d")
				if w.Exhausted && !w.HasReset && weekly {
					w.ResetsAt, w.HasReset = override, true
					w.Source, w.Confidence = "user_override", "medium"
					changed = true
				}
			}
			if changed {
				c.observeQuota(provider, ws)
			}
		}
	}
	return c.guardian.Blocked(provider, now)
}

func (c *Conductor) nextEligible(provider string, now time.Time) (time.Time, bool, bool) {
	_, ne, ht, missing := c.quotaBlocked(provider, now)
	return ne, ht, missing
}

// continueRun resumes the same native session after a wait.
func (c *Conductor) continueRun(runID, why string) {
	r, err := c.store.GetRun(runID)
	if err != nil || r == nil {
		return
	}
	prompt := c.recoveryPrompt(r)
	c.queueRecovery(r.ID, prompt, why, nil)
}

// ---- crash recovery ----

func (c *Conductor) recoverOnStartup() {
	// Upgrade runs parked solely by the former per-folder trust gate. Keep
	// their position and let the normal scheduler honor pauses/dependencies.
	blocked, err := c.store.ListRunsByStates(model.StateNeedsAttention)
	if err != nil {
		c.log.Errorf("recover workspace trust: %v", err)
	}
	for _, r := range blocked {
		if r.ErrorCode != "WORKSPACE_UNTRUSTED" {
			continue
		}
		if c.transition(r.ID, r.State, nil, func(x *model.Run) {
			x.ErrorCode, x.ErrorMsg = "", ""
		}) {
			c.queueRecovery(r.ID, c.recoveryPrompt(r), "queued folders are trusted", nil)
		}
	}
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
}

// ---- helpers ----

func quoteJSON(s string) string {
	b, _ := json.Marshal(map[string]string{"reason": s})
	return string(b)
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
