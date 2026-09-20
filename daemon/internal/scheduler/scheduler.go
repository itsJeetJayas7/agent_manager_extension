// Package scheduler is the conductor brain: FIFO queue, slot limits,
// external-agent awareness, batch row barriers, parallel worktrees,
// staged intelligence, quota waits, window anchors, and crash recovery.
// All state lives in SQLite; this loop is a projection over it.
package scheduler

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"agent-conductor/internal/db"
	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
	"agent-conductor/internal/quota"
	"agent-conductor/internal/vault"
	"agent-conductor/internal/worktrees"
)

// Settings are conductor tunables persisted in the settings table.
type Settings struct {
	MaxTotal             int      `json:"maxTotalRuns"`
	MaxClaude            int      `json:"maxClaudeRuns"`
	MaxCodex             int      `json:"maxCodexRuns"`
	AnchorCodexEnabled   bool     `json:"anchorCodexEnabled"`
	AnchorClaudeEnabled  bool     `json:"anchorClaudeEnabled"`
	AnchorModelCodex     string   `json:"anchorModelCodex"`
	AnchorModelClaude    string   `json:"anchorModelClaude"`
	ClaudeWeeklyResetUTC string   `json:"claudeWeeklyResetUtc"`
	ClaudePlanModel      string   `json:"claudePlanModel"`
	ClaudePlanEffort     string   `json:"claudePlanEffort"`
	ClaudeBuildModel     string   `json:"claudeBuildModel"`
	ClaudeBuildEffort    string   `json:"claudeBuildEffort"`
	ClaudeAuditModel     string   `json:"claudeAuditModel"`
	ClaudeAuditEffort    string   `json:"claudeAuditEffort"`
	CodexPlanModel       string   `json:"codexPlanModel"`
	CodexPlanEffort      string   `json:"codexPlanEffort"`
	CodexBuildModel      string   `json:"codexBuildModel"`
	CodexBuildEffort     string   `json:"codexBuildEffort"`
	CodexAuditModel      string   `json:"codexAuditModel"`
	CodexAuditEffort     string   `json:"codexAuditEffort"`
	AuditEnabled         bool     `json:"auditEnabled"`
	MaxAuditCycles       int      `json:"maxAuditCycles"`
	MaxVerifyFixCycles   int      `json:"maxVerifyFixCycles"`
	VerificationCommands []string `json:"verificationCommands"`
	ClaudeAPIMode        bool     `json:"claudeApiMode"`
	FIFOBlockOnExternal  bool     `json:"fifoBlockOnExternal"`
	Paused               bool     `json:"paused"`
}

// DefaultSettings returns production defaults.
func DefaultSettings() Settings {
	return Settings{
		MaxTotal: 2, MaxClaude: 2, MaxCodex: 2,
		AnchorModelCodex: "auto", AnchorModelClaude: "auto",
		ClaudePlanModel: "opus", ClaudePlanEffort: "high",
		ClaudeBuildModel: "sonnet", ClaudeBuildEffort: "high",
		ClaudeAuditModel: "opus", ClaudeAuditEffort: "high",
		CodexPlanModel: "", CodexPlanEffort: "high",
		CodexBuildModel: "", CodexBuildEffort: "medium",
		CodexAuditModel: "", CodexAuditEffort: "high",
		MaxAuditCycles: 1, MaxVerifyFixCycles: 2,
		FIFOBlockOnExternal: true,
	}
}

// OutEvent is a daemon -> UI event.
type OutEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// Verifier runs optional verification commands in a worktree.
type Verifier interface {
	Verify(ctx context.Context, cwd string, commands []string) (ok bool, summary string)
}

// Conductor is the scheduling engine.
type Conductor struct {
	store    *db.Store
	vault    *vault.Vault
	registry *providers.Registry
	guardian *quota.Guardian
	wt       *worktrees.Manager
	hookBus  *providers.HookBus
	log      *logging.Logger
	verifier Verifier

	mu      sync.Mutex
	wake    chan struct{}
	stopCh  chan struct{}
	stopped bool
	subsMu  sync.Mutex
	subs    []chan OutEvent
	cancels map[string]context.CancelFunc
	// pauseHeld marks runs interrupted by an INCLUDE_ACTIVE pause, so the
	// resulting TurnInterrupted is parked rather than treated as a failure.
	pauseHeld map[string]bool

	// clock is swapped by tests (virtual clock) while the scheduling loop and
	// every provider callback read it, so it cannot be a plain field.
	clock atomic.Pointer[clockFunc]

	anchorMu sync.Mutex

	settingsMu sync.Mutex
	settings   Settings

	// Pause lives under its own settings key so an unrelated settings write
	// cannot clobber it.
	pauseMu sync.Mutex
	pause   PauseState
}

// New wires the conductor.
func New(store *db.Store, vv *vault.Vault, reg *providers.Registry, g *quota.Guardian, wt *worktrees.Manager, hookBus *providers.HookBus, log *logging.Logger, verifier Verifier) *Conductor {
	c := &Conductor{
		store: store, vault: vv, registry: reg, guardian: g, wt: wt,
		hookBus: hookBus, log: log, verifier: verifier,
		wake: make(chan struct{}, 1), stopCh: make(chan struct{}),
		cancels:   map[string]context.CancelFunc{},
		pauseHeld: map[string]bool{},
	}
	c.SetClock(time.Now)
	c.settings = DefaultSettings()
	if s := c.loadSettings(); s != nil {
		c.settings = *s
	}
	c.pause = PauseState{Scope: PauseQueueOnly}
	c.loadPause()
	return c
}

// clockFunc is the swappable time source.
type clockFunc func() time.Time

// Now returns the (possibly virtual) clock.
func (c *Conductor) Now() time.Time {
	if fn := c.clock.Load(); fn != nil {
		return (*fn)().UTC()
	}
	return time.Now().UTC()
}

// SetClock installs a virtual clock (tests/dev fake quota clock).
func (c *Conductor) SetClock(fn func() time.Time) {
	f := clockFunc(fn)
	c.clock.Store(&f)
}

// Poke wakes the scheduling loop.
func (c *Conductor) Poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Start launches the loop and recovery.
func (c *Conductor) Start(ctx context.Context) {
	c.recoverOnStartup()
	// A pause whose deadline passed while the daemon was down must not
	// silently outlive it.
	c.honourElapsedPause()
	go c.hookEventLoop()
	go c.loop(ctx)
}

// Stop terminates the loop.
func (c *Conductor) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.stopped = true
	close(c.stopCh)
}

// Subscribe registers a UI event listener.
func (c *Conductor) Subscribe() chan OutEvent {
	ch := make(chan OutEvent, 256)
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	c.subs = append(c.subs, ch)
	return ch
}

// Unsubscribe removes a listener.
func (c *Conductor) Unsubscribe(ch chan OutEvent) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for i, s := range c.subs {
		if s == ch {
			c.subs = append(c.subs[:i], c.subs[i+1:]...)
			return
		}
	}
}

func (c *Conductor) emit(typ string, payload any) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- OutEvent{Type: typ, Payload: payload}:
		default:
		}
	}
}

// Settings returns current settings.
func (c *Conductor) Settings() Settings {
	c.settingsMu.Lock()
	defer c.settingsMu.Unlock()
	return c.settings
}

// UpdateSettings persists and applies new settings.
func (c *Conductor) UpdateSettings(s Settings) error {
	c.settingsMu.Lock()
	c.settings = s
	c.settingsMu.Unlock()
	if err := c.store.SetSetting("conductor.settings", s); err != nil {
		return err
	}
	c.applySettings(s)
	c.emit("settingsUpdated", s)
	c.Poke()
	return nil
}

// PatchSettings applies a partial settings object server-side.
//
// Every webview mutation used to read the whole settings object, change one
// field and write it all back, so two concurrent edits silently lost one — and
// a stale snapshot could resurrect old values. A patch touches only the keys
// it names.
func (c *Conductor) PatchSettings(patch json.RawMessage) (Settings, error) {
	c.settingsMu.Lock()
	next := c.settings
	// Unmarshalling onto a populated struct leaves absent keys untouched.
	if err := json.Unmarshal(patch, &next); err != nil {
		c.settingsMu.Unlock()
		return c.settings, err
	}
	c.settings = next
	c.settingsMu.Unlock()
	if err := c.store.SetSetting("conductor.settings", next); err != nil {
		return next, err
	}
	c.applySettings(next)
	c.emit("settingsUpdated", next)
	c.Poke()
	return next, nil
}

func (c *Conductor) applySettings(s Settings) {
	if claude, ok := c.registry.Get(model.ProviderClaude); ok {
		if cl, ok := claude.(*providers.Claude); ok {
			cl.SetAPIMode(s.ClaudeAPIMode)
		}
	}
}

func (c *Conductor) loadSettings() *Settings {
	var s Settings
	ok, err := c.store.GetSetting("conductor.settings", &s)
	if err != nil || !ok {
		return nil
	}
	return &s
}

// ---- external session awareness ----

// busyExternalEvents marks provider sessions as active/idle from hook events.
var busyEvents = map[string]bool{
	"SessionStart":     true,
	"UserPromptSubmit": true,
	"Stop":             false,
	"StopFailure":      false,
	"Interrupt":        false,
	"SessionEnd":       false,
}

// externalBusy reads external (non-managed) active sessions per provider.
func (c *Conductor) externalBusy() map[string]*model.ProviderSession {
	out := map[string]*model.ProviderSession{}
	for _, p := range []string{model.ProviderClaude, model.ProviderCodex} {
		sessions, err := c.store.ListProviderSessions(p)
		if err != nil {
			continue
		}
		for _, s := range sessions {
			if s.Kind != "external" {
				continue
			}
			busy, ok := busyEvents[s.LastEvent]
			if !ok || !busy {
				continue
			}
			// conservative staleness: 45 min without an update
			if c.Now().Sub(s.LastSeenAt) > 45*time.Minute {
				continue
			}
			if _, exists := out[p]; !exists {
				out[p] = s
			}
		}
	}
	return out
}

// ---- main loop ----

// schedulerSafetyNet is the longest the loop sleeps with nothing pending. Work
// is event-driven through Poke(); this only catches anything that forgets to
// poke. It used to run every second regardless, which drove a full scheduling
// pass (and its queries) 86,400 times a day on an idle machine.
const schedulerSafetyNet = 10 * time.Second

func (c *Conductor) loop(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-c.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		if err := c.schedule(ctx); err != nil {
			c.log.Errorf("schedule: %v", err)
		}
		timer.Reset(c.nextWakeDelay())
	}
}

// nextWakeDelay returns when the loop must next run even if nothing pokes it,
// so a pause timer or quota reset fires on time instead of up to a tick late.
func (c *Conductor) nextWakeDelay() time.Duration {
	now := c.Now()
	delay := schedulerSafetyNet
	consider := func(deadline time.Time) {
		if deadline.IsZero() {
			return
		}
		if d := deadline.Sub(now); d > 0 && d < delay {
			delay = d
		} else if d <= 0 {
			delay = 0
		}
	}
	if p := c.PauseState(); p.Paused && p.ResumeAt != nil {
		consider(*p.ResumeAt)
	}
	if waiting, err := c.store.ListRunsByStates(model.StateWaitingForQuota); err == nil {
		for _, r := range waiting {
			if r.NextEligibleAt != nil {
				consider(*r.NextEligibleAt)
			}
		}
	}
	if stuck, err := c.store.ListRunsByStates(model.StateCancelRequested); err == nil && len(stuck) > 0 {
		if delay > cancelWatchdog/4 {
			delay = cancelWatchdog / 4
		}
	}
	// Never busy-loop: a deadline already in the past still fires on the next
	// pass, and the pass itself takes longer than this floor.
	if delay < 50*time.Millisecond {
		delay = 50 * time.Millisecond
	}
	return delay
}
