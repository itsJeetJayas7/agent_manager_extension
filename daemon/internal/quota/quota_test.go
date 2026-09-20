package quota

import (
	"testing"
	"time"

	"agent-conductor/internal/providers"
)

func at(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }

func TestNotBlockedWhenEmpty(t *testing.T) {
	g := New()
	now := at("2026-01-01T12:00:00Z")
	if b, _, _, _ := g.Blocked("codex", now); b {
		t.Fatal("empty observations must not block")
	}
}

func TestFiveHourBlocking(t *testing.T) {
	g := New()
	now := at("2026-01-01T12:00:00Z")
	g.Observe("codex", []providers.QuotaWindow{
		{ID: "5h", Exhausted: true, HasReset: true, ResetsAt: at("2026-01-01T13:00:00Z")},
	}, now)
	b, ne, ht, mr := g.Blocked("codex", now)
	if !b || !ht || mr {
		t.Fatalf("b=%v ne=%v ht=%v mr=%v", b, ne, ht, mr)
	}
	if !ne.Equal(at("2026-01-01T13:00:00Z")) {
		t.Fatalf("next eligible %v", ne)
	}
}

func TestWeeklyDominates(t *testing.T) {
	g := New()
	now := at("2026-01-01T12:00:00Z")
	g.Observe("codex", []providers.QuotaWindow{
		{ID: "5h", Exhausted: true, HasReset: true, ResetsAt: at("2026-01-01T13:00:00Z")},
		{ID: "week", Exhausted: true, HasReset: true, ResetsAt: at("2026-01-05T00:00:00Z")},
	}, now)
	b, ne, _, _ := g.Blocked("codex", now)
	if !b {
		t.Fatal("should be blocked")
	}
	// Must NOT resume after only the 5h reset: max over exhausted windows.
	if !ne.Equal(at("2026-01-05T00:00:00Z")) {
		t.Fatalf("weekly must dominate, got %v", ne)
	}
}

func TestMissingResetFlagged(t *testing.T) {
	g := New()
	now := at("2026-01-01T12:00:00Z")
	g.Observe("claude", []providers.QuotaWindow{
		{ID: "week", Exhausted: true, HasReset: false},
	}, now)
	b, _, ht, mr := g.Blocked("claude", now)
	if !b || ht || !mr {
		t.Fatalf("b=%v ht=%v mr=%v — unknown reset must be surfaced, never invented", b, ht, mr)
	}
}

func TestExpiredWindowsDrop(t *testing.T) {
	g := New()
	now := at("2026-01-01T12:00:00Z")
	g.Observe("codex", []providers.QuotaWindow{
		{ID: "5h", Exhausted: false, HasReset: true, ResetsAt: at("2026-01-01T06:00:00Z")},
	}, now)
	if ws := g.Windows("codex"); len(ws) != 0 {
		t.Fatal("stale window should be dropped")
	}
}
