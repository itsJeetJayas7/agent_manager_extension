package scheduler

import (
	"testing"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

func mustPause(t *testing.T, h *harness, in PauseRequest) PauseState {
	t.Helper()
	st, err := h.conductor.PauseQueue(in)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	return st
}

// §5.7.1 — an indefinite pause holds the queue but never touches work already
// in flight.
func TestPauseIndefiniteHoldsQueueAndLetsActiveRunFinish(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", Duration: 400 * time.Millisecond}})
	_, active, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "in flight", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(active, 2*time.Second, model.StateRunning) {
		t.Fatal("first run should be running")
	}

	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	_, queued, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "held", Provider: model.ProviderClaude, Cwd: t.TempDir()})

	// The in-flight run finishes normally: QUEUE_ONLY never interrupts.
	if !h.waitState(active, 3*time.Second, model.StateCompleted) {
		t.Fatalf("active run did not finish: %s", h.run(active).State)
	}
	h.tickUntil(400*time.Millisecond, func() bool { return false })
	if st := h.run(queued).State; st != model.StateQueued {
		t.Fatalf("queued run dispatched while paused: %s", st)
	}

	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(queued, 3*time.Second, model.StateCompleted) {
		t.Fatalf("queued run did not start after resume: %s", h.run(queued).State)
	}
}

// §5.7.2 — a timer is honoured to the minute, not "some time later".
func TestPauseTimerResumesExactlyWhenDue(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser, DurationMs: int64(time.Hour / time.Millisecond)})
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "later", Provider: model.ProviderClaude, Cwd: t.TempDir()})

	h.advance(59 * time.Minute)
	h.tickUntil(200*time.Millisecond, func() bool { return false })
	if !h.conductor.PauseState().Paused {
		t.Fatal("still paused at 59m of a 1h pause")
	}
	if st := h.run(runID).State; st != model.StateQueued {
		t.Fatalf("run dispatched during the pause: %s", st)
	}

	h.advance(2 * time.Minute) // 61m total
	if !h.tickUntil(2*time.Second, func() bool { return !h.conductor.PauseState().Paused }) {
		t.Fatal("pause did not lift after the deadline")
	}
	if !h.waitState(runID, 3*time.Second, model.StateCompleted) {
		t.Fatalf("run did not dispatch after auto-resume: %s", h.run(runID).State)
	}
}

// §5.7.3 — INCLUDE_ACTIVE stops in-flight work at a safe point and leaves it
// resumable in the same native session, never FAILED.
func TestPauseIncludeActiveParksRunResumably(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "hold", Duration: 10 * time.Second}})
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "hold this", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 2*time.Second, model.StateRunning) {
		t.Fatal("run should be running")
	}
	session := h.run(runID).ProviderSessionID

	mustPause(t, h, PauseRequest{Source: PauseSourceUser, Scope: PauseIncludeActive})
	if !h.waitState(runID, 3*time.Second, model.StatePaused) {
		t.Fatalf("interrupted run should be PAUSED, got %s", h.run(runID).State)
	}
	r := h.run(runID)
	if r.State == model.StateFailed {
		t.Fatal("a paused run must never be FAILED")
	}
	if r.ProviderSessionID != session {
		t.Fatal("pausing must preserve the native session")
	}
	if r.ErrorCode != "" {
		t.Fatalf("a pause is not an error, got errorCode=%q", r.ErrorCode)
	}

	// Resuming continues the same session rather than starting over.
	h.fakeClaude.SetScripts(nil)
	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(runID, 4*time.Second, model.StateCompleted) {
		t.Fatalf("paused run did not resume: %s", h.run(runID).State)
	}
	if h.run(runID).ProviderSessionID != session {
		t.Fatal("resume must reuse the same native session")
	}
}

// §5.7.4 — the timer survives a daemon restart.
func TestPauseTimerSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser, DurationMs: int64(time.Hour / time.Millisecond)})
	before := h.conductor.PauseState()
	if before.ResumeAt == nil {
		t.Fatal("expected a deadline")
	}

	h.restart()

	after := h.conductor.PauseState()
	if !after.Paused {
		t.Fatal("pause did not survive the restart")
	}
	if after.ResumeAt == nil || !after.ResumeAt.Equal(*before.ResumeAt) {
		t.Fatalf("deadline changed across restart: %v -> %v", before.ResumeAt, after.ResumeAt)
	}

	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "after restart", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	h.advance(61 * time.Minute)
	if !h.tickUntil(2*time.Second, func() bool { return !h.conductor.PauseState().Paused }) {
		t.Fatal("restored timer never fired")
	}
	if !h.waitState(runID, 3*time.Second, model.StateCompleted) {
		t.Fatalf("run did not dispatch after the restored timer fired: %s", h.run(runID).State)
	}
}

// §5.7.5 — a deadline that passed while the daemon was down resumes at once.
func TestPauseDeadlineInThePastResumesOnStartup(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser, DurationMs: int64(30 * time.Minute / time.Millisecond)})
	// The clock moves on while the daemon is down.
	h.advance(45 * time.Minute)
	h.restart()
	if h.conductor.PauseState().Paused {
		t.Fatal("an elapsed pause must not outlive the restart")
	}
}

// §5.7.6 — extendPause moves the deadline; clearPauseTimer keeps the pause.
func TestExtendAndClearPauseTimer(t *testing.T) {
	h := newHarness(t)
	st := mustPause(t, h, PauseRequest{Source: PauseSourceUser, DurationMs: int64(30 * time.Minute / time.Millisecond)})
	first := *st.ResumeAt

	st, err := h.conductor.ExtendPause(int64(15 * time.Minute / time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.ResumeAt.Sub(first); got != 15*time.Minute {
		t.Fatalf("extend moved the deadline by %s, want 15m", got)
	}

	st, err = h.conductor.ClearPauseTimer()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Paused {
		t.Fatal("clearing the timer must keep the queue paused")
	}
	if st.ResumeAt != nil {
		t.Fatal("clearing the timer must drop the deadline")
	}
	// Indefinite now: time passing changes nothing.
	h.advance(4 * time.Hour)
	h.tickUntil(200*time.Millisecond, func() bool { return false })
	if !h.conductor.PauseState().Paused {
		t.Fatal("an indefinite pause must not expire")
	}
}

// §5.7.7 — a user pause outranks every system hold and is never cleared by one.
func TestUserPauseIsNotClearedByQuotaHold(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser, Reason: "waiting on the API review"})
	h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "work", Provider: model.ProviderClaude, Cwd: t.TempDir()})

	now := h.conductor.Now()
	// A quota hold arrives and then lifts.
	h.guardian.Observe(model.ProviderClaude, []providers.QuotaWindow{
		{ID: "5h", Exhausted: true, HasReset: true, ResetsAt: now.Add(time.Hour)},
	}, now)
	h.tickUntil(300*time.Millisecond, func() bool { return false })
	if p := h.conductor.PauseState(); p.Source != PauseSourceUser {
		t.Fatalf("a system hold overwrote the user pause: source=%s", p.Source)
	}

	h.guardian.Observe(model.ProviderClaude, nil, now)
	h.tickUntil(300*time.Millisecond, func() bool { return false })
	p := h.conductor.PauseState()
	if !p.Paused || p.Source != PauseSourceUser {
		t.Fatalf("quota auto-resume cleared the user pause: %+v", p)
	}
	if p.Reason != "waiting on the API review" {
		t.Fatalf("the user's note was lost: %q", p.Reason)
	}
}

// A quota hold explains itself and lifts on its own.
func TestQuotaHoldIsReportedAndLifts(t *testing.T) {
	h := newHarness(t)
	h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "work", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	now := h.conductor.Now()
	h.guardian.Observe(model.ProviderClaude, []providers.QuotaWindow{
		{ID: "5h", Exhausted: true, HasReset: true, ResetsAt: now.Add(2 * time.Hour)},
	}, now)
	if !h.tickUntil(time.Second, func() bool { return h.conductor.PauseState().Source == PauseSourceQuota }) {
		t.Fatalf("quota exhaustion was not reported as a hold: %+v", h.conductor.PauseState())
	}
	if r := h.conductor.PauseState().Reason; r == "" {
		t.Fatal("a system hold must explain itself in one plain sentence")
	}
	h.guardian.Observe(model.ProviderClaude, nil, now)
	if !h.tickUntil(time.Second, func() bool { return !h.conductor.PauseState().Paused }) {
		t.Fatal("quota hold did not lift once quota returned")
	}
}

func TestPauseValidation(t *testing.T) {
	h := newHarness(t)
	if _, err := h.conductor.PauseQueue(PauseRequest{DurationMs: -1}); err == nil {
		t.Fatal("negative durations must be rejected")
	}
	if _, err := h.conductor.PauseQueue(PauseRequest{DurationMs: int64(8 * 24 * time.Hour / time.Millisecond)}); err == nil {
		t.Fatal("durations beyond the 7-day clamp must be rejected")
	}
	past := h.conductor.Now().Add(-time.Hour)
	if _, err := h.conductor.PauseQueue(PauseRequest{ResumeAt: &past}); err == nil {
		t.Fatal("a resumeAt in the past must be rejected")
	}
	both := h.conductor.Now().Add(time.Hour)
	if _, err := h.conductor.PauseQueue(PauseRequest{DurationMs: 1000, ResumeAt: &both}); err == nil {
		t.Fatal("durationMs and resumeAt are mutually exclusive")
	}
	// Resuming when not paused is a no-op success.
	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatalf("resume while running should be a no-op success: %v", err)
	}
	// Pausing twice replaces the timer instead of erroring.
	mustPause(t, h, PauseRequest{DurationMs: 60_000})
	st := mustPause(t, h, PauseRequest{DurationMs: 120_000})
	if st.ResumeAt == nil || st.ResumeAt.Sub(h.conductor.Now()) < 110*time.Second {
		t.Fatal("a second pause must replace the timer")
	}
}
