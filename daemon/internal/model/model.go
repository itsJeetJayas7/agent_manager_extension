// Package model defines Agent Conductor's entities, run state machine,
// lifecycle events, and deterministic prompt/marker handling.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Provider identifiers.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
	ProviderFake   = "fake"
)

// RunState is the explicit, persisted state of a managed run.
type RunState string

// Run states. Transitions are validated by CanTransition.
const (
	StateDraft               RunState = "DRAFT"
	StateQueued              RunState = "QUEUED"
	StateWaitingForSlot      RunState = "WAITING_FOR_SLOT"
	StateWaitingForExternal  RunState = "WAITING_FOR_EXTERNAL_AGENT"
	StatePreparing           RunState = "PREPARING"
	StateStarting            RunState = "STARTING"
	StateRunning             RunState = "RUNNING"
	StateWaitingForProvider  RunState = "WAITING_FOR_PROVIDER"
	StateWaitingForQuota     RunState = "WAITING_FOR_QUOTA"
	StateAutoResuming        RunState = "AUTO_RESUMING"
	StateVerifying           RunState = "VERIFYING"
	StateCompleted           RunState = "COMPLETED"
	StateBlocked             RunState = "BLOCKED"
	StateNeedsAttention      RunState = "NEEDS_ATTENTION"
	StateFailed              RunState = "FAILED"
	StateCancelRequested     RunState = "CANCEL_REQUESTED"
	StateCancelled           RunState = "CANCELLED"
	StateOrphaned            RunState = "ORPHANED"
	StateSkipped             RunState = "SKIPPED"
	StateIntegrating         RunState = "INTEGRATING"
	StateAwaitingIntegration RunState = "AWAITING_INTEGRATION"
)

// Terminal reports whether the state is terminal for scheduling purposes.
func (s RunState) Terminal() bool {
	switch s {
	case StateCompleted, StateBlocked, StateNeedsAttention, StateFailed,
		StateCancelled, StateSkipped, StateOrphaned:
		return true
	}
	return false
}

// Active reports whether the run currently occupies an execution slot.
func (s RunState) Active() bool {
	switch s {
	case StatePreparing, StateStarting, StateRunning, StateWaitingForProvider,
		StateVerifying, StateIntegrating, StateCancelRequested, StateAutoResuming:
		return true
	}
	return false
}

// transitions is the legal transition table. Anything not listed is illegal.
var transitions = map[RunState][]RunState{
	StateDraft:               {StateQueued, StateCancelled, StateSkipped},
	StateQueued:              {StateWaitingForSlot, StateWaitingForExternal, StatePreparing, StateCancelRequested, StateCancelled, StateSkipped, StateFailed, StateNeedsAttention},
	StateWaitingForSlot:      {StatePreparing, StateWaitingForExternal, StateQueued, StateCancelRequested, StateCancelled, StateFailed, StateNeedsAttention},
	StateWaitingForExternal:  {StatePreparing, StateWaitingForSlot, StateQueued, StateCancelRequested, StateCancelled, StateFailed, StateNeedsAttention},
	StatePreparing:           {StateStarting, StateFailed, StateNeedsAttention, StateCancelRequested, StateBlocked},
	StateStarting:            {StateRunning, StateFailed, StateNeedsAttention, StateCancelRequested, StateWaitingForQuota},
	StateRunning:             {StateWaitingForQuota, StateVerifying, StateCompleted, StateBlocked, StateNeedsAttention, StateFailed, StateCancelRequested, StateWaitingForProvider, StateIntegrating, StateOrphaned},
	StateWaitingForProvider:  {StateRunning, StateWaitingForQuota, StateFailed, StateNeedsAttention, StateCancelRequested, StateOrphaned},
	StateWaitingForQuota:     {StateAutoResuming, StateCancelRequested, StateCancelled, StateFailed, StateNeedsAttention},
	StateAutoResuming:        {StateRunning, StateWaitingForQuota, StateFailed, StateNeedsAttention, StateCancelRequested},
	StateVerifying:           {StateCompleted, StateRunning, StateNeedsAttention, StateFailed, StateCancelRequested, StateBlocked},
	StateIntegrating:         {StateCompleted, StateAwaitingIntegration, StateNeedsAttention, StateFailed, StateCancelRequested},
	StateAwaitingIntegration: {StateIntegrating, StateCompleted, StateNeedsAttention, StateFailed, StateCancelled},
	StateCancelRequested:     {StateCancelled, StateFailed, StateCompleted},
	StateCompleted:           {StateRunning}, // completed run may be continued manually
	StateBlocked:             {StateRunning, StateSkipped, StateCancelled, StateFailed, StateQueued},
	StateNeedsAttention:      {StateRunning, StateQueued, StateAutoResuming, StateSkipped, StateCancelled, StateFailed, StatePreparing},
	StateFailed:              {StateQueued, StateSkipped, StateCancelled, StatePreparing},
	StateOrphaned:            {StateAutoResuming, StateNeedsAttention, StateSkipped, StateCancelled, StateFailed},
}

// CanTransition validates a state change.
func CanTransition(from, to RunState) bool {
	if from == to {
		return true
	}
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// RunEventType names significant lifecycle events persisted to run_events.
type RunEventType string

const (
	EventQueued                 RunEventType = "RUN_QUEUED"
	EventProviderSessionCreated RunEventType = "PROVIDER_SESSION_CREATED"
	EventTurnStarted            RunEventType = "TURN_STARTED"
	EventAgentStopped           RunEventType = "AGENT_STOPPED"
	EventCompletionMarkerFound  RunEventType = "COMPLETION_MARKER_FOUND"
	EventRateLimitDetected      RunEventType = "RATE_LIMIT_DETECTED"
	EventWaitScheduled          RunEventType = "WAIT_SCHEDULED"
	EventAutoResumeStarted      RunEventType = "AUTO_RESUME_STARTED"
	EventAutoResumeSucceeded    RunEventType = "AUTO_RESUME_SUCCEEDED"
	EventWorktreeCreated        RunEventType = "WORKTREE_CREATED"
	EventWorktreeCommitted      RunEventType = "WORKTREE_COMMITTED"
	EventIntegrationStarted     RunEventType = "INTEGRATION_STARTED"
	EventIntegrationConflict    RunEventType = "INTEGRATION_CONFLICT"
	EventRunCompleted           RunEventType = "RUN_COMPLETED"
	EventRunFailed              RunEventType = "RUN_FAILED"
	EventStateChanged           RunEventType = "STATE_CHANGED"
	EventStageChanged           RunEventType = "STAGE_CHANGED"
	EventVerificationStarted    RunEventType = "VERIFICATION_STARTED"
	EventVerificationFinished   RunEventType = "VERIFICATION_FINISHED"
	EventContinuationSent       RunEventType = "CONTINUATION_SENT"
	EventCancelRequested        RunEventType = "CANCEL_REQUESTED"
	EventRunCancelled           RunEventType = "RUN_CANCELLED"
	EventExternalWait           RunEventType = "EXTERNAL_AGENT_WAIT"
	EventOrphaned               RunEventType = "RUN_ORPHANED"
)

// RunEvent is one timeline entry for a run.
type RunEvent struct {
	ID        int64        `json:"id"`
	RunID     string       `json:"runId"`
	Timestamp time.Time    `json:"timestamp"`
	Type      RunEventType `json:"type"`
	Data      string       `json:"data,omitempty"` // JSON blob, redacted
}

// Stage for staged intelligence.
type Stage string

const (
	StageNone   Stage = ""
	StagePlan   Stage = "PLAN"
	StageBuild  Stage = "BUILD"
	StageAudit  Stage = "AUDIT"
	StageVerify Stage = "VERIFY"
)

// RowMode is the execution mode of a batch row.
type RowMode string

const (
	RowLinear   RowMode = "LINEAR"
	RowParallel RowMode = "PARALLEL"
)

// BatchStatus enumerates batch lifecycle.
type BatchStatus string

const (
	BatchDraft     BatchStatus = "DRAFT"
	BatchQueued    BatchStatus = "QUEUED"
	BatchRunning   BatchStatus = "RUNNING"
	BatchAttention BatchStatus = "ATTENTION"
	BatchDone      BatchStatus = "COMPLETED"
	BatchCancelled BatchStatus = "CANCELLED"
)

// RowStatus enumerates row lifecycle.
type RowStatus string

const (
	RowPending RowStatus = "PENDING"
	RowRunning RowStatus = "RUNNING"
	RowDone    RowStatus = "COMPLETED"
	RowBlocked RowStatus = "BLOCKED"
)

// Run is a single managed agent run.
type Run struct {
	ID                 string     `json:"id"`
	BatchID            string     `json:"batchId"`
	RowID              string     `json:"rowId"`
	CardIndex          int        `json:"cardIndex"`
	Title              string     `json:"title"`
	Provider           string     `json:"provider"`
	Model              string     `json:"model"`
	Effort             string     `json:"effort"`
	Permission         string     `json:"permission"` // bypass | plan | default | acceptEdits
	Staged             bool       `json:"staged"`
	AttachSecrets      bool       `json:"attachSecrets"`
	State              RunState   `json:"state"`
	Stage              Stage      `json:"stage"`
	PromptCipherStore  string     `json:"-"`
	PromptPreview      string     `json:"promptPreview"` // redacted first line(s)
	PromptHash         string     `json:"promptHash"`
	SecretVersionID    string     `json:"secretVersionId,omitempty"`
	WorktreePath       string     `json:"worktreePath,omitempty"`
	Branch             string     `json:"branch,omitempty"`
	ProviderSessionID  string     `json:"providerSessionId,omitempty"` // claude session id / codex thread id
	ProviderTurnID     string     `json:"providerTurnId,omitempty"`
	ProviderSubSession string     `json:"providerSubSessionId,omitempty"` // codex session id when distinct
	ProviderTitle      string     `json:"providerTitle,omitempty"`
	ContinuationCount  int        `json:"continuationCount"`
	AuditCycles        int        `json:"auditCycles"`
	VerifyCycles       int        `json:"verifyCycles"`
	ErrorCode          string     `json:"errorCode,omitempty"`
	ErrorMsg           string     `json:"errorMsg,omitempty"`     // redacted
	FinalOutcome       string     `json:"finalOutcome,omitempty"` // COMPLETE | BLOCKED:...
	MeaningfulProgress bool       `json:"meaningfulProgress"`
	CreatedAt          time.Time  `json:"createdAt"`
	QueuedAt           time.Time  `json:"queuedAt"`
	StartedAt          time.Time  `json:"startedAt"`
	FinishedAt         time.Time  `json:"finishedAt"`
	NextEligibleAt     *time.Time `json:"nextEligibleAt,omitempty"`
	QueueWaitNS        int64      `json:"queueWaitNs"`
	ActiveNS           int64      `json:"activeNs"`
	QuotaWaitNS        int64      `json:"quotaWaitNs"`
	DispatchUUID       string     `json:"dispatchUuid,omitempty"`
	Attempt            int        `json:"attempt"`
	SortKey            int64      `json:"sortKey"`
	Cwd                string     `json:"cwd"`
}

// Batch is an ordered set of rows.
type Batch struct {
	ID                string      `json:"id"`
	Title             string      `json:"title"`
	Status            BatchStatus `json:"status"`
	BaseCwd           string      `json:"baseCwd"`
	BaseBranch        string      `json:"baseBranch"`
	BaseCommit        string      `json:"baseCommit"`
	IntegrationBranch string      `json:"integrationBranch,omitempty"`
	IntegrationWTPath string      `json:"integrationWorktreePath,omitempty"`
	CreatedAt         time.Time   `json:"createdAt"`
	StartedAt         time.Time   `json:"startedAt"`
	FinishedAt        time.Time   `json:"finishedAt"`
	Applied           bool        `json:"applied"`
	Implicit          bool        `json:"implicit"`
}

// Row is one LINEAR or PARALLEL row inside a batch.
type Row struct {
	ID       string    `json:"id"`
	BatchID  string    `json:"batchId"`
	Position int       `json:"position"`
	Mode     RowMode   `json:"mode"`
	Status   RowStatus `json:"status"`
}

// ProviderSession is a native provider conversation known to the daemon.
type ProviderSession struct {
	Provider   string    `json:"provider"`
	NativeID   string    `json:"nativeId"` // claude session id / codex thread id
	RunID      string    `json:"runId,omitempty"`
	Kind       string    `json:"kind"` // managed | external
	Cwd        string    `json:"cwd,omitempty"`
	Model      string    `json:"model,omitempty"`
	Title      string    `json:"title,omitempty"`
	LastEvent  string    `json:"lastEvent"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

// ExternalBusy reports whether provider has an active non-managed session.
type ExternalBusy struct {
	Provider string           `json:"provider"`
	Busy     bool             `json:"busy"`
	Session  *ProviderSession `json:"session,omitempty"`
}

// Result markers.
const (
	MarkerComplete  = "<AC_RESULT>COMPLETE</AC_RESULT>"
	MarkerBlocked   = "<AC_RESULT>BLOCKED:"
	MarkerStagePlan = "<AC_STAGE>PLAN_READY</AC_STAGE>"
)

// ParseResultMarker scans a final assistant message for the AC result marker.
// Returns outcome "COMPLETE", "BLOCKED" with reason, or "" when absent.
func ParseResultMarker(msg string) (outcome, reason string) {
	if strings.Contains(msg, MarkerComplete) {
		return "COMPLETE", ""
	}
	if i := strings.Index(msg, MarkerBlocked); i >= 0 {
		rest := msg[i+len(MarkerBlocked):]
		if j := strings.Index(rest, "</AC_RESULT>"); j >= 0 {
			return "BLOCKED", strings.TrimSpace(rest[:j])
		}
		return "BLOCKED", strings.TrimSpace(rest)
	}
	return "", ""
}

// HasStageMarker reports whether the plan-stage marker is present.
func HasStageMarker(msg string) bool {
	return strings.Contains(msg, MarkerStagePlan)
}

// ExecutionContract is the fixed wording prepended to managed prompts.
const ExecutionContract = `[AGENT CONDUCTOR EXECUTION CONTRACT]

Treat the user's task below as the authoritative task.

Think carefully before acting. Inspect the repository and existing implementation before making consequential decisions. Then proceed with the requested work end-to-end.

Do not stop merely after proposing a plan unless the user's task explicitly asks only for a plan.

When implementation is requested, implement it. Run the relevant tests, validation, build, lint, browser checks, or other verification that is reasonably available. Investigate failures, correct them, and continue until the requested task is actually complete or genuinely blocked.

Do not silently drop requirements. Preserve unrelated user work.

If you encounter a recoverable problem, solve it rather than handing the problem back immediately.

At the very end of your final response, on its own line, emit exactly one of:

<AC_RESULT>COMPLETE</AC_RESULT>

or

<AC_RESULT>BLOCKED: brief precise reason</AC_RESULT>

Do not emit COMPLETE until the requested implementation and reasonable verification are finished.`

// PlanStageInstruction is prepended for staged PLAN phase (read-only).
const PlanStageInstruction = `[AGENT CONDUCTOR — PLAN PHASE]

Produce a precise implementation plan for the user task below. You are in a read-only planning phase: do not modify any files in this phase.

Inspect the repository first. End your final response with exactly this marker on its own line:

<AC_STAGE>PLAN_READY</AC_STAGE>`

// BuildStageContinuation resumes the same session for BUILD phase.
const BuildStageContinuation = `Proceed now to implement the original user request using the plan you just produced.

Inspect current repository state before changing anything. Carry the task through implementation, testing, validation, and correction. Do not stop after restating the plan.

End with the standard Agent Conductor result marker: <AC_RESULT>COMPLETE</AC_RESULT> or <AC_RESULT>BLOCKED: reason</AC_RESULT>.`

// ContinueAfterQuota is the fixed continuation message after a quota reset.
const ContinueAfterQuota = `Continue from exactly where you stopped. Complete the existing task. Do not restart work that is already finished. Re-inspect the current repository state and continue through implementation and verification until the original request is complete.

Remember to end your final response with the required Agent Conductor result marker.`

// EffectivePrompt assembles the full prompt for a run phase.
// userPrompt is always verbatim. secretBlock is appended only when requested
// and non-empty. planPhase switches the framing for staged intelligence.
func EffectivePrompt(userPrompt string, attachSecrets bool, secretBlock string, planPhase bool) string {
	var b strings.Builder
	if planPhase {
		b.WriteString(PlanStageInstruction)
		b.WriteString("\n\n[USER TASK — VERBATIM]\n\n")
		b.WriteString(userPrompt)
		b.WriteString("\n[END USER TASK]\n")
		return b.String()
	}
	b.WriteString(ExecutionContract)
	b.WriteString("\n\n[USER TASK — VERBATIM]\n\n")
	b.WriteString(userPrompt)
	b.WriteString("\n[END USER TASK]\n")
	if attachSecrets && strings.TrimSpace(secretBlock) != "" {
		b.WriteString("\n[SHARED TESTING / SUDO BLOCK — USER SUPPLIED]\n\n")
		b.WriteString(secretBlock)
		b.WriteString("\n[END SHARED TESTING / SUDO BLOCK]\n")
	}
	return b.String()
}

// HashPrompt returns a hex sha256 of arbitrary prompt text (safe to store).
func HashPrompt(p string) string {
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:])
}

// PreviewRedacted builds a short preview from a user prompt without ever
// including secret block content. Only the user's own prompt text is used.
func PreviewRedacted(userPrompt string, max int) string {
	line := strings.TrimSpace(userPrompt)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if len(line) > max {
		line = line[:max] + "…"
	}
	return line
}

// DeterministicSessionTitle builds the provider-visible session/thread title.
func DeterministicSessionTitle(batchNum, rowNum, cardNum int, preview string) string {
	title := PreviewRedacted(preview, 48)
	if title == "" {
		title = "Managed run"
	}
	return fmt.Sprintf("[AC B%02d-R%02d-C%02d] %s", batchNum, rowNum, cardNum, title)
}

// ErrorClass is the normalized provider error taxonomy.
type ErrorClass string

const (
	ErrRateLimit          ErrorClass = "RATE_LIMIT"
	ErrAuth               ErrorClass = "AUTH"
	ErrBilling            ErrorClass = "BILLING"
	ErrModelUnavailable   ErrorClass = "MODEL_UNAVAILABLE"
	ErrNetwork            ErrorClass = "NETWORK"
	ErrProviderOverloaded ErrorClass = "PROVIDER_OVERLOADED"
	ErrPermission         ErrorClass = "PERMISSION"
	ErrWorkspace          ErrorClass = "WORKSPACE"
	ErrContextLimit       ErrorClass = "CONTEXT_LIMIT"
	ErrProcessCrash       ErrorClass = "PROCESS_CRASH"
	ErrUserInterrupt      ErrorClass = "USER_INTERRUPT"
	ErrUnknown            ErrorClass = "UNKNOWN"
)

// ClassifyClaudeStopFailure maps a Claude StopFailure error type to the
// normalized taxonomy (documented matcher values).
func ClassifyClaudeStopFailure(errType string) ErrorClass {
	switch strings.TrimSpace(errType) {
	case "rate_limit":
		return ErrRateLimit
	case "overloaded":
		return ErrProviderOverloaded
	case "authentication_failed", "oauth_org_not_allowed":
		return ErrAuth
	case "billing_error":
		return ErrBilling
	case "model_not_found":
		return ErrModelUnavailable
	case "invalid_request":
		return ErrUnknown
	case "server_error":
		return ErrProviderOverloaded
	case "max_output_tokens":
		return ErrContextLimit
	default:
		return ErrUnknown
	}
}

// ClassifyCodexError maps structured codex error info where possible.
func ClassifyCodexError(code, message string) ErrorClass {
	c := strings.ToUpper(strings.TrimSpace(code))
	m := strings.ToLower(message)
	switch {
	case strings.Contains(c, "RATE_LIMIT") || strings.Contains(m, "rate limit") || strings.Contains(m, "usage limit"):
		return ErrRateLimit
	case strings.Contains(c, "AUTH") || strings.Contains(m, "authentication") || strings.Contains(m, "login"):
		return ErrAuth
	case strings.Contains(m, "billing") || strings.Contains(m, "payment"):
		return ErrBilling
	case strings.Contains(c, "MODEL"):
		return ErrModelUnavailable
	case strings.Contains(c, "NETWORK") || strings.Contains(m, "network") || strings.Contains(m, "connection"):
		return ErrNetwork
	case strings.Contains(m, "overload") || strings.Contains(m, "capacity"):
		return ErrProviderOverloaded
	case strings.Contains(m, "context") || strings.Contains(m, "token limit"):
		return ErrContextLimit
	default:
		return ErrUnknown
	}
}
