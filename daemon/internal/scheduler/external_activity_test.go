package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

func TestExternalSessionActivityLifecycle(t *testing.T) {
	for _, provider := range []string{model.ProviderClaude, model.ProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			h := newHarness(t)
			for _, step := range []struct {
				event, source string
				busy          bool
			}{
				{"SessionStart", "startup", false},
				{"Notification", "", false},
				{"SessionStart", "resume", false},
				{"UserPromptSubmit", "", true},
				{"PreToolUse", "", true},
				{"PostToolUse", "", true},
				{"PostToolUseFailure", "", true},
				{"Notification", "", true},
				{"Stop", "", false},
				{"Notification", "", false},
				{"UserPromptSubmit", "", true},
				{"StopFailure", "", false},
				{"UserPromptSubmit", "", true},
				{"Interrupt", "", false},
				{"UserPromptSubmit", "", true},
				{"SessionEnd", "", false},
				// Reopening a session resets a busy record left by a missed stop.
				{"UserPromptSubmit", "", true},
				{"SessionStart", "resume", false},
			} {
				h.conductor.onHookPayload(providers.HookPayload{
					Provider: provider, SessionID: "manual", Event: step.event,
					Raw: map[string]any{"source": step.source},
				})
				if busy := h.conductor.externalBusy()[provider] != nil; busy != step.busy {
					t.Fatalf("%s (%s): busy=%v, want %v", step.event, step.source, busy, step.busy)
				}
			}
		})
	}
}

func TestClaudeCompactionPreservesTurnActivity(t *testing.T) {
	h := newHarness(t)
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: "manual", Event: event})
		h.conductor.onHookPayload(providers.HookPayload{
			Provider: "claude", SessionID: "manual", Event: "SessionStart",
			Raw: map[string]any{"source": "compact"},
		})
		if busy := h.conductor.externalBusy()["claude"] != nil; busy != (event == "UserPromptSubmit") {
			t.Fatalf("compaction changed turn activity after %s: busy=%v", event, busy)
		}
	}
}

func TestExternalRunsNeverHoldQueue(t *testing.T) {
	h := newHarness(t)
	// A stale webview or stored setting must not revive the retired policy.
	if _, err := h.conductor.PatchSettings(json.RawMessage(`{"fifoBlockOnExternal":true}`)); err != nil {
		t.Fatal(err)
	}
	send := func(id, event string) {
		h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: id, Event: event, Cwd: "/srv/manual"})
	}
	enqueue := func(provider string) string {
		t.Helper()
		_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "queued work", Provider: provider, Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Merely opening a window must not block either provider's FIFO work.
	send("manual", "SessionStart")
	for _, provider := range []string{"claude", "codex"} {
		id := enqueue(provider)
		if !h.waitState(id, 3*time.Second, model.StateCompleted) {
			t.Fatalf("idle session blocked %s: %s", provider, h.run(id).State)
		}
		if h.conductor.PauseState().Paused {
			t.Fatal("idle session displayed a hold")
		}
	}
	send("manual", "UserPromptSubmit")
	send("second", "UserPromptSubmit")
	ids := []string{enqueue("claude"), enqueue("codex")}
	for _, id := range ids {
		if !h.waitState(id, 3*time.Second, model.StateCompleted) {
			t.Fatalf("external activity blocked %s: %s", id, h.run(id).State)
		}
	}
	if h.conductor.PauseState().Paused {
		t.Fatal("external activity paused the queue")
	}
	// Only the user's pause controls dispatch, and ending external work must
	// never clear that pause. Resuming works even while another turn is active.
	mustPause(t, h, PauseRequest{Source: PauseSourceUser, Reason: "manual control"})
	id := enqueue("claude")
	send("manual", "Stop")
	h.tickUntil(150*time.Millisecond, func() bool { return false })
	if h.run(id).State != model.StateQueued || h.conductor.PauseState().Source != PauseSourceUser {
		t.Fatal("external activity changed the manual pause")
	}
	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(id, 3*time.Second, model.StateCompleted) {
		t.Fatalf("manual resume failed during external work: %s", h.run(id).State)
	}
}

func TestRestartClearsLegacyExternalHoldAndWaitingRuns(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "after upgrade", Provider: "claude", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h.conductor.transition(id, model.StateWaitingForExternal, nil, nil)
	h.stopFn()
	h.conductor.Stop()
	if err := h.store.UpsertProviderSession(&model.ProviderSession{
		Provider: "claude", NativeID: "running-before-upgrade", Kind: "external",
		LastEvent: "UserPromptSubmit", LastSeenAt: h.conductor.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(pauseSettingKey, PauseState{Paused: true, Source: PauseSourceExternal}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if h.conductor.PauseState().Paused {
		t.Fatal("legacy external hold was not cleared at startup")
	}
	if !h.waitState(id, 3*time.Second, model.StateCompleted) || h.conductor.PauseState().Paused {
		t.Fatal("legacy waiting run did not dispatch during external work")
	}
}

func TestExternalOverlapNotifiesOncePerTurnDuringManagedWork(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", Duration: 10 * time.Second}})
	_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "managed work", Provider: "claude", Cwd: t.TempDir()})
	if err != nil || !h.waitState(id, 2*time.Second, model.StateRunning) {
		t.Fatalf("managed run did not start: %v", err)
	}
	ch := h.conductor.Subscribe()
	defer h.conductor.Unsubscribe(ch)
	assertNotices := func(want int) {
		t.Helper()
		count := 0
		for {
			select {
			case event := <-ch:
				if event.Type == "externalRunOverlap" {
					count++
					p := event.Payload.(ExternalRunOverlap)
					if p.ActiveRunCount != 1 || p.SessionID != "manual" || p.QueuePaused != h.conductor.PauseState().Paused {
						t.Fatalf("unexpected overlap: %+v", p)
					}
				}
			default:
				if count != want {
					t.Fatalf("got %d overlap notices, want %d", count, want)
				}
				return
			}
		}
	}
	for _, provider := range []string{"claude", "codex"} {
		send := func(event string) {
			h.conductor.onHookPayload(providers.HookPayload{Provider: provider, SessionID: "manual", Event: event})
		}
		send("SessionStart")
		send("Notification")
		assertNotices(0)
		for _, event := range []string{"UserPromptSubmit", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Notification"} {
			send(event)
		}
		assertNotices(1)
		if provider == "claude" {
			h.conductor.onHookPayload(providers.HookPayload{Provider: provider, SessionID: "manual", Event: "SessionStart", Raw: map[string]any{"source": "compact"}})
			send("PostToolUse")
			assertNotices(0)
		}
		send("Stop")
		send("UserPromptSubmit")
		assertNotices(1)
		send("StopFailure")
		// Tool activity recovers awareness when the initial prompt hook was missed.
		send("PreToolUse")
		assertNotices(1)
		h.conductor.onHookPayload(providers.HookPayload{Provider: provider, SessionID: "managed", RunID: id, Event: "UserPromptSubmit"})
		assertNotices(0)
	}
	if h.run(id).State != model.StateRunning || h.conductor.PauseState().Paused {
		t.Fatal("advisory changed managed execution")
	}
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: "manual", Event: "Stop"})
	h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: "manual", Event: "UserPromptSubmit"})
	assertNotices(1) // the current managed run still executes under QUEUE_ONLY
}

func TestExternalOverlapIgnoresIdleAndManuallyPausedQueues(t *testing.T) {
	h := newHarness(t)
	ch := h.conductor.Subscribe()
	defer h.conductor.Unsubscribe(ch)
	h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: "idle", Event: "UserPromptSubmit"})
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	_, _, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "waiting", Provider: "claude", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h.conductor.onHookPayload(providers.HookPayload{Provider: "claude", SessionID: "paused", Event: "UserPromptSubmit"})
	for {
		select {
		case event := <-ch:
			if event.Type == "externalRunOverlap" {
				t.Fatal("notified without any active managed work")
			}
		default:
			return
		}
	}
}

func TestExternalActivityAllowsManagedContinuation(t *testing.T) {
	h, adapter := observeHarness(t)
	manualSession(h, "claude", "external")
	adapter.turn = func(_ context.Context, _ providers.ExecSpec, sink providers.Sink, attempt int) {
		final := "Still working"
		if attempt > 1 {
			final = model.MarkerComplete
		}
		sink(providers.Event{Kind: providers.EvTurnCompleted, Status: providers.TurnCompletedOK, FinalMsg: final})
	}
	_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "continue work", Provider: "claude", Cwd: t.TempDir()})
	if err != nil || !h.waitState(id, 3*time.Second, model.StateCompleted) {
		t.Fatalf("external activity blocked continuation: %v", err)
	}
	attempts, peak := adapter.records()
	if len(attempts) != 2 || peak != 1 || attempts[1].SessionID == "" {
		t.Fatalf("continuation failed to preserve normal dispatch: attempts=%+v peak=%d", attempts, peak)
	}
}

func TestExternalNotificationsCannotCreateQuotaHolds(t *testing.T) {
	h := newHarness(t)
	for _, notification := range []string{"usage_limit", "quota_auto_resume_fired"} {
		h.conductor.onHookPayload(providers.HookPayload{
			Provider: "claude", SessionID: "manual", Event: "Notification",
			NotificationType: notification, NotificationMessage: "rate limit notification",
		})
	}
	if len(h.guardian.Windows("claude")) != 0 {
		t.Fatal("external notification changed managed quota")
	}
	_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "managed work", Provider: "claude", Cwd: t.TempDir()})
	if err != nil || !h.waitState(id, 3*time.Second, model.StateCompleted) || h.conductor.PauseState().Paused {
		t.Fatalf("external notification blocked dispatch: %v", err)
	}
	// The same evidence from a managed session retains the existing quota path.
	h.conductor.onHookPayload(providers.HookPayload{
		Provider: "claude", SessionID: "managed", RunID: id, Event: "Notification",
		NotificationType: "usage_limit", NotificationMessage: "rate limit reached",
	})
	if len(h.guardian.Windows("claude")) == 0 {
		t.Fatal("managed quota evidence was ignored")
	}
}
