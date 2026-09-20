// Package providers defines the adapter interface every provider runtime
// implements, plus shared event and quota types.
package providers

import (
	"context"
	"sync"
	"time"

	"agent-conductor/internal/model"
)

// EventKind enumerates adapter events consumed by the scheduler.
type EventKind string

const (
	EvSessionStarted EventKind = "session_started"
	EvTurnStarted    EventKind = "turn_started"
	EvMessageDelta   EventKind = "message_delta"
	EvTurnCompleted  EventKind = "turn_completed" // terminal for the turn
	EvProcessExited  EventKind = "process_exited"
	EvQuotaUpdate    EventKind = "quota_update"
	EvAccountUpdate  EventKind = "account_update"
)

// TurnStatus on EvTurnCompleted: completed | interrupted | failed.
type TurnStatus string

const (
	TurnCompletedOK TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnFailed      TurnStatus = "failed"
)

// ProviderError is a normalized provider failure.
type ProviderError struct {
	Class   model.ErrorClass `json:"class"`
	Code    string           `json:"code,omitempty"`
	Message string           `json:"message"` // already redacted upstream
}

func (e *ProviderError) Error() string { return string(e.Class) + ": " + e.Message }

// Event is one adapter observation.
type Event struct {
	Kind       EventKind
	RunID      string
	SessionID  string // native claude session / codex thread id
	SubSession string // codex session id when distinct
	TurnID     string
	Status     TurnStatus
	Text       string // final assistant message or delta
	FinalMsg   string // full final assistant message at turn end
	Error      *ProviderError
	Progress   bool // meaningful progress evidence
	Quota      []QuotaWindow
	Model      string
	At         time.Time
}

// QuotaWindow is a normalized provider quota window observation.
type QuotaWindow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	UsedPercent float64   `json:"usedPercent"`
	HasPercent  bool      `json:"hasPercent"`
	ResetsAt    time.Time `json:"resetsAt"`
	HasReset    bool      `json:"hasReset"`
	Exhausted   bool      `json:"exhausted"`
	Source      string    `json:"source"`     // provider_structured | provider_event | provider_text | user_config | inferred
	Confidence  string    `json:"confidence"` // exact | high | medium | experimental | unknown
}

// ExecSpec fully describes one provider execution (turn).
type ExecSpec struct {
	RunID      string
	Provider   string
	Model      string
	Effort     string
	Permission string // bypass | plan | default | acceptEdits
	Cwd        string
	Prompt     string // fully assembled effective prompt (may contain secrets)
	SessionID  string // non-empty => continue SAME native session
	Continue   bool   // true = continuation turn on SessionID
	Stage      model.Stage
	Title      string // deterministic native session/thread title
	Env        map[string]string
}

// Caps describes what an adapter/runtime supports.
type Caps struct {
	Installed     bool        `json:"installed"`
	Version       string      `json:"version"`
	Models        []ModelInfo `json:"models"`
	AuthState     string      `json:"authState"` // ok | api_key | none | unknown
	PlanLabel     string      `json:"planLabel"`
	Notes         []string    `json:"notes"`
	RealTimeQuota bool        `json:"realTimeQuota"`
}

// ModelInfo is one discoverable model.
type ModelInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Efforts []string `json:"efforts"`
	Hidden  bool     `json:"hidden,omitempty"`
}

// Sink receives events for one execution.
type Sink func(Event)

// Adapter is a provider runtime adapter.
type Adapter interface {
	Name() string
	// Available reports whether the runtime is installed.
	Available(ctx context.Context) bool
	// Capabilities probes the runtime (version, models, auth).
	Capabilities(ctx context.Context) Caps
	// Execute runs one turn to completion; events flow to sink. Blocking.
	Execute(ctx context.Context, spec ExecSpec, sink Sink) error
	// Cancel interrupts an in-flight run.
	Cancel(runID string) error
	// ProbeQuota returns the freshest quota observation available.
	ProbeQuota(ctx context.Context) ([]QuotaWindow, error)
	// CancelAll interrupts everything (daemon shutdown).
	Close() error
}

// SessionResumer is implemented by adapters that can reattach to a native
// session by id. Adapters that cannot must not implement it: the scheduler
// treats "cannot resume" as "needs a human", never as "start over".
type SessionResumer interface {
	CanResumeSession() bool
}

// CanResume reports whether this adapter can continue an existing session.
func CanResume(a Adapter) bool {
	r, ok := a.(SessionResumer)
	return ok && r.CanResumeSession()
}

// Registry maps provider names to adapters.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{adapters: map[string]Adapter{}} }

// Register adds an adapter.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.Name()] = a
}

// Get returns the adapter for a provider name.
func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// All returns registered names.
func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		out = append(out, a)
	}
	return out
}

// HookBus delivers provider hook events (from the hook bridge subcommand)
// to whichever component subscribed. Claude adapter subscribes to correlate
// hook observations with managed PTY processes.
type HookBus struct {
	mu   sync.Mutex
	subs []chan HookPayload
}

// HookPayload is the JSON the hook bridge forwards.
type HookPayload struct {
	Provider             string         `json:"provider"`
	Event                string         `json:"event"` // SessionStart, UserPromptSubmit, Stop, ...
	SessionID            string         `json:"sessionId"`
	ThreadID             string         `json:"threadId,omitempty"`
	TurnID               string         `json:"turnId,omitempty"`
	Cwd                  string         `json:"cwd,omitempty"`
	Model                string         `json:"model,omitempty"`
	RunID                string         `json:"runId,omitempty"` // present only for managed runs
	StopHookActive       bool           `json:"stopHookActive,omitempty"`
	LastAssistantMessage string         `json:"lastAssistantMessage,omitempty"`
	ErrorType            string         `json:"errorType,omitempty"` // StopFailure error type
	ErrorMessage         string         `json:"errorMessage,omitempty"`
	NotificationType     string         `json:"notificationType,omitempty"`
	NotificationMessage  string         `json:"notificationMessage,omitempty"`
	Raw                  map[string]any `json:"raw,omitempty"`
}

// NewHookBus creates the bus.
func NewHookBus() *HookBus { return &HookBus{} }

// Subscribe registers a listener; returns the channel.
func (b *HookBus) Subscribe() chan HookPayload {
	ch := make(chan HookPayload, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, ch)
	return ch
}

// Unsubscribe removes a listener.
func (b *HookBus) Unsubscribe(ch chan HookPayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, s := range b.subs {
		if s == ch {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

// Publish fans out non-blockingly.
func (b *HookBus) Publish(p HookPayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- p:
		default: // never stall a hook on a slow consumer
		}
	}
}
