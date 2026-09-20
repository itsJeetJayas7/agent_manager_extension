// Package quota implements the provider-independent Quota Guardian:
// normalized window state, blocking decisions, and next-eligible computation
// across all exhausted windows (five-hour AND weekly).
package quota

import (
	"sync"
	"time"

	"agent-conductor/internal/providers"
)

// Guardian computes eligibility from the freshest provider observations.
//
// It is read and written concurrently from the scheduler loop, provider event
// callbacks, the API server's snapshot handlers and startup restore, so every
// access is guarded. Without the mutex this produced "concurrent map read and
// map write", which is a fatal, unrecoverable runtime error rather than a
// warning.
type Guardian struct {
	mu      sync.RWMutex
	windows map[string][]providers.QuotaWindow // provider -> windows
}

// New creates an empty guardian.
func New() *Guardian { return &Guardian{windows: map[string][]providers.QuotaWindow{}} }

// Observe records the latest windows for a provider.
func (g *Guardian) Observe(provider string, ws []providers.QuotaWindow, now time.Time) {
	kept := make([]providers.QuotaWindow, 0, len(ws))
	for _, w := range ws {
		// drop windows that already reset longer than an hour ago
		if w.HasReset && w.ResetsAt.Before(now.Add(-time.Hour)) && !w.Exhausted {
			continue
		}
		kept = append(kept, w)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.windows[provider] = kept
}

// Blocked reports whether the provider is currently blocked and when every
// exhausted window will have reset. nextEligible = max(reset of every
// exhausted required window); a window with no reset timestamp yields
// hasTime=false (caller must not invent one).
func (g *Guardian) Blocked(provider string, now time.Time) (blocked bool, nextEligible time.Time, hasTime bool, missingReset bool) {
	g.mu.RLock()
	ws := g.windows[provider]
	g.mu.RUnlock()
	for _, w := range ws {
		if !w.Exhausted {
			continue
		}
		// already reset?
		if w.HasReset && !w.ResetsAt.After(now) {
			continue
		}
		blocked = true
		if !w.HasReset {
			missingReset = true
			continue
		}
		if !hasTime {
			nextEligible = w.ResetsAt
			hasTime = true
		} else if w.ResetsAt.After(nextEligible) {
			nextEligible = w.ResetsAt
		}
	}
	return blocked, nextEligible, hasTime, missingReset
}

// Windows returns a copy of the current observations for a provider. Callers
// range over the result outside the lock, so handing out the live slice would
// race with the next Observe.
func (g *Guardian) Windows(provider string) []providers.QuotaWindow {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ws := g.windows[provider]
	if len(ws) == 0 {
		return nil
	}
	out := make([]providers.QuotaWindow, len(ws))
	copy(out, ws)
	return out
}

// Confidence summarizes observation quality for display.
func Confidence(ws []providers.QuotaWindow) string {
	worst := "exact"
	rank := map[string]int{"exact": 0, "high": 1, "medium": 2, "experimental": 3, "unknown": 4}
	for _, w := range ws {
		if r, ok := rank[w.Confidence]; ok && r > rank[worst] {
			worst = w.Confidence
		}
	}
	return worst
}
