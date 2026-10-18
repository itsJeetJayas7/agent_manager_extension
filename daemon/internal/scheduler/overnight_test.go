package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

type observingAdapter struct {
	*providers.Fake
	mu         sync.Mutex
	specs      []providers.ExecSpec
	live, peak int
	turn       func(context.Context, providers.ExecSpec, providers.Sink, int)
}

func (a *observingAdapter) Execute(ctx context.Context, spec providers.ExecSpec, sink providers.Sink) error {
	a.mu.Lock()
	a.specs = append(a.specs, spec)
	n := len(a.specs)
	a.live++
	if a.live > a.peak {
		a.peak = a.live
	}
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.live--; a.mu.Unlock() }()
	if spec.SessionID == "" {
		spec.SessionID = "native-" + spec.RunID
	}
	sink(providers.Event{Kind: providers.EvSessionStarted, SessionID: spec.SessionID, Model: spec.Model})
	if a.turn != nil {
		a.turn(ctx, spec, sink, n)
	} else {
		final := model.MarkerComplete
		if spec.Stage == model.StagePlan {
			final = model.MarkerStagePlan
		}
		sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnCompletedOK, FinalMsg: final})
	}
	return nil
}

func (a *observingAdapter) records() ([]providers.ExecSpec, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]providers.ExecSpec{}, a.specs...), a.peak
}

func observeHarness(t *testing.T) (*harness, *observingAdapter) {
	h := newHarness(t)
	a := &observingAdapter{Fake: providers.NewFakeNamed(model.ProviderClaude)}
	h.registry.Register(a)
	return h, a
}

func TestStagesUseTheirOwnSettingsAndNeverOverlap(t *testing.T) {
	h, a := observeHarness(t)
	s := h.conductor.Settings()
	s.ClaudePlanModel, s.ClaudePlanEffort = "plan-model", "max"
	s.ClaudeBuildModel, s.ClaudeBuildEffort = "build-model", "low"
	s.ClaudeAuditModel, s.ClaudeAuditEffort = "audit-model", "xhigh"
	s.AuditEnabled = true
	h.conductor.UpdateSettings(s)
	_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "build feature", Provider: "claude", Staged: true, Cwd: t.TempDir(), Permission: "bypass"})
	if err != nil {
		t.Fatal(err)
	}
	if !h.waitState(id, 5*time.Second, model.StateCompleted) {
		t.Fatalf("run: %+v", h.run(id))
	}
	specs, peak := a.records()
	if len(specs) != 3 || peak != 1 {
		t.Fatalf("attempts=%d peak=%d", len(specs), peak)
	}
	for i, want := range []struct{ model, effort, permission string }{{"plan-model", "max", "plan"}, {"build-model", "low", "bypass"}, {"audit-model", "xhigh", "plan"}} {
		if specs[i].Model != want.model || specs[i].Effort != want.effort || specs[i].Permission != want.permission {
			t.Fatalf("stage %d: %+v", i, specs[i])
		}
	}
	if specs[1].SessionID != specs[2].SessionID || specs[1].SessionID == "" {
		t.Fatal("stage lost session")
	}
	if h.run(id).AuditCycles != 1 {
		t.Fatal("audit cycle not persisted")
	}
	if h.run(id).ContinuationCount != 2 || h.run(id).EffectiveEffort != "xhigh" {
		t.Fatal("continuation count or actual stage effort not persisted")
	}
}

func TestExternalToolNotificationsPreserveBusyUntilStop(t *testing.T) {
	h := newHarness(t)
	for _, event := range []string{"UserPromptSubmit", "PreToolUse", "Notification", "PostToolUse"} {
		h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: "manual", Event: event})
		if h.conductor.externalBusy()["claude"] == nil {
			t.Fatalf("%s incorrectly released external session", event)
		}
	}
	for _, event := range []string{"Stop", "Notification"} {
		h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: "manual", Event: event})
		if h.conductor.externalBusy()["claude"] != nil {
			t.Fatalf("%s incorrectly marked idle session busy", event)
		}
	}
}

func TestWeeklyOverrideExpiresAndPreservesOtherWindows(t *testing.T) {
	h := newHarness(t)
	now := h.conductor.Now()
	s := h.conductor.Settings()
	s.ClaudeWeeklyResetUTC = now.Add(time.Minute).Format(time.RFC3339)
	h.conductor.UpdateSettings(s)
	h.guardian.Observe("claude", []providers.QuotaWindow{
		{ID: "seven_day", Exhausted: true},
		{ID: "five_hour", Exhausted: true, HasReset: true, ResetsAt: now.Add(2 * time.Minute)},
	}, now)
	blocked, at, hasTime, missing := h.conductor.quotaBlocked("claude", now)
	if !blocked || !hasTime || missing || !at.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("override bypassed other window: %v %v %v %v", blocked, at, hasTime, missing)
	}
	if blocked, _, _, _ := h.conductor.quotaBlocked("claude", now.Add(3*time.Minute)); blocked {
		t.Fatal("past override never released quota")
	}
	rows, _ := h.store.ListQuotaWindows("claude")
	if len(rows) != 2 {
		t.Fatal("reset estimate was not persisted")
	}
	// A new exhaustion cannot keep using an old override indefinitely.
	h.guardian.Observe("claude", []providers.QuotaWindow{{ID: "seven_day", Exhausted: true}}, now.Add(3*time.Minute))
	if blocked, _, _, missing := h.conductor.quotaBlocked("claude", now.Add(3*time.Minute)); !blocked || !missing {
		t.Fatal("stale override bypassed fresh provider exhaustion")
	}
}

func TestPlanMarkerRetriesAreBoundedAndBlockedIsHonored(t *testing.T) {
	for _, final := range []string{"planning in progress", "<AC_RESULT>BLOCKED: need requirements</AC_RESULT>"} {
		t.Run(final, func(t *testing.T) {
			h, a := observeHarness(t)
			a.turn = func(_ context.Context, _ providers.ExecSpec, sink providers.Sink, _ int) {
				sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnCompletedOK, FinalMsg: final})
			}
			s := h.conductor.Settings()
			s.MaxCompletionContinuations = 1
			h.conductor.UpdateSettings(s)
			_, id, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "plan", Provider: "claude", Staged: true, Cwd: t.TempDir()})
			want, attempts := model.StateNeedsAttention, 2
			if strings.Contains(final, "BLOCKED") {
				want, attempts = model.StateBlocked, 1
			}
			if !h.waitState(id, 3*time.Second, want) {
				t.Fatalf("run: %+v", h.run(id))
			}
			specs, _ := a.records()
			if len(specs) != attempts {
				t.Fatalf("attempts=%d", len(specs))
			}
		})
	}
}

type cancellationVerifier struct{ started chan struct{} }

func (v cancellationVerifier) Verify(ctx context.Context, _ string, _ []string) (bool, string) {
	close(v.started)
	<-ctx.Done()
	return false, "cancelled"
}

func TestCancellingVerificationNeverRestartsAgent(t *testing.T) {
	h, a := observeHarness(t)
	v := cancellationVerifier{started: make(chan struct{})}
	h.conductor.verifier = v
	s := h.conductor.Settings()
	s.VerificationCommands = []string{"test command"}
	h.conductor.UpdateSettings(s)
	_, id, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "task", Provider: "claude", Cwd: t.TempDir()})
	select {
	case <-v.started:
	case <-time.After(3 * time.Second):
		t.Fatal("verification not started")
	}
	if err := h.conductor.CancelRun(id); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(id, 2*time.Second, model.StateCancelled) {
		t.Fatalf("run: %+v", h.run(id))
	}
	time.Sleep(50 * time.Millisecond)
	specs, _ := a.records()
	if len(specs) != 1 {
		t.Fatal("cancelled verification restarted agent")
	}
}

func TestTerminalEventDoesNotReleaseSlotUntilProcessExits(t *testing.T) {
	h, a := observeHarness(t)
	release := make(chan struct{})
	a.turn = func(ctx context.Context, spec providers.ExecSpec, sink providers.Sink, n int) {
		sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnCompletedOK, FinalMsg: model.MarkerComplete})
		if n == 1 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	_, first, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "first", Provider: "claude", Cwd: t.TempDir()})
	_, second, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "second", Provider: "claude", Cwd: t.TempDir()})
	if !h.waitState(first, time.Second, model.StateRunning) {
		t.Fatal("not running")
	}
	time.Sleep(50 * time.Millisecond)
	if h.run(second).State.Active() || h.run(first).State.Terminal() {
		t.Fatal("slot released while process alive")
	}
	close(release)
	if !h.waitState(second, 3*time.Second, model.StateCompleted) {
		t.Fatal("queue did not advance")
	}
	_, peak := a.records()
	if peak != 1 {
		t.Fatalf("overlap=%d", peak)
	}
}

func TestTransientRetryPersistsAcrossRestartAndKeepsSession(t *testing.T) {
	h, a := observeHarness(t)
	a.turn = func(ctx context.Context, spec providers.ExecSpec, sink providers.Sink, n int) {
		if n == 1 {
			sink(providers.Event{Kind: providers.EvMessageDelta, Progress: true})
			sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnFailed, Error: &providers.ProviderError{Class: model.ErrNetwork, Message: "connection reset"}})
		} else {
			sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnCompletedOK, FinalMsg: model.MarkerComplete})
		}
	}
	_, id, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "original task", Provider: "claude", Model: "claude-opus-5-5", Effort: "max", Cwd: t.TempDir()})
	if !h.tickUntil(2*time.Second, func() bool { return h.run(id).RetryCount == 1 && h.run(id).NextEligibleAt != nil }) {
		t.Fatal("no retry scheduled")
	}
	session := h.run(id).ProviderSessionID
	h.restart()
	specs, _ := a.records()
	if len(specs) != 1 {
		t.Fatal("retried before deadline")
	}
	h.advance(6 * time.Second)
	if !h.waitState(id, 3*time.Second, model.StateCompleted) {
		t.Fatalf("run: %+v", h.run(id))
	}
	specs, _ = a.records()
	if len(specs) != 2 || specs[1].SessionID != session || specs[1].Model != "claude-opus-5-5" || specs[1].Effort != "max" {
		t.Fatalf("recovery settings lost: %+v", specs)
	}
}

func TestQuotaResetCannotBypassManualPause(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", RateLimit: true, PartialWork: true}})
	h.fakeClaude.SetQuota([]providers.QuotaWindow{{ID: "five_hour", Exhausted: true, HasReset: true, ResetsAt: h.conductor.Now().Add(time.Minute)}})
	_, id, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "task", Provider: "claude", Cwd: t.TempDir()})
	if !h.waitState(id, 2*time.Second, model.StateWaitingForQuota) {
		t.Fatal("not waiting")
	}
	h.conductor.PauseQueue(PauseRequest{})
	h.fakeClaude.SetScripts(nil)
	h.advance(2 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if h.run(id).Attempt != 1 || h.run(id).State.Active() {
		t.Fatal("quota reset bypassed pause")
	}
	h.conductor.ResumeQueue()
	if !h.waitState(id, 3*time.Second, model.StateCompleted) {
		t.Fatal("resume failed")
	}
}

func TestMissingMarkerContinuationIsBounded(t *testing.T) {
	h, a := observeHarness(t)
	a.turn = func(_ context.Context, _ providers.ExecSpec, sink providers.Sink, _ int) {
		sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnCompletedOK, FinalMsg: "still working"})
	}
	s := h.conductor.Settings()
	s.MaxCompletionContinuations = 2
	h.conductor.UpdateSettings(s)
	_, id, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "task", Provider: "claude", Cwd: t.TempDir()})
	if !h.waitState(id, 3*time.Second, model.StateNeedsAttention) {
		t.Fatal("unbounded continuation")
	}
	specs, _ := a.records()
	if len(specs) != 3 {
		t.Fatalf("got %d attempts", len(specs))
	}
}

func TestPermanentFailuresNeverAutomaticallyRetry(t *testing.T) {
	for _, cls := range []model.ErrorClass{model.ErrAuth, model.ErrBilling, model.ErrPermission, model.ErrModelUnavailable, model.ErrWorkspace, model.ErrContextLimit} {
		t.Run(string(cls), func(t *testing.T) {
			h, a := observeHarness(t)
			a.turn = func(_ context.Context, _ providers.ExecSpec, sink providers.Sink, _ int) {
				sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnFailed, Error: &providers.ProviderError{Class: cls, Message: "requires attention"}})
			}
			_, id, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "task", Provider: "claude", Cwd: t.TempDir()})
			if !h.waitState(id, 2*time.Second, model.StateFailed) {
				t.Fatal("permanent failure retried")
			}
			if h.run(id).RetryCount != 0 {
				t.Fatal("retry count")
			}
		})
	}
}

func TestContinuationIsEncryptedAndHonorsPause(t *testing.T) {
	h := newHarness(t)
	_, id, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "task", Provider: "claude", Cwd: t.TempDir()})
	if !h.waitState(id, 2*time.Second, model.StateCompleted) {
		t.Fatal("not complete")
	}
	h.conductor.PauseQueue(PauseRequest{})
	if err := h.conductor.ContinueRun(id, "private continuation text"); err != nil {
		t.Fatal(err)
	}
	r := h.run(id)
	if r.State != model.StateQueued || r.ProviderSessionID == "" {
		t.Fatal("not queued resumably")
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "private continuation") || strings.Contains(r.PendingPromptCipher, "private continuation") {
		t.Fatal("plaintext continuation leaked")
	}
	plain, err := h.vault.Open(r.PendingPromptCipher)
	if err != nil || plain != "private continuation text" {
		t.Fatal("continuation not recoverable")
	}
	if err := h.conductor.CancelRun(id); err != nil {
		t.Fatal(err)
	}
	if h.run(id).State != model.StateCancelled {
		t.Fatal("queued native session should cancel immediately")
	}
}

func TestVerificationTimeoutKillsChildAndBoundsOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	ok, _ := (ShellVerifier{}).Verify(ctx, t.TempDir(), []string{"sleep 30 & wait"})
	if ok || time.Since(start) > 3*time.Second {
		t.Fatal("verification child outlived timeout")
	}
	b := &verificationOutput{}
	for i := 0; i < 1000; i++ {
		b.Write([]byte(strings.Repeat("x", 10000)))
	}
	if len(b.String()) > 8192 {
		t.Fatal("unbounded verifier output")
	}
}
