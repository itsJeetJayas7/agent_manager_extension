package providers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"agent-conductor/internal/model"
)

// Script lets tests and mock mode drive deterministic behavior. Each case
// matches a substring of the prompt (or "*") and describes what the fake
// provider does.
type Script struct {
	Match       string           `json:"match"`
	FailBefore  bool             `json:"failBefore"` // provider error before meaningful start
	Class       model.ErrorClass `json:"class"`
	RateLimit   bool             `json:"rateLimit"`   // rate limit error
	PartialWork bool             `json:"partialWork"` // progress before rate limit
	Weekly      bool             `json:"weekly"`
	ResetAfter  time.Duration    `json:"resetAfter"`
	Duration    time.Duration    `json:"duration"`
	FinalMsg    string           `json:"finalMsg"`
	NoMarker    bool             `json:"noMarker"`
	Blocked     bool             `json:"blocked"`
	Interrupt   bool             `json:"interrupt"` // pretend user interrupt
	Crash       bool             `json:"crash"`
	Resumes     bool             `json:"resumes"` // auto-resumes natively after reset
}

// Fake is a deterministic in-process provider used by tests and dev mode.
type Fake struct {
	mu        sync.Mutex
	scripts   []Script
	clock     func() time.Time
	running   map[string]context.CancelFunc
	cancelled map[string]bool
	quota     []QuotaWindow
	name      string
}

// NewFake creates the fake adapter.
func NewFake() *Fake {
	return newFakeNamed(model.ProviderFake)
}

// NewFakeNamed creates a fake adapter that impersonates a provider name
// (tests drive claude/codex scheduling without real runtimes).
func NewFakeNamed(name string) *Fake { return newFakeNamed(name) }

func newFakeNamed(name string) *Fake {
	return &Fake{
		name:      name,
		clock:     time.Now,
		running:   map[string]context.CancelFunc{},
		cancelled: map[string]bool{},
	}
}

// SetClock overrides time (virtual clock for multi-hour tests).
func (f *Fake) SetClock(fn func() time.Time) { f.mu.Lock(); f.clock = fn; f.mu.Unlock() }

// SetScripts replaces the script table.
func (f *Fake) SetScripts(s []Script) { f.mu.Lock(); f.scripts = s; f.mu.Unlock() }

// SetQuota overrides probe results.
func (f *Fake) SetQuota(q []QuotaWindow) { f.mu.Lock(); f.quota = q; f.mu.Unlock() }

// Quota returns the currently configured quota observation.
func (f *Fake) Quota() []QuotaWindow { f.mu.Lock(); defer f.mu.Unlock(); return f.quota }

// Now returns the virtual clock.
func (f *Fake) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.clock() }

// IsCancelled reports whether the run was cancelled.
func (f *Fake) IsCancelled(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled[runID]
}

func (f *Fake) Name() string                   { return f.name }
func (f *Fake) Available(context.Context) bool { return true }

func (f *Fake) Capabilities(context.Context) Caps {
	return Caps{
		Installed: true,
		Version:   "fake-1.0",
		AuthState: "ok",
		Models: []ModelInfo{
			{ID: "fake-cheap", Name: "Fake Cheap", Efforts: []string{"low", "medium", "high"}},
			{ID: "fake-strong", Name: "Fake Strong", Efforts: []string{"medium", "high", "max"}},
		},
		RealTimeQuota: false,
	}
}

// Execute simulates one full turn synchronously, emitting events.
func (f *Fake) Execute(ctx context.Context, spec ExecSpec, sink Sink) error {
	f.mu.Lock()
	script := Script{Duration: 50 * time.Millisecond, FinalMsg: "done\n" + model.MarkerComplete}
	for _, s := range f.scripts {
		if s.Match == "*" || (s.Match != "" && strings.Contains(spec.Prompt, s.Match)) {
			script = s
			break
		}
	}
	// Phase-aware defaults: PLAN phase prompts must yield the stage marker;
	// staged BUILD continuation yields the result marker.
	if script.Match == "" && strings.Contains(spec.Prompt, "PLAN PHASE") {
		script.FinalMsg = "plan ready\n" + model.MarkerStagePlan
	}
	if spec.Continue && !script.RateLimit {
		// continuation after quota: succeed with marker
		script.RateLimit = false
		script.PartialWork = false
		script.FailBefore = false
	}
	runCtx, cancel := context.WithCancel(ctx)
	f.running[spec.RunID] = cancel
	f.cancelled[spec.RunID] = false
	clock := f.clock
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.running, spec.RunID)
		f.mu.Unlock()
	}()

	sessionID := spec.SessionID
	if sessionID == "" {
		sessionID = "fake-sess-" + spec.RunID
	}
	if script.Crash {
		time.Sleep(script.Duration)
		sink(Event{Kind: EvProcessExited, RunID: spec.RunID, SessionID: sessionID,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "fake process crash"}})
		return nil
	}
	sink(Event{Kind: EvSessionStarted, RunID: spec.RunID, SessionID: sessionID, Model: spec.Model, At: clock()})
	sink(Event{Kind: EvTurnStarted, RunID: spec.RunID, SessionID: sessionID, TurnID: "t1", At: clock()})

	if script.FailBefore {
		sleepCtx(runCtx, script.Duration)
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, TurnID: "t1", Status: TurnFailed,
			Error: f.classErr(script), At: clock()})
		return nil
	}

	if script.RateLimit && !script.PartialWork {
		// rate limited before meaningful start
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, TurnID: "t1", Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrRateLimit, Message: "rate limited (fake)"}, At: clock()})
		return nil
	}

	if script.PartialWork {
		sink(Event{Kind: EvMessageDelta, RunID: spec.RunID, SessionID: sessionID, Text: "partial work", Progress: true, At: clock()})
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, TurnID: "t1", Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrRateLimit, Message: "rate limited mid-run (fake)"}, At: clock()})
		return nil
	}

	// normal run: progress, then wait, checking cancellation
	sink(Event{Kind: EvMessageDelta, RunID: spec.RunID, SessionID: sessionID, Text: "working", Progress: true, At: clock()})
	if !sleepCtx(runCtx, script.Duration) || f.IsCancelled(spec.RunID) {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, TurnID: "t1", Status: TurnInterrupted, At: clock()})
		return nil
	}
	if script.Interrupt {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, TurnID: "t1", Status: TurnInterrupted, At: clock()})
		return nil
	}
	msg := script.FinalMsg
	if msg == "" {
		if script.NoMarker {
			msg = "finished without marker"
		} else if script.Blocked {
			msg = "cannot proceed\n<AC_RESULT>BLOCKED: fake blocker</AC_RESULT>"
		} else {
			msg = "done\n" + model.MarkerComplete
		}
	}
	sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, TurnID: "t1", Status: TurnCompletedOK,
		FinalMsg: msg, Progress: true, At: clock()})
	return nil
}

func (f *Fake) classErr(s Script) *ProviderError {
	if s.RateLimit {
		return &ProviderError{Class: model.ErrRateLimit, Message: "rate limited (fake)"}
	}
	return &ProviderError{Class: s.Class, Message: "fake error"}
}

// CanResumeSession mirrors the real adapters so restart-recovery is exercised.
func (f *Fake) CanResumeSession() bool { return true }

func (f *Fake) Cancel(runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled[runID] = true
	if c, ok := f.running[runID]; ok {
		c()
	}
	return nil
}

func (f *Fake) ProbeQuota(context.Context) ([]QuotaWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quota != nil {
		return f.quota, nil
	}
	return nil, fmt.Errorf("no quota data")
}

func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.running {
		c()
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = 10 * time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
