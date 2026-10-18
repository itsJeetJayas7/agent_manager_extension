package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent-conductor/internal/model"
)

// PauseScope decides how far a pause reaches.
type PauseScope string

const (
	// PauseQueueOnly stops dispatch. Runs already executing finish normally.
	// This is the default and the safe, expected behaviour.
	PauseQueueOnly PauseScope = "QUEUE_ONLY"
	// PauseIncludeActive additionally asks in-flight runs to stop at a natural
	// boundary, leaving them resumable in the same native session. It must
	// never hard-kill a run mid-tool-use.
	PauseIncludeActive PauseScope = "INCLUDE_ACTIVE"
)

// Pause sources. A user pause outranks every system pause and is never cleared
// automatically except by its own timer.
const (
	PauseSourceUser     = "user"
	PauseSourceQuota    = "quota"
	PauseSourceExternal = "external-agent"
	PauseSourceSchedule = "schedule"
	PauseSourceAPI      = "api"
)

// pauseSettingKey is deliberately separate from conductor.settings so that an
// unrelated settings write cannot clobber the pause.
const pauseSettingKey = "conductor.pause"

// maxPauseDuration bounds a timer to something a human could plausibly mean.
const maxPauseDuration = 7 * 24 * time.Hour

// PauseState is the single answer to "why is nothing running?".
type PauseState struct {
	Paused bool       `json:"paused"`
	Scope  PauseScope `json:"scope"`
	Reason string     `json:"reason"`
	// PausedAt is when this pause began.
	PausedAt time.Time `json:"pausedAt"`
	// ResumeAt is absolute; nil means "until I resume".
	ResumeAt *time.Time `json:"resumeAt"`
	// ResumeAfter preserves the human input ("45m") for display only.
	ResumeAfter string `json:"resumeAfter"`
	Source      string `json:"source"`
	AutoResume  bool   `json:"autoResume"`
}

// Active reports whether dispatch is currently held.
func (p PauseState) Active() bool { return p.Paused }

// PauseRequest is the input to PauseQueue.
type PauseRequest struct {
	Scope PauseScope `json:"scope"`
	// DurationMs and ResumeAt are mutually exclusive; neither = indefinite.
	DurationMs int64      `json:"durationMs"`
	ResumeAt   *time.Time `json:"resumeAt"`
	Reason     string     `json:"reason"`
	// Source defaults to "user".
	Source string `json:"source"`
}

// PauseState returns a copy of the current pause.
func (c *Conductor) PauseState() PauseState {
	c.pauseMu.Lock()
	defer c.pauseMu.Unlock()
	return c.pause.clone()
}

func (p PauseState) clone() PauseState {
	out := p
	if p.ResumeAt != nil {
		t := *p.ResumeAt
		out.ResumeAt = &t
	}
	return out
}

// loadPause restores the persisted pause and honours a deadline that elapsed
// while the daemon was down.
func (c *Conductor) loadPause() {
	var p PauseState
	ok, err := c.store.GetSetting(pauseSettingKey, &p)
	if err != nil || !ok {
		c.pauseMu.Lock()
		// Migrate the old boolean, so an upgrade does not silently un-pause.
		if c.settings.Paused {
			c.pause = PauseState{
				Paused: true, Scope: PauseQueueOnly, Source: PauseSourceUser,
				PausedAt: c.Now(), Reason: "restored from a previous version",
			}
		}
		c.pauseMu.Unlock()
		return
	}
	if p.Scope == "" {
		p.Scope = PauseQueueOnly
	}
	// External activity no longer pauses dispatch. Retire persisted holds from
	// older versions without touching a user's explicit pause.
	legacyExternal := p.Source == PauseSourceExternal
	if legacyExternal {
		p = PauseState{Scope: PauseQueueOnly}
	}
	c.pauseMu.Lock()
	c.pause = p
	c.pauseMu.Unlock()
	if legacyExternal {
		c.persistPause(p)
	}
}

// honourElapsedPause resumes a pause whose deadline passed while the daemon
// was down. Called from Start, once the clock is final.
func (c *Conductor) honourElapsedPause() {
	p := c.PauseState()
	if !p.Paused || p.ResumeAt == nil || p.ResumeAt.After(c.Now()) {
		return
	}
	c.log.Infof("pause timer elapsed while the daemon was down (due %s); resuming now",
		p.ResumeAt.UTC().Format(time.RFC3339))
	if _, err := c.ResumeQueue(); err != nil {
		c.log.Errorf("resume after restart: %v", err)
	}
}

// persistPause writes the pause under its own settings key.
func (c *Conductor) persistPause(p PauseState) {
	if err := c.store.SetSetting(pauseSettingKey, p); err != nil {
		c.log.Errorf("persist pause: %v", err)
	}
	// Keep the legacy flag in step for anything still reading Settings.Paused.
	c.settingsMu.Lock()
	changed := c.settings.Paused != p.Paused
	c.settings.Paused = p.Paused
	s := c.settings
	c.settingsMu.Unlock()
	if changed {
		if err := c.store.SetSetting("conductor.settings", s); err != nil {
			c.log.Errorf("persist settings: %v", err)
		}
	}
}

// PauseQueue holds the queue. Pausing while already paused replaces the timer
// rather than erroring.
func (c *Conductor) PauseQueue(in PauseRequest) (PauseState, error) {
	now := c.Now()
	if in.DurationMs != 0 && in.ResumeAt != nil {
		return c.PauseState(), fmt.Errorf("durationMs and resumeAt are mutually exclusive")
	}
	var resumeAt *time.Time
	var after string
	switch {
	case in.DurationMs != 0:
		if in.DurationMs <= 0 {
			return c.PauseState(), fmt.Errorf("durationMs must be positive")
		}
		d := time.Duration(in.DurationMs) * time.Millisecond
		if d > maxPauseDuration {
			return c.PauseState(), fmt.Errorf("a pause cannot exceed %s", maxPauseDuration)
		}
		t := now.Add(d)
		resumeAt, after = &t, humanDuration(d)
	case in.ResumeAt != nil:
		t := in.ResumeAt.UTC()
		if !t.After(now) {
			return c.PauseState(), fmt.Errorf("resumeAt is in the past")
		}
		if t.Sub(now) > maxPauseDuration {
			return c.PauseState(), fmt.Errorf("a pause cannot exceed %s", maxPauseDuration)
		}
		resumeAt, after = &t, t.Local().Format("15:04")
	}
	scope := in.Scope
	if scope != PauseIncludeActive {
		scope = PauseQueueOnly
	}
	source := in.Source
	if source == "" {
		source = PauseSourceUser
	}

	c.pauseMu.Lock()
	prev := c.pause
	// A system pause must never overwrite a user pause.
	if prev.Paused && prev.Source == PauseSourceUser && source != PauseSourceUser {
		c.pauseMu.Unlock()
		return prev.clone(), nil
	}
	pausedAt := now
	if prev.Paused {
		pausedAt = prev.PausedAt
	}
	next := PauseState{
		Paused: true, Scope: scope, Reason: in.Reason, PausedAt: pausedAt,
		ResumeAt: resumeAt, ResumeAfter: after, Source: source, AutoResume: resumeAt != nil,
	}
	c.pause = next
	c.pauseMu.Unlock()

	c.persistPause(next)
	c.log.Infof("queue paused (%s)", pauseLogLine(next))
	if scope == PauseIncludeActive {
		c.interruptActiveForPause()
	}
	c.emit("pauseUpdated", next)
	c.Poke()
	return next.clone(), nil
}

// ResumeQueue clears the pause and any timer. Resuming while not paused is a
// no-op success.
func (c *Conductor) ResumeQueue() (PauseState, error) {
	c.pauseMu.Lock()
	was := c.pause
	c.pause = PauseState{Scope: PauseQueueOnly}
	c.pauseMu.Unlock()
	if !was.Paused {
		return PauseState{Scope: PauseQueueOnly}, nil
	}
	cleared := PauseState{Scope: PauseQueueOnly}
	c.persistPause(cleared)
	c.log.Infof("queue resumed (was %s)", pauseLogLine(was))
	c.resumePausedRuns()
	c.emit("pauseUpdated", cleared)
	c.Poke()
	return cleared, nil
}

// ExtendPause moves the deadline by delta. A negative delta shortens it; a
// pause with no timer gains one starting from now.
func (c *Conductor) ExtendPause(deltaMs int64) (PauseState, error) {
	if deltaMs == 0 {
		return c.PauseState(), fmt.Errorf("deltaMs must be non-zero")
	}
	now := c.Now()
	c.pauseMu.Lock()
	p := c.pause
	if !p.Paused {
		c.pauseMu.Unlock()
		return p.clone(), fmt.Errorf("the queue is not paused")
	}
	base := now
	if p.ResumeAt != nil && p.ResumeAt.After(now) {
		base = *p.ResumeAt
	}
	t := base.Add(time.Duration(deltaMs) * time.Millisecond)
	if !t.After(now) {
		// Shortened past the present: that is a resume.
		c.pauseMu.Unlock()
		return c.ResumeQueue()
	}
	if t.Sub(now) > maxPauseDuration {
		c.pauseMu.Unlock()
		return p.clone(), fmt.Errorf("a pause cannot exceed %s", maxPauseDuration)
	}
	p.ResumeAt = &t
	p.ResumeAfter = humanDuration(t.Sub(now))
	p.AutoResume = true
	c.pause = p
	c.pauseMu.Unlock()

	c.persistPause(p)
	c.emit("pauseUpdated", p)
	c.Poke()
	return p.clone(), nil
}

// ClearPauseTimer keeps the queue paused but drops the deadline.
func (c *Conductor) ClearPauseTimer() (PauseState, error) {
	c.pauseMu.Lock()
	p := c.pause
	if !p.Paused {
		c.pauseMu.Unlock()
		return p.clone(), fmt.Errorf("the queue is not paused")
	}
	p.ResumeAt = nil
	p.ResumeAfter = ""
	p.AutoResume = false
	c.pause = p
	c.pauseMu.Unlock()

	c.persistPause(p)
	c.emit("pauseUpdated", p)
	c.Poke()
	return p.clone(), nil
}

// SetSystemPause applies a non-user hold (quota, external agent, schedule).
// It never overrides a user pause and never clears one.
func (c *Conductor) SetSystemPause(source, reason string, resumeAt *time.Time) {
	c.pauseMu.Lock()
	p := c.pause
	if p.Paused && p.Source == PauseSourceUser {
		c.pauseMu.Unlock()
		return
	}
	if p.Paused && p.Source == source && sameInstant(p.ResumeAt, resumeAt) {
		c.pauseMu.Unlock()
		return // already reported; do not spam events
	}
	next := PauseState{
		Paused: true, Scope: PauseQueueOnly, Reason: reason, PausedAt: c.Now(),
		ResumeAt: resumeAt, Source: source, AutoResume: resumeAt != nil,
	}
	if resumeAt != nil {
		next.ResumeAfter = humanDuration(resumeAt.Sub(c.Now()))
	}
	c.pause = next
	c.pauseMu.Unlock()
	c.persistPause(next)
	c.log.Infof("queue held: %s", pauseLogLine(next))
	c.emit("pauseUpdated", next)
}

// ClearSystemPause lifts a system hold from the given source. A user pause is
// left untouched.
func (c *Conductor) ClearSystemPause(source string) {
	c.pauseMu.Lock()
	p := c.pause
	if !p.Paused || p.Source != source {
		c.pauseMu.Unlock()
		return
	}
	c.pause = PauseState{Scope: PauseQueueOnly}
	c.pauseMu.Unlock()
	c.persistPause(PauseState{Scope: PauseQueueOnly})
	c.resumePausedRuns()
	c.emit("pauseUpdated", PauseState{Scope: PauseQueueOnly})
	c.Poke()
}

// evaluatePauseTimer resumes when the deadline has arrived. Called on every
// scheduling pass, including while paused.
func (c *Conductor) evaluatePauseTimer(now time.Time) {
	c.pauseMu.Lock()
	p := c.pause
	due := p.Paused && p.ResumeAt != nil && !p.ResumeAt.After(now)
	c.pauseMu.Unlock()
	if !due {
		return
	}
	c.log.Infof("pause timer elapsed; resuming the queue")
	if _, err := c.ResumeQueue(); err != nil {
		c.log.Errorf("auto-resume: %v", err)
	}
}

// interruptActiveForPause asks in-flight runs to stop at a safe point. The
// adapter's interrupt is cooperative; nothing is killed mid-tool-use.
func (c *Conductor) interruptActiveForPause() {
	runs, err := c.store.ListRunsByStates(
		model.StateRunning, model.StateWaitingForProvider, model.StateAutoResuming,
	)
	if err != nil {
		return
	}
	for _, r := range runs {
		c.mu.Lock()
		_, live := c.cancels[r.ID]
		c.pauseHeld[r.ID] = true
		c.mu.Unlock()
		if !live {
			continue
		}
		if adapter, ok := c.registry.Get(r.Provider); ok {
			if err := adapter.Cancel(r.ID); err != nil {
				c.log.Errorf("pause interrupt %s: %v", r.ID, err)
			}
		}
		c.store.AddRunEvent(r.ID, model.EventPausedAtSafePoint, quoteJSON("queue paused; stopping at a safe point"))
	}
}

// resumePausedRuns continues every run parked by an INCLUDE_ACTIVE pause,
// reusing its native session so no work is thrown away.
func (c *Conductor) resumePausedRuns() {
	runs, err := c.store.ListRunsByStates(model.StatePaused)
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.ProviderSessionID == "" {
			// Never reached the provider: plain requeue is correct.
			if c.transition(r.ID, model.StateQueued, nil, nil) {
				c.emitRun(r.ID)
			}
			continue
		}
		if err := c.ContinueRun(r.ID, model.ContinueAfterPause); err != nil {
			c.log.Errorf("resume paused run %s: %v", r.ID, err)
			c.transition(r.ID, model.StateNeedsAttention, nil, func(x *model.Run) {
				x.ErrorCode = "RESUME"
				x.ErrorMsg = "could not resume after the pause: " + err.Error()
			})
			c.emitRun(r.ID)
		}
	}
}

// wasPauseHeld reports (and consumes) whether a run was interrupted by a pause.
func (c *Conductor) wasPauseHeld(runID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.pauseHeld[runID]
	delete(c.pauseHeld, runID)
	return held
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func pauseLogLine(p PauseState) string {
	if !p.Paused {
		return "not paused"
	}
	s := fmt.Sprintf("source=%s scope=%s", p.Source, p.Scope)
	if p.ResumeAt != nil {
		s += " until=" + p.ResumeAt.UTC().Format(time.RFC3339)
	} else {
		s += " until=manual"
	}
	if p.Reason != "" {
		s += " reason=" + p.Reason
	}
	return s
}

// humanDuration renders a duration the way the pause chips name it.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

// GatesDispatch reports whether this pause stops dispatch outright.
//
// Legacy external holds never gate dispatch and are discarded on upgrade.
func (p PauseState) GatesDispatch() bool {
	return p.Paused && p.Source != PauseSourceExternal
}

// updateSystemHolds keeps the pause record in step with conditions the user
// did not ask for, so the UI never has to say "nothing is running" without a
// reason. A user pause always wins and is never touched here.
func (c *Conductor) updateSystemHolds(s Settings, now time.Time) {
	c.ClearSystemPause(PauseSourceExternal)
	if p := c.PauseState(); p.Paused && p.Source == PauseSourceUser {
		return
	}
	// Quiet hours outrank the other system holds: they are a standing
	// instruction, not a transient condition.
	if c.applyQuietHours(s, now) {
		return
	}
	waiting, err := c.store.ListRunsByStates(queueWaitingStates()...)
	if err != nil {
		return
	}
	if len(waiting) == 0 {
		c.ClearSystemPause(PauseSourceQuota)
		return
	}

	// Quota: hold only when every provider that has waiting work is blocked.
	providersWaiting := map[string]bool{}
	for _, r := range waiting {
		providersWaiting[r.Provider] = true
	}
	allBlocked := len(providersWaiting) > 0
	var latest time.Time
	haveReset := false
	for p := range providersWaiting {
		blocked, ne, ht, _ := c.quotaBlocked(p, now)
		if !blocked {
			allBlocked = false
			break
		}
		if ht && (!haveReset || ne.After(latest)) {
			latest, haveReset = ne, true
		}
	}
	if allBlocked {
		var resumeAt *time.Time
		if haveReset {
			t := latest
			resumeAt = &t
		}
		reason := "every provider with queued work is out of quota"
		if haveReset {
			reason = fmt.Sprintf("quota resets at %s", latest.Local().Format("15:04"))
		}
		c.SetSystemPause(PauseSourceQuota, reason, resumeAt)
		return
	}
	c.ClearSystemPause(PauseSourceQuota)
}

// queueWaitingStates are runs that would dispatch if nothing held them.
func queueWaitingStates() []model.RunState {
	return []model.RunState{
		model.StateQueued, model.StateWaitingForSlot,
		model.StateWaitingForExternal, model.StateWaitingForQuota,
	}
}

// ---- quiet hours (§7.4) ----

// parseHHMM reads a local "HH:MM" clock time.
func parseHHMM(v string) (h, m int, ok bool) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// withinWorkingHours reports whether now falls inside [start, end), handling a
// window that wraps past midnight (e.g. 22:00 → 08:00 means "work overnight").
func withinWorkingHours(now time.Time, start, end string) (inside bool, nextChange time.Time, ok bool) {
	sh, sm, ok1 := parseHHMM(start)
	eh, em, ok2 := parseHHMM(end)
	if !ok1 || !ok2 {
		return true, time.Time{}, false
	}
	local := now.Local()
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	s := day.Add(time.Duration(sh)*time.Hour + time.Duration(sm)*time.Minute)
	e := day.Add(time.Duration(eh)*time.Hour + time.Duration(em)*time.Minute)
	if s.Equal(e) {
		return true, time.Time{}, false // a zero-length window means "no limit"
	}
	if s.Before(e) {
		switch {
		case local.Before(s):
			return false, s, true
		case local.Before(e):
			return true, e, true
		default:
			return false, s.AddDate(0, 0, 1), true
		}
	}
	// Wrapping window: working hours run from s to midnight and midnight to e.
	if local.Before(e) {
		return true, e, true
	}
	if local.Before(s) {
		return false, s, true
	}
	return true, e.AddDate(0, 0, 1), true
}

// applyQuietHours holds the queue outside the configured working window.
func (c *Conductor) applyQuietHours(s Settings, now time.Time) bool {
	if !s.QuietHoursEnabled {
		c.ClearSystemPause(PauseSourceSchedule)
		return false
	}
	inside, nextChange, ok := withinWorkingHours(now, s.QuietHoursStart, s.QuietHoursEnd)
	if !ok {
		c.ClearSystemPause(PauseSourceSchedule)
		return false
	}
	if inside {
		c.ClearSystemPause(PauseSourceSchedule)
		return false
	}
	var resumeAt *time.Time
	if !nextChange.IsZero() {
		t := nextChange.UTC()
		resumeAt = &t
	}
	reason := fmt.Sprintf("outside your working hours (%s–%s)", s.QuietHoursStart, s.QuietHoursEnd)
	c.SetSystemPause(PauseSourceSchedule, reason, resumeAt)
	return true
}
