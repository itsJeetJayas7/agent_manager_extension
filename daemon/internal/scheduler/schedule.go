package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-conductor/internal/db"
	"agent-conductor/internal/model"
)

// schedule is the single decision function. It is pure with respect to DB
// state: recompute slots, advance batches, dispatch eligible runs, fire
// anchors only when truly idle.
func (c *Conductor) schedule(ctx context.Context) error {
	s := c.Settings()
	if s.Paused {
		return nil
	}
	now := c.Now()

	if err := c.resumeQuotaReady(now); err != nil {
		return err
	}

	batches, err := c.store.ListBatches(500)
	if err != nil {
		return err
	}
	busyExt := c.externalBusy()

	for _, b := range batches {
		if b.Status != model.BatchQueued && b.Status != model.BatchRunning {
			continue
		}
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
			if rb, _ := c.store.GetBatch(r.BatchID); rb != nil && rb.Implicit {
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
	if !model.CanTransition(r.State, model.StateAutoResuming) {
		// e.g. NEEDS_ATTENTION -> RUNNING allowed via table
		if !model.CanTransition(r.State, model.StateRunning) {
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
	if err := c.store.TransitionRun(r.ID, model.StateAutoResuming, nil, func(x *model.Run) {
		x.ActiveNS = r.ActiveNS
	}); err != nil {
		c.log.Errorf("continue %s: %v", runID, err)
		return
	}
	c.launchExecution(r, prompt, true)
	c.emitRun(r.ID)
}

// ---- crash recovery ----

func (c *Conductor) recoverOnStartup() {
	runs, err := c.store.ListRunsByStates(activeStates()...)
	if err != nil {
		return
	}
	for _, r := range runs {
		reason := "daemon restarted while run was active"
		c.store.AddRunEvent(r.ID, model.EventOrphaned, quoteJSON(reason))
		c.store.TransitionRun(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
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
			c.store.TransitionRun(r.ID, model.StateQueued, nil, nil)
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
	}
}

var _ = db.Store{}

// scheduleDebug exposes schedule for tests.
func (c *Conductor) scheduleDebug() error { return c.schedule(context.Background()) }
