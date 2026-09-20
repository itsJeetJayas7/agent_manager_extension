package db

import (
	"path/filepath"
	"testing"
	"time"

	"agent-conductor/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunLifecyclePersistence(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	b := &model.Batch{ID: "b1", Title: "T", Status: model.BatchQueued, BaseCwd: "/tmp", CreatedAt: now}
	if err := s.CreateBatch(b); err != nil {
		t.Fatal(err)
	}
	row := &model.Row{ID: "r1", BatchID: "b1", Position: 0, Mode: model.RowLinear}
	if err := s.CreateRow(row); err != nil {
		t.Fatal(err)
	}
	run := &model.Run{
		ID: "run1", BatchID: "b1", RowID: "r1", Provider: model.ProviderClaude,
		State: model.StateDraft, PromptCipherStore: "CIPHER", PromptHash: "H",
		PromptPreview: "preview", CreatedAt: now, QueuedAt: now, SortKey: 1, Cwd: "/tmp",
	}
	if err := s.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun("run1")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.PromptCipherStore != "CIPHER" {
		t.Fatal("cipher not persisted")
	}
	// legal transition with event
	if err := s.TransitionRun("run1", model.StateQueued, &model.RunEvent{Type: model.EventQueued, Timestamp: now},
		func(r *model.Run) { r.QueuedAt = now }); err != nil {
		t.Fatal(err)
	}
	// illegal transition rejected
	if err := s.TransitionRun("run1", model.StateRunning, nil, nil); err == nil {
		t.Fatal("expected illegal transition error")
	}
	if err := s.TransitionRun("run1", model.StatePreparing, nil, nil, ); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionRun("run1", model.StateStarting, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionRun("run1", model.StateRunning, nil, func(r *model.Run) { r.StartedAt = now }); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRun("run1")
	if got.State != model.StateRunning {
		t.Fatalf("state = %s", got.State)
	}
	events, err := s.ListRunEvents("run1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 5 { // queued evt + state changed per transition
		t.Fatalf("expected >=5 events, got %d", len(events))
	}
}

func TestProviderSessionAndQuota(t *testing.T) {
	s := openTest(t)
	ps := &model.ProviderSession{Provider: "claude", NativeID: "s1", Kind: "external", LastEvent: "UserPromptSubmit", LastSeenAt: time.Now().UTC()}
	if err := s.UpsertProviderSession(ps); err != nil {
		t.Fatal(err)
	}
	ps.LastEvent = "Stop"
	if err := s.UpsertProviderSession(ps); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListProviderSessions("claude")
	if len(list) != 1 || list[0].LastEvent != "Stop" {
		t.Fatal("upsert failed")
	}
	if err := s.UpsertQuotaWindow(QuotaWindowRow{Provider: "codex", WindowID: "5h", Name: "5-hour", UsedPercent: 90, ResetsAt: "2026-01-01T00:00:00Z", Exhausted: true, Source: "provider_structured", Confidence: "exact"}); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.ListQuotaWindows("codex")
	if len(ws) != 1 || !ws[0].Exhausted {
		t.Fatal("quota window wrong")
	}
}

func TestAnchorExactlyOnce(t *testing.T) {
	s := openTest(t)
	ok, err := s.ClaimAnchor("codex", "2026-01-01T05:00:00Z", "a1")
	if err != nil || !ok {
		t.Fatalf("first claim failed: %v %v", ok, err)
	}
	ok, err = s.ClaimAnchor("codex", "2026-01-01T05:00:00Z", "a2")
	if err != nil || ok {
		t.Fatalf("second claim should be rejected: %v %v", ok, err)
	}
	if err := s.CompleteAnchor("codex", "2026-01-01T05:00:00Z", "thr_1"); err != nil {
		t.Fatal(err)
	}
	la, _ := s.LatestAnchor("codex")
	if la == nil || la.CompletedAt == "" {
		t.Fatal("anchor completion not persisted")
	}
}

func TestSecretVersions(t *testing.T) {
	s := openTest(t)
	if v, _ := s.LatestSecretVersion(); v != nil {
		t.Fatal("expected empty initially")
	}
	id1, _ := s.AddSecretVersion("c1", "h1", "")
	id2, _ := s.AddSecretVersion("c2", "h2", "")
	if id1 == id2 {
		t.Fatal("ids must differ")
	}
	v, _ := s.LatestSecretVersion()
	if v.Cipher() != "c2" {
		t.Fatal("latest should be c2")
	}
	byID, _ := s.GetSecretVersion(id1)
	if byID == nil || byID.Cipher() != "c1" {
		t.Fatal("version by id wrong")
	}
}

func TestSortKeyMonotonic(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	b := &model.Batch{ID: "sk", Title: "T", CreatedAt: now}
	s.CreateBatch(b)
	row := &model.Row{ID: "skrow", BatchID: "sk", Position: 0, Mode: model.RowLinear}
	s.CreateRow(row)
	k1, _ := s.NextSortKey()
	s.CreateRun(&model.Run{ID: "sk1", BatchID: "sk", RowID: "skrow", State: model.StateQueued, CreatedAt: now, SortKey: k1, Cwd: "/t"})
	k2, _ := s.NextSortKey()
	if k2 <= k1 {
		t.Fatalf("sort keys not monotonic: %d %d", k1, k2)
	}
}
