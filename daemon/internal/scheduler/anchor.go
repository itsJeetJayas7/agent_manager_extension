package scheduler

import (
	"context"
	"strings"
	"sync"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
	"agent-conductor/internal/worktrees"
)

func worktreesApplyOptions(b *model.Batch) worktrees.ApplyOptions {
	return worktrees.ApplyOptions{
		Repo:              b.BaseCwd,
		OriginalBranch:    b.BaseBranch,
		OriginalCommit:    b.BaseCommit,
		IntegrationBranch: b.IntegrationBranch,
	}
}

// ---- hook event loop: external session awareness ----

func (c *Conductor) hookEventLoop() {
	ch := c.hookBus.Subscribe()
	defer c.hookBus.Unsubscribe(ch)
	for {
		select {
		case <-c.stopCh:
			return
		case p := <-ch:
			c.onHookPayload(p)
		}
	}
}

// onHookPayload records external (manual) provider session activity. Managed
// sessions carry RunID and are consumed by the Claude adapter directly.
func (c *Conductor) onHookPayload(p providers.HookPayload) {
	if p.Provider == "" || p.Event == "" {
		return
	}
	kind := "external"
	runID := ""
	if p.RunID != "" {
		kind = "managed"
		runID = p.RunID
	}
	sessID := p.SessionID
	if sessID == "" {
		sessID = p.ThreadID
	}
	if sessID == "" {
		return
	}
	// Notify on the transition into an external turn, never on opening a
	// window, duplicate prompt hooks, tool events in that turn, or reconnects.
	startedExternal := false
	if kind == "external" && busyEvents[p.Event] {
		previous, err := c.store.GetProviderSession(p.Provider, sessID)
		if err != nil {
			c.log.Errorf("read external session activity: %v", err)
		} else {
			startedExternal = !externalSessionBusy(previous, c.Now())
		}
	}
	ps := &model.ProviderSession{
		Provider: p.Provider, NativeID: sessID, RunID: runID, Kind: kind,
		Cwd: p.Cwd, Model: p.Model, LastEvent: p.Event, LastSeenAt: c.Now(),
	}
	if _, changesBusy := busyEvents[p.Event]; !changesBusy {
		ps.LastEvent = "" // refresh liveness without losing the last busy/idle decision
	}
	// Claude also emits SessionStart after compaction inside a turn. Preserve
	// that turn's activity; opening/resuming a session otherwise starts idle.
	if p.Provider == model.ProviderClaude && p.Event == "SessionStart" && p.Raw["source"] == "compact" {
		ps.LastEvent = ""
	}
	if err := c.store.UpsertProviderSession(ps); err != nil {
		c.log.Errorf("hook payload persist: %v", err)
		return
	}
	if kind == "external" {
		c.emit("externalSessionUpdated", p.Provider)
		if startedExternal {
			c.notifyExternalOverlap(p.Provider, sessID)
		}
	}
	// Only managed sessions can update Conductor's quota state. External
	// sessions may use a different account, and generic notifications (even
	// quota-auto-resume notices) must not turn informational activity into a hold.
	if kind == "managed" && p.Provider == model.ProviderClaude && p.Event == "Notification" {
		msg := p.NotificationMessage
		nt := p.NotificationType
		if containsAny(strings.ToLower(nt+" "+msg), "rate limit", "limit reached", "usage limit", "quota") {
			c.observeQuota(model.ProviderClaude, claudeQuotaFromNotification(nt, msg, c.Now()))
		}
	}
}

// ExternalRunOverlap is an advisory for connected VS Code windows. No prompt
// content is sent and no scheduler state is changed by this notification.
type ExternalRunOverlap struct {
	Provider       string `json:"provider"`
	SessionID      string `json:"sessionId"`
	ActiveRunCount int    `json:"activeRunCount"`
	QueuePaused    bool   `json:"queuePaused"`
}

func (c *Conductor) notifyExternalOverlap(provider, sessionID string) {
	active, err := c.store.ListRunsByStates(
		model.StatePreparing, model.StateStarting, model.StateRunning,
		model.StateWaitingForProvider, model.StateVerifying, model.StateIntegrating,
		model.StateCancelRequested,
	)
	if err != nil {
		c.log.Errorf("check external run overlap: %v", err)
		return
	}
	if len(active) > 0 {
		c.emit("externalRunOverlap", ExternalRunOverlap{
			Provider: provider, SessionID: sessionID, ActiveRunCount: len(active), QueuePaused: c.PauseState().Paused,
		})
	}
}

// containsAny reports whether s contains any of subs. Needles are lower-case
// by convention, so every caller must lower-case s first — three call sites
// did not, and therefore never matched anything.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// claudeQuotaFromNotification derives low-confidence windows from
// notification text. Confidence never claimed as exact.
func claudeQuotaFromNotification(nt, msg string, now time.Time) []providers.QuotaWindow {
	low := strings.ToLower(nt + " " + msg)
	var ws []providers.QuotaWindow
	if containsAny(low, "weekly", "week") {
		ws = append(ws, providers.QuotaWindow{
			ID: "week", Name: "Weekly", Exhausted: true,
			Source: "provider_event", Confidence: "medium",
		})
	}
	if len(ws) == 0 {
		ws = append(ws, providers.QuotaWindow{
			ID: "5h", Name: "5-hour", Exhausted: true,
			Source: "provider_event", Confidence: "medium",
		})
	}
	return ws
}

// ---- Window Anchor ----

// markWindowAnchoredByWork records that a real run's first provider request
// opened the current window, so the synthetic Window Anchor never
// double-spends it. Stored in service_state, distinct from synthetic anchors.
func (c *Conductor) markWindowAnchoredByWork(provider string) {
	now := c.Now()
	for _, w := range c.guardian.Windows(provider) {
		if w.HasReset && !w.ResetsAt.After(now.Add(5*time.Minute)) {
			c.store.SetServiceState("anchor."+provider+"."+w.ResetsAt.UTC().Format(time.RFC3339), "real-work")
			return
		}
	}
}

// maybeAnchor fires one synthetic minimal request per quota window when the
// host is genuinely idle. Real work always anchors naturally instead.
func (c *Conductor) maybeAnchor(ctx context.Context, s Settings, now time.Time, busyExt map[string]*model.ProviderSession) {
	c.anchorMu.Lock()
	defer c.anchorMu.Unlock()

	// Priority rule: any real work waiting or active means no anchor.
	if c.hasRealWork(now) {
		return
	}
	if len(busyExt) > 0 {
		return
	}
	for _, provider := range []string{model.ProviderCodex, model.ProviderClaude} {
		enabled := s.AnchorCodexEnabled && provider == model.ProviderCodex
		if provider == model.ProviderClaude {
			enabled = s.AnchorClaudeEnabled
		}
		if !enabled {
			continue
		}
		c.tryAnchor(ctx, provider, s, now)
	}
}

// hasRealWork: active run, queued ready run, or quota-waiting run whose
// deadline has passed.
func (c *Conductor) hasRealWork(now time.Time) bool {
	active, err := c.store.ListRunsByStates(activeStates()...)
	if err == nil && len(active) > 0 {
		return true
	}
	queued, err := c.store.ListRunsByStates(model.StateQueued, model.StateWaitingForSlot, model.StateWaitingForExternal)
	if err == nil && len(queued) > 0 {
		return true
	}
	waiting, err := c.store.ListRunsByStates(model.StateWaitingForQuota)
	if err != nil {
		return false
	}
	for _, r := range waiting {
		if r.NextEligibleAt != nil && !r.NextEligibleAt.After(now) {
			return true
		}
	}
	return false
}

// tryAnchor claims and fires one anchor for the current window.
func (c *Conductor) tryAnchor(ctx context.Context, provider string, s Settings, now time.Time) {
	ws := c.guardian.Windows(provider)
	if len(ws) == 0 {
		return
	}
	// find the window that most recently reset (i.e., current window start)
	var windowReset time.Time
	found := false
	for _, w := range ws {
		if w.HasReset && !w.ResetsAt.After(now.Add(5*time.Minute)) {
			if !found || w.ResetsAt.After(windowReset) {
				windowReset = w.ResetsAt
				found = true
			}
		}
	}
	if !found {
		return
	}
	// weekly exhaustion suppresses five-hour anchors
	blocked, _, _, _ := c.quotaBlocked(provider, now)
	if blocked {
		return
	}
	windowKey := windowReset.UTC().Format(time.RFC3339)
	// a real run already anchored this window naturally
	if _, claimed, _ := c.store.GetServiceState("anchor." + provider + "." + windowKey); claimed {
		return
	}
	// Resolve the adapter BEFORE claiming: the claim is exactly-once, so
	// claiming first and then finding no adapter burned the window forever.
	adapter, okA := c.registry.Get(provider)
	if !okA {
		return
	}
	// exactly-once claim (unique provider+window_reset)
	ok, err := c.store.ClaimAnchor(provider, windowKey, newID("anchor"))
	if err != nil || !ok {
		return
	}
	modelID := s.AnchorModelCodex
	if provider == model.ProviderClaude {
		modelID = s.AnchorModelClaude
	}
	if modelID == "" || modelID == "auto" {
		modelID = c.pickAnchorModel(provider)
	}
	c.log.Infof("window anchor firing for %s (window reset %s, model %s)", provider, windowReset.Format(time.RFC3339), modelID)

	// The adapter callback keeps firing after the turn completes (Codex emits
	// account/rateLimits/updated freely), so completion is handled inline under
	// a sync.Once instead of by a second goroutine that closes the channel out
	// from under the still-live producer — which panicked the whole daemon with
	// "send on closed channel".
	var once sync.Once
	complete := func(sessionID string) {
		c.store.CompleteAnchor(provider, windowKey, sessionID)
		c.log.Infof("window anchor completed for %s", provider)
		// refresh quota right after anchoring
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if ws2, err := adapter.ProbeQuota(cctx); err == nil {
			c.observeQuota(provider, ws2)
		}
	}
	go func() {
		spec := providers.ExecSpec{
			RunID:      newID("anchor"),
			Provider:   provider,
			Model:      modelID,
			Effort:     "low",
			Permission: "plan", // anchor must not touch anything
			Cwd:        homeDirOr("/tmp"),
			Prompt:     "Reply exactly: OK",
			Title:      "[AC] Window Anchor",
		}
		adapter.Execute(ctx, spec, func(ev providers.Event) {
			if ev.Kind == providers.EvTurnCompleted {
				sessionID := ev.SessionID
				once.Do(func() { complete(sessionID) })
			}
		})
	}()
}

// pickAnchorModel chooses a lightweight model from the current catalog.
// Preference order favors clearly lightweight classes; never silent about
// the choice — it is logged and persisted in the anchor record.
func (c *Conductor) pickAnchorModel(provider string) string {
	adapter, ok := c.registry.Get(provider)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	caps := adapter.Capabilities(ctx)
	prefer := []string{"haiku", "luna", "mini", "flash", "cheap", "fast"}
	for _, want := range prefer {
		for _, m := range caps.Models {
			if m.Hidden {
				continue
			}
			lower := strings.ToLower(m.ID + " " + m.Name)
			if containsAny(lower, want) {
				return m.ID
			}
		}
	}
	if len(caps.Models) > 0 {
		return caps.Models[0].ID
	}
	return ""
}

func homeDirOr(fallback string) string {
	home := userHomeDir()
	if home == "" {
		return fallback
	}
	return home
}
