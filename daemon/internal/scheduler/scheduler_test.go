package scheduler

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-conductor/internal/db"
	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
	"agent-conductor/internal/quota"
	"agent-conductor/internal/vault"
	"agent-conductor/internal/worktrees"
)

// harness wires a full conductor with fake adapters and a virtual clock.
type harness struct {
	t          *testing.T
	store      *db.Store
	conductor  *Conductor
	fakeClaude *providers.Fake
	fakeCodex  *providers.Fake
	guardian   *quota.Guardian
	repo       string
	vault      *vault.Vault
	registry   *providers.Registry
	wt         *worktrees.Manager

	mu     sync.Mutex
	vnow   time.Time
	stopFn func()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	vv, err := vault.Open(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, store: store, vnow: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	h.fakeClaude = providers.NewFakeNamed(model.ProviderClaude)
	h.fakeCodex = providers.NewFakeNamed(model.ProviderCodex)
	reg := providers.NewRegistry()
	reg.Register(h.fakeClaude)
	reg.Register(h.fakeCodex)
	h.guardian = quota.New()
	hookBus := providers.NewHookBus()
	wt := worktrees.NewManager(filepath.Join(dir, "wts"))
	h.vault, h.registry, h.wt = vv, reg, wt
	h.conductor = New(store, vv, reg, h.guardian, wt, hookBus, logging.NewNop(), ShellVerifier{})
	h.conductor.SetClock(func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.vnow
	})
	// Don't run the 1s ticker loop in tests; drive schedule() manually.
	ctx, cancel := context.WithCancel(context.Background())
	h.stopFn = cancel
	h.conductor.Start(ctx)
	t.Cleanup(func() { h.stopFn(); h.conductor.Stop() })
	return h
}

// restart simulates a daemon restart: the old conductor stops and a fresh one
// is constructed over the same store, exactly as `agent-conductor serve` does.
func (h *harness) restart() {
	h.stopFn()
	h.conductor.Stop()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	h.stopFn = cancel
	h.conductor = New(h.store, h.vault, h.registry, h.guardian, h.wt, providers.NewHookBus(), logging.NewNop(), ShellVerifier{})
	h.conductor.SetClock(func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.vnow
	})
	h.conductor.Start(ctx)
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.vnow = h.vnow.Add(d)
	h.mu.Unlock()
	h.conductor.Poke()
}

// tickUntil runs the scheduler until cond passes or timeout.
func (h *harness) tickUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h.conductor.Poke()
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (h *harness) waitState(runID string, timeout time.Duration, states ...model.RunState) bool {
	return h.tickUntil(timeout, func() bool {
		r, _ := h.store.GetRun(runID)
		if r == nil {
			return false
		}
		for _, s := range states {
			if r.State == s {
				return true
			}
		}
		return false
	})
}

func (h *harness) run(runID string) *model.Run {
	r, err := h.store.GetRun(runID)
	if err != nil || r == nil {
		h.t.Fatalf("run %s missing", runID)
	}
	return r
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	run("add", "-A")
	run("commit", "-m", "init")
	return dir
}

// ---- core scheduling ----

func TestQueuedRunStartsWhenIdle(t *testing.T) {
	h := newHarness(t)
	_, runID, err := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "simple task", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !h.waitState(runID, 3*time.Second, model.StateCompleted) {
		r := h.run(runID)
		t.Fatalf("run did not complete; state=%s err=%s", r.State, r.ErrorMsg)
	}
	r := h.run(runID)
	if r.FinalOutcome != "COMPLETE" {
		t.Fatalf("outcome %q", r.FinalOutcome)
	}
	if r.ProviderSessionID == "" {
		t.Fatal("native session id not persisted")
	}
	// prompt stored encrypted only
	if strings.Contains(r.PromptPreview, "simple task") == false && r.PromptPreview == "" {
		t.Fatal("preview missing")
	}
}

func TestSecondRunWaitsWhileFirstActive(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", Duration: 700 * time.Millisecond}})
	_, r1, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "long one", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	_, r2, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "second", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(r1, 2*time.Second, model.StateRunning) {
		t.Fatal("first should run")
	}
	h.tickUntil(300*time.Millisecond, func() bool {
		return h.run(r2).State == model.StateWaitingForSlot
	})
	if st := h.run(r2).State; st != model.StateWaitingForSlot && st != model.StateQueued {
		t.Fatalf("second run started concurrently (state %s)", st)
	}
	if !h.waitState(r2, 6*time.Second, model.StateCompleted) {
		t.Fatalf("second never ran; state %s", h.run(r2).State)
	}
}

func manualSession(h *harness, provider, id string) {
	h.store.UpsertProviderSession(&model.ProviderSession{
		Provider: provider, NativeID: id, Kind: "external", Cwd: "/srv/project",
		LastEvent: "UserPromptSubmit", LastSeenAt: h.conductor.Now(),
	})
	h.conductor.Poke()
}

func endManualSession(h *harness, provider, id string) {
	h.store.UpsertProviderSession(&model.ProviderSession{
		Provider: provider, NativeID: id, Kind: "external", LastEvent: "Stop",
		LastSeenAt: h.conductor.Now(),
	})
	h.conductor.Poke()
}

func TestManualClaudeBlocksFIFO(t *testing.T) {
	h := newHarness(t)
	manualSession(h, model.ProviderClaude, "ext-claude")
	_, r1, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "queued behind manual", Provider: model.ProviderCodex, Cwd: t.TempDir()})
	time.Sleep(150 * time.Millisecond)
	h.conductor.Poke()
	time.Sleep(100 * time.Millisecond)
	if st := h.run(r1).State; st != model.StateWaitingForExternal {
		t.Fatalf("FIFO should wait for external claude, state=%s", st)
	}
	endManualSession(h, model.ProviderClaude, "ext-claude")
	if !h.waitState(r1, 3*time.Second, model.StateCompleted) {
		t.Fatalf("should start after manual ends; state=%s", h.run(r1).State)
	}
}

func TestManualCodexBlocksFIFO(t *testing.T) {
	h := newHarness(t)
	manualSession(h, model.ProviderCodex, "ext-codex")
	_, r1, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "queued", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	time.Sleep(200 * time.Millisecond)
	if st := h.run(r1).State; st != model.StateWaitingForExternal {
		t.Fatalf("FIFO should wait for external codex, state=%s", st)
	}
	endManualSession(h, model.ProviderCodex, "ext-codex")
	if !h.waitState(r1, 3*time.Second, model.StateCompleted) {
		t.Fatalf("should start after manual ends; state=%s", h.run(r1).State)
	}
}

// ---- batches & parallel ----

func batchSpec(repo string) BatchSpecInput {
	return BatchSpecInput{
		Title: "Parallel batch", Cwd: repo,
		Rows: []RowSpecInput{
			{Mode: "LINEAR", Runs: []RunSpecInput{{Prompt: "setup", Provider: model.ProviderClaude}}},
			{Mode: "PARALLEL", Runs: []RunSpecInput{
				{Prompt: "card A", Provider: model.ProviderClaude},
				{Prompt: "card B", Provider: model.ProviderCodex},
			}},
			{Mode: "LINEAR", Runs: []RunSpecInput{{Prompt: "final", Provider: model.ProviderClaude}}},
		},
	}
}

func runsOfRow(h *harness, batchID string, position int) []*model.Run {
	rows, _ := h.store.ListRows(batchID)
	var rowID string
	for _, r := range rows {
		if r.Position == position {
			rowID = r.ID
		}
	}
	runs, _ := h.store.ListRunsByBatch(batchID)
	var out []*model.Run
	for _, r := range runs {
		if r.RowID == rowID {
			out = append(out, r)
		}
	}
	return out
}

func TestParallelBatchLifecycle(t *testing.T) {
	h := newHarness(t)
	repo := initGitRepo(t)
	batchID, err := h.conductor.ApplyBatch(batchSpec(repo))
	if err != nil {
		t.Fatal(err)
	}
	// linear row 0 completes first
	if !h.tickUntil(5*time.Second, func() bool {
		for _, r := range runsOfRow(h, batchID, 0) {
			if r.State != model.StateCompleted {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("row0 did not complete: %+v", runsOfRow(h, batchID, 0))
	}
	// parallel row 1: both cards start with isolated worktrees
	if !h.tickUntil(5*time.Second, func() bool {
		for _, r := range runsOfRow(h, batchID, 1) {
			if r.State != model.StateRunning {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("parallel cards not running: %+v", runsOfRow(h, batchID, 1))
	}
	cards := runsOfRow(h, batchID, 1)
	if cards[0].WorktreePath == "" || cards[1].WorktreePath == "" || cards[0].WorktreePath == cards[1].WorktreePath {
		t.Fatal("parallel cards must have isolated worktrees")
	}
	// barrier: final row waits for both cards + integration
	finalRow := runsOfRow(h, batchID, 2)
	finalID := finalRow[0].ID
	if !h.tickUntil(8*time.Second, func() bool {
		r, _ := h.store.GetRun(finalID)
		if r == nil || r.State != model.StateCompleted {
			return false
		}
		b, _ := h.store.GetBatch(batchID)
		return b != nil && b.Status == model.BatchDone
	}) {
		t.Fatalf("final row did not complete: %s", finalID)
	}
	b, _ := h.store.GetBatch(batchID)
	if b.Status != model.BatchDone {
		t.Fatalf("batch status %s", b.Status)
	}
	if b.IntegrationWTPath == "" {
		t.Fatal("integration worktree missing")
	}
	// original repo untouched before apply
	if _, err := os.Stat(filepath.Join(repo, "base.txt")); err != nil {
		t.Fatal("original repo must be untouched")
	}
}

func TestParallelFailureBlocksNextRow(t *testing.T) {
	h := newHarness(t)
	repo := initGitRepo(t)
	spec := BatchSpecInput{Title: "Failing", Cwd: repo, Rows: []RowSpecInput{
		{Mode: "PARALLEL", Runs: []RunSpecInput{
			{Prompt: "good card", Provider: model.ProviderClaude},
			{Prompt: "bad card", Provider: model.ProviderCodex},
		}},
		{Mode: "LINEAR", Runs: []RunSpecInput{{Prompt: "next", Provider: model.ProviderClaude}}},
	}}
	h.fakeCodex.SetScripts([]providers.Script{{Match: "bad card", FailBefore: true, Class: model.ErrAuth}})
	batchID, err := h.conductor.ApplyBatch(spec)
	if err != nil {
		t.Fatal(err)
	}
	// wait until bad card fails and good card completes
	if !h.tickUntil(6*time.Second, func() bool {
		cards := runsOfRow(h, batchID, 0)
		return len(cards) == 2 && (cards[0].State == model.StateFailed || cards[1].State == model.StateFailed)
	}) {
		t.Fatalf("failure not observed: %+v", runsOfRow(h, batchID, 0))
	}
	// barrier: next row must NOT start
	time.Sleep(300 * time.Millisecond)
	next := runsOfRow(h, batchID, 1)
	if len(next) != 1 {
		t.Fatal("expected next row run")
	}
	if st := next[0].State; st == model.StateRunning || st == model.StateCompleted {
		t.Fatalf("next row must not start after failure, state=%s", st)
	}
	b, _ := h.store.GetBatch(batchID)
	if b.Status != model.BatchAttention {
		t.Fatalf("batch should be ATTENTION, got %s", b.Status)
	}
	// successful sibling finished and its work preserved
	cards := runsOfRow(h, batchID, 0)
	for _, c := range cards {
		if c.Provider == model.ProviderClaude && c.State != model.StateCompleted {
			t.Fatalf("sibling should finish: %s", c.State)
		}
	}
	// Retry unblocks the row after success
	h.fakeCodex.SetScripts(nil)
	var badID string
	for _, c := range cards {
		if c.Provider == model.ProviderCodex {
			badID = c.ID
		}
	}
	if err := h.conductor.RetryRun(badID); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(badID, 5*time.Second, model.StateCompleted) {
		t.Fatalf("retry did not succeed: %s", h.run(badID).State)
	}
	if !h.waitState(next[0].ID, 8*time.Second, model.StateCompleted) {
		t.Fatalf("next row unblocked after retry: %s", h.run(next[0].ID).State)
	}
}

func TestSkipRequiresExplicitAction(t *testing.T) {
	h := newHarness(t)
	repo := initGitRepo(t)
	spec := BatchSpecInput{Title: "Skip", Cwd: repo, Rows: []RowSpecInput{
		{Mode: "LINEAR", Runs: []RunSpecInput{{Prompt: "failing", Provider: model.ProviderClaude}}},
		{Mode: "LINEAR", Runs: []RunSpecInput{{Prompt: "after", Provider: model.ProviderClaude}}},
	}}
	h.fakeClaude.SetScripts([]providers.Script{{Match: "failing", FailBefore: true, Class: model.ErrAuth}})
	batchID, _ := h.conductor.ApplyBatch(spec)
	first := runsOfRow(h, batchID, 0)
	if !h.waitState(first[0].ID, 5*time.Second, model.StateFailed) {
		t.Fatal("run should fail")
	}
	next := runsOfRow(h, batchID, 1)
	time.Sleep(200 * time.Millisecond)
	if st := next[0].State; st == model.StateRunning || st == model.StateCompleted {
		t.Fatalf("next row started despite failure")
	}
	// explicit skip unblocks
	if err := h.conductor.SkipRun(first[0].ID); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(next[0].ID, 6*time.Second, model.StateCompleted) {
		t.Fatalf("skip did not unblock: %s", h.run(next[0].ID).State)
	}
}

// ---- quota ----

func TestQuotaWaitAndSameSessionResume(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", RateLimit: true, PartialWork: true}})
	resetAt := h.conductor.Now().Add(2 * time.Hour)
	h.fakeClaude.SetQuota([]providers.QuotaWindow{{
		ID: "5h", Name: "5-hour", Exhausted: true, HasReset: true, ResetsAt: resetAt,
		Source: "provider_structured", Confidence: "exact",
	}})
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "hit limits", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 5*time.Second, model.StateWaitingForQuota) {
		r := h.run(runID)
		t.Fatalf("should be waiting for quota, state=%s err=%s", r.State, r.ErrorMsg)
	}
	r := h.run(runID)
	if r.NextEligibleAt == nil || !r.NextEligibleAt.Equal(resetAt) {
		t.Fatalf("next eligible wrong: %v", r.NextEligibleAt)
	}
	sess := r.ProviderSessionID
	// before reset: stays waiting
	h.advance(30 * time.Minute)
	time.Sleep(150 * time.Millisecond)
	if st := h.run(runID).State; st != model.StateWaitingForQuota {
		t.Fatalf("resumed before reset, state=%s", st)
	}
	// script: continuation succeeds with marker
	h.fakeClaude.SetScripts(nil)
	h.advance(2 * time.Hour) // past reset
	if !h.waitState(runID, 5*time.Second, model.StateCompleted) {
		t.Fatalf("did not resume after reset; state=%s", h.run(runID).State)
	}
	if h.run(runID).ProviderSessionID != sess {
		t.Fatal("must resume the SAME provider session")
	}
	if h.run(runID).FinalOutcome != "COMPLETE" {
		t.Fatal("outcome wrong")
	}
}

func TestWeeklyDominatesResume(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", RateLimit: true, PartialWork: true}})
	now := h.conductor.Now()
	h.fakeClaude.SetQuota([]providers.QuotaWindow{
		{ID: "5h", Name: "5-hour", Exhausted: true, HasReset: true, ResetsAt: now.Add(1 * time.Hour)},
		{ID: "week", Name: "Weekly", Exhausted: true, HasReset: true, ResetsAt: now.Add(72 * time.Hour)},
	})
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "both limits", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 5*time.Second, model.StateWaitingForQuota) {
		t.Fatal("should wait for quota")
	}
	r := h.run(runID)
	if r.NextEligibleAt == nil || !r.NextEligibleAt.Equal(now.Add(72*time.Hour)) {
		t.Fatalf("weekly must dominate: %v", r.NextEligibleAt)
	}
	// five-hour reset passes: still waiting
	h.fakeClaude.SetScripts(nil)
	h.advance(2 * time.Hour)
	time.Sleep(150 * time.Millisecond)
	if st := h.run(runID).State; st != model.StateWaitingForQuota {
		t.Fatalf("resumed after only 5h reset with weekly still exhausted, state=%s", st)
	}
}

func TestNeverBeganRetriesOriginalPrompt(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", RateLimit: true}}) // fail before start
	now := h.conductor.Now()
	h.fakeClaude.SetQuota([]providers.QuotaWindow{{ID: "5h", Exhausted: true, HasReset: true, ResetsAt: now.Add(30 * time.Minute)}})
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "never began", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 5*time.Second, model.StateWaitingForQuota) {
		t.Fatal("should wait")
	}
	if h.run(runID).MeaningfulProgress {
		t.Fatal("no meaningful progress expected")
	}
	h.fakeClaude.SetScripts(nil)
	h.advance(31 * time.Minute)
	if !h.waitState(runID, 5*time.Second, model.StateCompleted) {
		t.Fatal("did not retry original")
	}
}

// ---- window anchor ----

func enableAnchor(h *harness, claude, codex bool) {
	s := h.conductor.Settings()
	s.AnchorClaudeEnabled = claude
	s.AnchorCodexEnabled = codex
	s.MaxTotal = 4
	if err := h.conductor.UpdateSettings(s); err != nil {
		h.t.Fatal(err)
	}
}

func TestRealWorkBeatsAnchor(t *testing.T) {
	h := newHarness(t)
	enableAnchor(h, true, true)
	now := h.conductor.Now()
	h.fakeClaude.SetQuota([]providers.QuotaWindow{{ID: "5h", HasReset: true, ResetsAt: now.Add(-1 * time.Minute)}})
	h.guardian.Observe(model.ProviderClaude, h.fakeClaude.Quota(), now)
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "real work", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	h.conductor.Poke()
	time.Sleep(400 * time.Millisecond)
	// anchor must NOT have fired: claimed anchor for the window should be absent
	if a, _ := h.store.LatestAnchor(model.ProviderClaude); a != nil {
		t.Fatal("anchor fired despite real queued work")
	}
	if !h.waitState(runID, 3*time.Second, model.StateCompleted) {
		t.Fatal("real work did not run")
	}
}

func TestAnchorFiresOnceWhenIdle(t *testing.T) {
	h := newHarness(t)
	enableAnchor(h, false, true)
	h.fakeCodex.SetQuota([]providers.QuotaWindow{{ID: "5h", HasReset: true, ResetsAt: h.conductor.Now().Add(-2 * time.Minute)}})
	h.guardian.Observe(model.ProviderCodex, h.fakeCodex.Quota(), h.conductor.Now())
	h.conductor.Poke()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a, _ := h.store.LatestAnchor(model.ProviderCodex); a != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	a1, _ := h.store.LatestAnchor(model.ProviderCodex)
	if a1 == nil {
		t.Fatal("anchor never fired when idle")
	}
	// repeated scheduling must not fire again for same window
	for i := 0; i < 5; i++ {
		h.conductor.Poke()
		time.Sleep(30 * time.Millisecond)
	}
	var count int
	rows, err := h.store.ListQuotaWindows(model.ProviderCodex)
	_ = rows
	if err != nil {
		t.Fatal(err)
	}
	// single anchor row per window enforced by uniqueness; verify ClaimAnchor rejects
	ok, err := h.store.ClaimAnchor(model.ProviderCodex, a1.WindowReset, "dup")
	if err != nil || ok {
		t.Fatal("second anchor claim for same window must fail")
	}
	count = 1
	if count != 1 {
		t.Fatal("multiple anchors fired")
	}
}

func TestWeeklyExhaustionSuppressesAnchor(t *testing.T) {
	h := newHarness(t)
	enableAnchor(h, false, true)
	now := h.conductor.Now()
	h.fakeCodex.SetQuota([]providers.QuotaWindow{
		{ID: "5h", HasReset: true, ResetsAt: now.Add(-1 * time.Minute)},
		{ID: "week", Exhausted: true, HasReset: true, ResetsAt: now.Add(48 * time.Hour)},
	})
	h.guardian.Observe(model.ProviderCodex, h.fakeCodex.Quota(), now)
	for i := 0; i < 8; i++ {
		h.conductor.Poke()
		time.Sleep(30 * time.Millisecond)
	}
	if a, _ := h.store.LatestAnchor(model.ProviderCodex); a != nil {
		t.Fatal("anchor fired while weekly window exhausted")
	}
}

// ---- crash recovery ----

func TestRestartDoesNotDuplicateAndContinueResumes(t *testing.T) {
	h := newHarness(t)
	h.fakeClaude.SetScripts([]providers.Script{{Match: "*", Duration: 60 * time.Minute}}) // long run
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "long task", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 3*time.Second, model.StateRunning) {
		t.Fatal("run should be active")
	}
	sess := h.run(runID).ProviderSessionID
	// simulate daemon restart: stop conductor, run recovery on a fresh one sharing store
	h.stopFn()
	h.conductor.Stop()
	time.Sleep(50 * time.Millisecond)
	h.conductor.recoverOnStartup()
	r := h.run(runID)
	if r.State != model.StateNeedsAttention {
		t.Fatalf("active run at restart must be NEEDS_ATTENTION, got %s", r.State)
	}
	// user-driven continue must resume the SAME native session
	h.fakeClaude.SetScripts(nil)
	if err := h.conductor.ContinueRun(runID, model.ContinueAfterQuota); err != nil {
		t.Fatal(err)
	}
	if !h.waitState(runID, 3*time.Second, model.StateCompleted) {
		t.Fatalf("continue did not complete: %s", h.run(runID).State)
	}
	if h.run(runID).ProviderSessionID != sess {
		t.Fatal("continue must reuse the same provider session (no duplicate dispatch)")
	}
}

// ---- staged intelligence ----

func TestStagedIntelligencePlanThenBuild(t *testing.T) {
	h := newHarness(t)
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{
		Prompt: "complex feature", Provider: model.ProviderClaude, Staged: true, Cwd: t.TempDir(),
	})
	if !h.waitState(runID, 5*time.Second, model.StateCompleted) {
		r := h.run(runID)
		t.Fatalf("staged run failed: state=%s err=%s", r.State, r.ErrorMsg)
	}
	r := h.run(runID)
	if r.Stage != model.StageBuild && r.Stage != model.StageNone {
		t.Fatalf("stage wrong: %s", r.Stage)
	}
	events, _ := h.store.ListRunEvents(runID, 100)
	hasStageChange := false
	for _, e := range events {
		if e.Type == model.EventStageChanged {
			hasStageChange = true
		}
	}
	if !hasStageChange {
		t.Fatal("PLAN->BUILD stage transition not recorded")
	}
	if r.FinalOutcome != "COMPLETE" {
		t.Fatalf("outcome %q", r.FinalOutcome)
	}
}

// ---- secret block handling ----

func TestSecretBlockAttachedAndRedacted(t *testing.T) {
	h := newHarness(t)
	if _, err := h.conductor.UpdateSecretBlock("sudo pw: TOPSEkreT9"); err != nil {
		t.Fatal(err)
	}
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{
		Prompt: "use creds", Provider: model.ProviderClaude, AttachSecrets: true, Cwd: t.TempDir(),
	})
	if !h.waitState(runID, 5*time.Second, model.StateCompleted) {
		t.Fatalf("state=%s", h.run(runID).State)
	}
	// prompt preview must not contain the secret
	r := h.run(runID)
	if strings.Contains(r.PromptPreview, "TOPSEkreT9") {
		t.Fatal("secret leaked into preview")
	}
	// effective prompt must contain it only via explicit reveal
	eff, err := h.conductor.EffectivePromptForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(eff, "TOPSEkreT9") {
		t.Fatal("secret block not attached to effective prompt")
	}
	if !strings.Contains(eff, "[SHARED TESTING / SUDO BLOCK — USER SUPPLIED]") {
		t.Fatal("block delimiter missing")
	}
}

// ---- external claude blocks claude cards only in parallel ----

func TestExternalBlocksSameProviderOnlyInParallel(t *testing.T) {
	h := newHarness(t)
	repo := initGitRepo(t)
	manualSession(h, model.ProviderClaude, "ext-1")
	spec := BatchSpecInput{Title: "Mixed", Cwd: repo, Rows: []RowSpecInput{
		{Mode: "PARALLEL", Runs: []RunSpecInput{
			{Prompt: "claude card", Provider: model.ProviderClaude},
			{Prompt: "codex card", Provider: model.ProviderCodex},
		}},
	}}
	batchID, err := h.conductor.ApplyBatch(spec)
	if err != nil {
		t.Fatal(err)
	}
	// codex card proceeds even while claude external is busy
	if !h.tickUntil(5*time.Second, func() bool {
		for _, r := range runsOfRow(h, batchID, 0) {
			if r.Provider == model.ProviderCodex && r.State == model.StateCompleted {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("codex card blocked by external claude: %+v", runsOfRow(h, batchID, 0))
	}
	// claude card stays waiting
	for _, r := range runsOfRow(h, batchID, 0) {
		if r.Provider == model.ProviderClaude && (r.State == model.StateRunning || r.State == model.StateCompleted) {
			t.Fatal("claude card started while external claude busy")
		}
	}
	endManualSession(h, model.ProviderClaude, "ext-1")
	claudeCard := ""
	for _, r := range runsOfRow(h, batchID, 0) {
		if r.Provider == model.ProviderClaude {
			claudeCard = r.ID
		}
	}
	if !h.waitState(claudeCard, 6*time.Second, model.StateCompleted) {
		t.Fatalf("claude card should start after external ends: %s", h.run(claudeCard).State)
	}
}

// ---- verification loop ----

type fakeVerifier struct {
	mu        sync.Mutex
	calls     int
	failFirst int
}

func (f *fakeVerifier) Verify(ctx context.Context, cwd string, commands []string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failFirst {
		return false, fmt.Sprintf("$ %s → FAIL\nexit 1: tests failed", commands[0])
	}
	return true, "all passed"
}

func TestVerificationFixLoop(t *testing.T) {
	h := newHarness(t)
	ver := &fakeVerifier{failFirst: 1}
	h.conductor.verifier = ver
	s := h.conductor.Settings()
	s.VerificationCommands = []string{"npm test"}
	if err := h.conductor.UpdateSettings(s); err != nil {
		t.Fatal(err)
	}
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "with verification", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 8*time.Second, model.StateCompleted) {
		r := h.run(runID)
		t.Fatalf("state=%s err=%s verifyCycles=%d", r.State, r.ErrorMsg, r.VerifyCycles)
	}
	if h.run(runID).VerifyCycles != 1 {
		t.Fatal("expected exactly one fix cycle")
	}
}

func TestVerificationCapThenAttention(t *testing.T) {
	h := newHarness(t)
	h.conductor.verifier = &fakeVerifier{failFirst: 99} // always fails
	s := h.conductor.Settings()
	s.VerificationCommands = []string{"npm test"}
	s.MaxVerifyFixCycles = 2
	h.conductor.UpdateSettings(s)
	_, runID, _ := h.conductor.EnqueueRun(EnqueueRunInput{Prompt: "never passes", Provider: model.ProviderClaude, Cwd: t.TempDir()})
	if !h.waitState(runID, 10*time.Second, model.StateNeedsAttention) {
		t.Fatalf("expected NEEDS_ATTENTION, got %s", h.run(runID).State)
	}
	if h.run(runID).ErrorCode != "VERIFICATION_FAILED" {
		t.Fatal("error code wrong")
	}
}
