package scheduler

import (
	"strings"
	"testing"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

// dispatchOrder returns the run ids in the order the scheduler would start
// them: exactly the ordering the dispatcher reads.
func dispatchOrder(t *testing.T, h *harness) []string {
	t.Helper()
	runs, err := h.store.ListRunsByStates(model.StateQueued, model.StateWaitingForSlot, model.StateWaitingForExternal)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

// §3.1 / §10.5 — "Run Next" must mean next, not last. It used to assign
// time.Now().UnixNano() as the sort key, which is astronomically larger than
// the sequential keys and therefore sent the run to the back of the queue.
func TestRunNextDispatchesBeforeEarlierQueuedRuns(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser}) // freeze the queue so nothing starts

	cwd := t.TempDir()
	_, a, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "first", Provider: model.ProviderClaude, Cwd: cwd})
	_, b, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "second", Provider: model.ProviderClaude, Cwd: cwd})
	_, c, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "third", Provider: model.ProviderClaude, Cwd: cwd})
	_, urgent, _ := h.conductor.EnqueueRun(EnqueueRunInput{
		Prompt: "urgent", Provider: model.ProviderClaude, Cwd: cwd, RunNext: true,
	})

	got := dispatchOrder(t, h)
	want := []string{urgent, a, b, c}
	if len(got) != len(want) {
		t.Fatalf("expected %d queued runs, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatch order %v, want %v (urgent=%s)", got, want, urgent)
		}
	}

	// And end to end: the urgent run really is the one that starts first.
	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(urgent, 3*time.Second, model.StateRunning, model.StateCompleted) {
		t.Fatalf("Run Next did not start first; states a=%s urgent=%s", h.run(a).State, h.run(urgent).State)
	}
	if st := h.run(a).State; st != model.StateQueued && st != model.StateWaitingForSlot {
		t.Fatalf("an earlier run started ahead of Run Next: %s", st)
	}
}

// Run Next twice keeps the most recent request in front.
func TestRunNextTwiceKeepsLatestInFront(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	cwd := t.TempDir()
	h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "base", Provider: model.ProviderClaude, Cwd: cwd})
	_, first, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "u1", Provider: model.ProviderClaude, Cwd: cwd, RunNext: true})
	_, second, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "u2", Provider: model.ProviderClaude, Cwd: cwd, RunNext: true})
	got := dispatchOrder(t, h)
	if got[0] != second || got[1] != first {
		t.Fatalf("order %v; expected %s then %s", got, second, first)
	}
}

// Reordering to the end really puts the run last.
func TestReorderToEnd(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	cwd := t.TempDir()
	_, a, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "a", Provider: model.ProviderClaude, Cwd: cwd})
	_, b, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "b", Provider: model.ProviderClaude, Cwd: cwd})
	if err := h.conductor.ReorderQueue(a, ""); err != nil {
		t.Fatal(err)
	}
	got := dispatchOrder(t, h)
	if got[0] != b || got[1] != a {
		t.Fatalf("order %v; expected %s then %s", got, b, a)
	}
	// And back in front of b.
	if err := h.conductor.ReorderQueue(a, b); err != nil {
		t.Fatal(err)
	}
	got = dispatchOrder(t, h)
	if got[0] != a {
		t.Fatalf("reorder-before failed: %v", got)
	}
}

// Sparse keys must survive repeated insertions between the same neighbours.
func TestReorderRenumbersWhenGapsRunOut(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	cwd := t.TempDir()
	_, a, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "a", Provider: model.ProviderClaude, Cwd: cwd})
	_, target, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "target", Provider: model.ProviderClaude, Cwd: cwd})
	_ = a
	var ids []string
	for i := 0; i < 40; i++ {
		_, id, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "x", Provider: model.ProviderClaude, Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.conductor.ReorderQueue(id, target); err != nil {
			t.Fatalf("reorder %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	order := dispatchOrder(t, h)
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	for _, id := range ids {
		if pos[id] >= pos[target] {
			t.Fatalf("run %s did not land before the target after renumbering", id)
		}
	}
}

// §3.2 / §10.6 — cancelling a run that was never dispatched used to park it in
// CANCEL_REQUESTED forever, permanently consuming one of the two slots.
func TestCancellingQueuedRunReachesCancelledAndFreesItsSlot(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	cwd := t.TempDir()
	_, a, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "a", Provider: model.ProviderClaude, Cwd: cwd})
	_, b, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "b", Provider: model.ProviderClaude, Cwd: cwd})
	_, survivor, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "survivor", Provider: model.ProviderClaude, Cwd: cwd})

	for _, id := range []string{a, b} {
		if err := h.conductor.CancelRun(id); err != nil {
			t.Fatal(err)
		}
		if st := h.run(id).State; st != model.StateCancelled {
			t.Fatalf("cancelling a queued run left it in %s, not CANCELLED", st)
		}
	}
	// Both slots (MaxTotal defaults to 2) must be free again.
	active, err := h.store.ListRunsByStates(activeStates()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("cancelled runs still hold %d slot(s)", len(active))
	}
	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(survivor, 3*time.Second, model.StateCompleted) {
		t.Fatalf("queue was dead after two cancels; survivor=%s", h.run(survivor).State)
	}
}

// A cancellation with no live execution is force-completed by the watchdog
// instead of leaking its slot.
func TestCancelWatchdogReapsStuckCancelRequested(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", Duration: 30 * time.Second}})
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "long", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 2*time.Second, model.StateRunning) {
		t.Fatal("run should be running")
	}
	// Force the pathological shape directly: CANCEL_REQUESTED with nothing in
	// flight, which no provider event will ever resolve.
	h.conductor.mu.Lock()
	delete(h.conductor.cancels, runID)
	h.conductor.mu.Unlock()
	if err := h.store.TransitionRun(runID, model.StateCancelRequested, nil, func(x *model.Run) {
		x.CancelRequestedAt = h.conductor.Now()
	}); err != nil {
		t.Fatal(err)
	}

	h.tickUntil(200*time.Millisecond, func() bool { return false })
	if h.run(runID).State != model.StateCancelRequested {
		t.Fatal("watchdog fired too early")
	}
	h.advance(2 * time.Minute)
	if !h.tickUntil(2*time.Second, func() bool { return h.run(runID).State == model.StateCancelled }) {
		t.Fatalf("watchdog never reaped the stuck cancel: %s", h.run(runID).State)
	}
}

// §3.3 / §10.7 — Delete must actually delete. It used to set an unread flag
// while the plaintext stayed in the database and kept reaching the provider.
func TestDeleteSecretBlockRemovesItFromFuturePrompts(t *testing.T) {
	h := newHarness(t)
	const secret = "sudo password: hunter2-correct-horse"
	if _, err := h.conductor.UpdateSecretBlock(secret); err != nil {
		t.Fatal(err)
	}
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	_, before, _ := h.conductor.EnqueueRun(EnqueueRunInput{
		Prompt: "task", Provider: model.ProviderClaude, Cwd: t.TempDir(), AttachSecrets: true,
	})
	p, err := h.conductor.EffectivePromptForRun(before)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, secret) {
		t.Fatal("precondition failed: the secret should be attached before deletion")
	}

	if err := h.conductor.DeleteSecretBlock(); err != nil {
		t.Fatal(err)
	}

	if h.conductor.HasSecretBlock() {
		t.Fatal("HasSecretBlock still reports a block after deletion")
	}
	if _, _, err := h.conductor.RevealSecretBlock(); err == nil {
		t.Fatal("RevealSecretBlock still returns a deleted secret")
	}
	// A newly queued run must carry no secret section at all.
	_, after, _ := h.conductor.EnqueueRun(EnqueueRunInput{
		Prompt: "task", Provider: model.ProviderClaude, Cwd: t.TempDir(), AttachSecrets: true,
	})
	p, err = h.conductor.EffectivePromptForRun(after)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, secret) {
		t.Fatal("a deleted secret is still being shipped to the provider")
	}
	if strings.Contains(p, "SHARED TESTING / SUDO BLOCK") {
		t.Fatal("an empty secret section is still rendered")
	}
	// Already-queued runs must stop carrying it too.
	p, err = h.conductor.EffectivePromptForRun(before)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, secret) {
		t.Fatal("an already-queued run still carries the deleted secret")
	}
}

// §3.4 / §10.9 — two calls in one clock tick used to produce identical ids,
// violating the PRIMARY KEY mid-batch.
func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := newID("run")
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id after %d iterations: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

// §3.6 — a partial update must not blank the fields it does not mention.
func TestUpdateRunIsAPatch(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{
		Prompt: "task", Provider: model.ProviderClaude, Model: "fake-strong",
		Effort: "high", Permission: "plan", Cwd: t.TempDir(),
	})
	effort := "low"
	if err := h.conductor.UpdateRun(runID, UpdateRunInput{Effort: &effort}); err != nil {
		t.Fatal(err)
	}
	r := h.run(runID)
	if r.Model != "fake-strong" {
		t.Fatalf("model was blanked by a patch that did not mention it: %q", r.Model)
	}
	if r.Permission != "plan" {
		t.Fatalf("permission was blanked: %q", r.Permission)
	}
	if r.Effort != "low" {
		t.Fatalf("effort not applied: %q", r.Effort)
	}
}

// §3.7 — the batch report used to panic on an empty provider string.
func TestBatchReportSurvivesEmptyProvider(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	batchID, runID, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "x", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := h.run(runID)
	r.Provider = ""
	if err := h.store.UpdateRun(r); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conductor.CopyBatchReport(batchID); err != nil {
		t.Fatalf("report failed: %v", err)
	}
}

// §3.8 — an empty row can never satisfy the barrier, so it must be rejected.
func TestBatchWithEmptyRowIsRejected(t *testing.T) {
	h := newHarness(t)
	_, err := h.conductor.ApplyBatch(BatchSpecInput{
		Title: "bad", Cwd: t.TempDir(),
		Rows: []RowSpecInput{{Mode: "LINEAR", Runs: nil}},
	})
	if err == nil {
		t.Fatal("a row with no runs must be rejected, not stall the batch forever")
	}
}

// markWaitingExternal used to decide from the in-memory slice dispatchRow had
// already invalidated in the same tick: a run it still saw as WAITING_FOR_SLOT
// was PREPARING or RUNNING on disk, so the moment a waiting run finally got
// its slot the scheduler asked the store to send it back to QUEUED. The
// transition table refused it — no run was corrupted — but every such refusal
// landed in the daemon log and on the user's run timeline as
// "refused: illegal transition STARTING -> QUEUED".
//
// Found by running two real provider runs through the daemon back to back.
func TestWaitingRunGetsItsSlotWithoutARefusedTransition(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()

	// The first run holds the single slot long enough for the second to be
	// marked WAITING_FOR_SLOT before the slot frees.
	h.fakeClaude.SetScripts([]providers.Script{
		{Match: "holds the slot", Duration: 300 * time.Millisecond, FinalMsg: "done\n" + model.MarkerComplete},
	})

	_, first, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "holds the slot", Provider: model.ProviderClaude, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "waits for the slot", Provider: model.ProviderClaude, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}

	if !h.tickUntil(3*time.Second, func() bool {
		return h.run(second).State == model.StateWaitingForSlot
	}) {
		t.Fatalf("second run never waited for a slot; state=%s", h.run(second).State)
	}

	if !h.waitState(second, 10*time.Second, model.StateCompleted) {
		t.Fatalf("second run did not complete; first=%s second=%s", h.run(first).State, h.run(second).State)
	}

	events, err := h.store.ListRunEvents(second, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Data, "refused") {
			t.Fatalf("scheduler recorded a refused transition on a run it had just dispatched: %s %s", e.Type, e.Data)
		}
	}
}

// The usage counters were persisted, summed by UsageSince and rendered in the
// Usage panel, but nothing ever incremented them: the only assignment in the
// codebase was `x.ActiveNS = r.ActiveNS`. Every run therefore reported 0ms
// active and the panel read as zeros however long the agents actually ran.
//
// Found by reading usageSummary after two real provider runs.
func TestUsageCountersRecordRealTime(t *testing.T) {
	h := newHarness(t)
	cwd := t.TempDir()

	h.fakeClaude.SetScripts([]providers.Script{
		{Match: "measured", Duration: 400 * time.Millisecond, FinalMsg: "done\n" + model.MarkerComplete},
	})

	_, runID, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "measured work", Provider: model.ProviderClaude, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	// The harness runs on virtual time, which the provider's real-time sleep
	// does not move. Advance it while the turn is in flight so there is real
	// elapsed time for the accounting to record.
	if !h.tickUntil(5*time.Second, func() bool { return h.run(runID).State == model.StateRunning }) {
		t.Fatalf("run never started; state=%s", h.run(runID).State)
	}
	h.advance(45 * time.Second)
	if !h.waitState(runID, 10*time.Second, model.StateCompleted) {
		t.Fatalf("run did not complete; state=%s", h.run(runID).State)
	}

	r := h.run(runID)
	if r.ActiveNS <= 0 {
		t.Fatalf("run completed but recorded %dns of active time", r.ActiveNS)
	}

	usage, err := h.conductor.UsageSince(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if usage.ActiveMs <= 0 {
		t.Fatalf("usage summary reports %dms active for a run that really ran", usage.ActiveMs)
	}
	if usage.ByProvider[model.ProviderClaude] <= 0 {
		t.Fatalf("per-provider active time is %dms", usage.ByProvider[model.ProviderClaude])
	}
	if len(usage.ByDay) == 0 {
		t.Fatal("no per-day usage recorded")
	}
}

// Queue wait is measured too, and a run that is requeued does not count its
// original wait twice.
func TestQueueWaitIsMeasuredAndNotDoubleCounted(t *testing.T) {
	h := newHarness(t)
	mustPause(t, h, PauseRequest{Source: PauseSourceUser})
	cwd := t.TempDir()

	_, runID, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "waits", Provider: model.ProviderClaude, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	h.advance(90 * time.Second) // held by the pause
	if _, err := h.conductor.ResumeQueue(); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(runID, 10*time.Second, model.StateCompleted) {
		t.Fatalf("run did not complete; state=%s", h.run(runID).State)
	}

	first := h.run(runID).QueueWaitNS
	if first < int64(60*time.Second) {
		t.Fatalf("queue wait recorded as %v for a run held ~90s", time.Duration(first))
	}

	// Requeue: the new wait is short, so the total must grow by far less than
	// another 90s. Before QueuedAt was re-stamped on entry to QUEUED, the
	// original wait was charged again.
	if err := h.conductor.RetryRun(runID); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(runID, 10*time.Second, model.StateCompleted) {
		t.Fatalf("retried run did not complete; state=%s", h.run(runID).State)
	}
	if grew := h.run(runID).QueueWaitNS - first; grew > int64(30*time.Second) {
		t.Fatalf("requeue charged another %v of queue wait", time.Duration(grew))
	}
}
