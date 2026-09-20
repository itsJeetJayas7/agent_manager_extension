package providers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"agent-conductor/internal/model"
)

// The headless path drives Claude Code through its documented programmatic
// interface — `claude -p --output-format stream-json` — instead of starting the
// interactive TUI in a PTY, sleeping 2500ms and writing a bracketed-paste
// escape sequence.
//
// The PTY approach broke on: slow startup from a cold cache, the trust dialog,
// first-run theme prompts, any TUI layout change, any prompt containing
// sequences the TUI intercepts, and terminal resize. It was also unobservable,
// because PTY output was explicitly not parsed, so the adapter was blind and
// depended entirely on hooks for state.
//
// Verified against claude 2.1.278; see docs/provider-capabilities.md.

// streamMessage is one NDJSON line of the stream-json output.
type streamMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// system/init
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	// assistant / user
	Message json.RawMessage `json:"message"`
	// result
	IsError        bool            `json:"is_error"`
	Result         string          `json:"result"`
	StopReason     string          `json:"stop_reason"`
	TerminalReason string          `json:"terminal_reason"`
	APIErrorStatus json.RawMessage `json:"api_error_status"`
	NumTurns       int             `json:"num_turns"`
	// rate_limit_event
	RateLimitInfo *claudeRateLimitInfo `json:"rate_limit_info"`
}

type claudeRateLimitInfo struct {
	Status         string                        `json:"status"` // allowed | rejected | ...
	ResetsAt       int64                         `json:"resetsAt"`
	RateLimitType  string                        `json:"rateLimitType"`
	UnifiedWindows map[string]claudeRateLimitWin `json:"unifiedWindows"`
}

type claudeRateLimitWin struct {
	Utilization float64 `json:"utilization"` // 0..1
	ResetsAt    int64   `json:"resetsAt"`
}

type assistantMessage struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"` // text | tool_use | thinking | tool_result
		Text string `json:"text"`
		Name string `json:"name"`
	} `json:"content"`
}

// claudeWindowNames maps the unified window keys to display names.
var claudeWindowNames = map[string]string{
	"five_hour": "5-hour",
	"seven_day": "Weekly",
}

// quotaFromRateLimitEvent converts a rate_limit_event into normalized windows.
//
// This is a real structured quota surface. The adapter previously reported
// that Claude had none and inferred windows from notification text.
func quotaFromRateLimitEvent(info *claudeRateLimitInfo) []QuotaWindow {
	if info == nil {
		return nil
	}
	blockedType := ""
	if !strings.EqualFold(info.Status, "allowed") && info.Status != "" {
		blockedType = info.RateLimitType
	}
	var out []QuotaWindow
	for key, w := range info.UnifiedWindows {
		q := QuotaWindow{
			ID:          key,
			Name:        claudeWindowNames[key],
			UsedPercent: w.Utilization * 100,
			HasPercent:  true,
			Source:      "provider_structured",
			Confidence:  "exact",
		}
		if q.Name == "" {
			q.Name = key
		}
		if w.ResetsAt > 0 {
			q.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
			q.HasReset = true
		}
		if q.UsedPercent >= 100 || (blockedType != "" && key == blockedType) {
			q.Exhausted = true
		}
		out = append(out, q)
	}
	// A rejection naming a window we did not see still has to be recorded.
	if blockedType != "" {
		found := false
		for _, q := range out {
			if q.ID == blockedType {
				found = true
			}
		}
		if !found {
			q := QuotaWindow{
				ID: blockedType, Name: claudeWindowNames[blockedType], Exhausted: true,
				Source: "provider_structured", Confidence: "exact",
			}
			if q.Name == "" {
				q.Name = blockedType
			}
			if info.ResetsAt > 0 {
				q.ResetsAt = time.Unix(info.ResetsAt, 0).UTC()
				q.HasReset = true
			}
			out = append(out, q)
		}
	}
	return out
}

// supportsStreamJSON reports whether the installed CLI offers the headless
// protocol. Probed once from --help; the PTY path is the fallback.
func (c *Claude) supportsStreamJSON(ctx context.Context) bool {
	c.mu.Lock()
	cached := c.streamOK
	c.mu.Unlock()
	if cached != nil {
		return *cached
	}
	ok := false
	if bin := c.bin(); bin != "" {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx2, bin, "--help").CombinedOutput()
		h := string(out)
		ok = strings.Contains(h, "--output-format") && strings.Contains(h, "stream-json") && strings.Contains(h, "--print")
	}
	c.mu.Lock()
	c.streamOK = &ok
	c.mu.Unlock()
	if !ok {
		c.log.Infof("claude: stream-json unavailable; falling back to the interactive PTY path")
	}
	return ok
}

// headlessArgs builds the command line for one turn.
func (c *Claude) headlessArgs(ctx context.Context, spec ExecSpec, sessionID string, settingsJSON string) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		// Required for the full message stream in print mode.
		"--verbose",
		// Nobody is here to answer a permission prompt: deny rather than hang.
		"--permission-prompts", "none",
	}
	if spec.SessionID != "" {
		args = append(args, "--resume", spec.SessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != "" && c.supportsEffort(ctx) {
		args = append(args, "--effort", spec.Effort)
	}
	if spec.Permission != "" {
		args = append(args, "--permission-mode", claudePermissionMode(spec.Permission))
	}
	if settingsJSON != "" {
		args = append(args, "--settings", settingsJSON)
	}
	return args
}

// executeHeadless runs one turn and translates the stream into adapter events.
func (c *Claude) executeHeadless(ctx context.Context, spec ExecSpec, sink Sink) error {
	bin := c.bin()
	settingsJSON, err := c.managedSettingsJSON()
	if err != nil {
		return err
	}
	sessionID := spec.SessionID
	if sessionID == "" {
		sessionID = spec.Env["AC_SESSION_ID"]
		if sessionID == "" {
			sessionID = newUUID()
		}
	}
	args := c.headlessArgs(ctx, spec, sessionID, settingsJSON)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.Cwd
	extraEnv := map[string]string{"AGENT_CONDUCTOR_RUN_ID": spec.RunID}
	if c.socketPath != "" {
		extraEnv["AC_SOCKET"] = c.socketPath
	}
	c.mu.Lock()
	apiMode := c.apiMode
	c.mu.Unlock()
	cmd.Env = claudeChildEnv(apiMode, extraEnv)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrTail := &tailBuffer{limit: 4096}
	cmd.Stderr = stderrTail

	if err := cmd.Start(); err != nil {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude start: " + err.Error()}})
		return nil
	}

	cr := &claudeRun{runID: spec.RunID, sessionID: sessionID, cancel: cancel, proc: cmd.Process, exited: make(chan struct{})}
	c.mu.Lock()
	c.running[spec.RunID] = cr
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		// Only remove OUR registration: a retry may have registered a new run
		// under the same id.
		if c.running[spec.RunID] == cr {
			delete(c.running, spec.RunID)
		}
		c.mu.Unlock()
	}()

	// The prompt goes on stdin so an arbitrarily long prompt never hits argv
	// limits and never passes through a terminal that could interpret it.
	go func() {
		io.WriteString(stdin, spec.Prompt)
		stdin.Close()
	}()

	// Terminate the child when the execution context is cancelled.
	go func() {
		select {
		case <-runCtx.Done():
			gracefulStop(cmd)
		case <-cr.exited:
		}
	}()

	sink(Event{Kind: EvSessionStarted, RunID: spec.RunID, SessionID: sessionID, Model: spec.Model, At: time.Now().UTC()})

	st := &headlessState{spec: spec, sink: sink, sessionID: sessionID}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg streamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // tolerate unknown lines rather than failing the run
		}
		c.handleStreamMessage(st, msg)
	}
	scanErr := scanner.Err()
	close(cr.exited)

	// Reap the child. Nothing reaped it before, so every managed Claude run
	// left a zombie until the daemon exited.
	waitErr := cmd.Wait()
	exitCode := cmd.ProcessState.ExitCode()

	if st.completed {
		return nil
	}
	// The stream ended without a result event.
	switch {
	case runCtx.Err() != nil:
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: st.sessionID, Status: TurnInterrupted,
			Progress: st.meaningful, At: time.Now().UTC()})
	case st.meaningful:
		sink(Event{Kind: EvProcessExited, RunID: spec.RunID, SessionID: st.sessionID,
			Error: &ProviderError{Class: model.ErrProcessCrash,
				Message: fmt.Sprintf("claude exited mid-turn (code %d): %s", exitCode, stderrTail.String())}})
	default:
		msg := fmt.Sprintf("claude exited before completing the turn (code %d)", exitCode)
		if tail := stderrTail.String(); tail != "" {
			msg += ": " + tail
		}
		if scanErr != nil {
			msg += " [stream: " + scanErr.Error() + "]"
		}
		if waitErr != nil && exitCode == 0 {
			msg += " [" + waitErr.Error() + "]"
		}
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: st.sessionID, Status: TurnFailed,
			Error: &ProviderError{Class: classifyClaudeText(msg), Message: msg}, At: time.Now().UTC()})
	}
	return nil
}

// headlessState accumulates per-turn observations.
type headlessState struct {
	spec       ExecSpec
	sink       Sink
	sessionID  string
	turnOpened bool
	meaningful bool
	completed  bool
	finalText  strings.Builder
}

func (c *Claude) handleStreamMessage(st *headlessState, msg streamMessage) {
	now := time.Now().UTC()
	switch msg.Type {
	case "system":
		if msg.Subtype == "init" {
			if msg.SessionID != "" {
				st.sessionID = msg.SessionID
				st.sink(Event{Kind: EvSessionStarted, RunID: st.spec.RunID, SessionID: msg.SessionID,
					Model: msg.Model, At: now})
			}
			if !st.turnOpened {
				st.turnOpened = true
				st.sink(Event{Kind: EvTurnStarted, RunID: st.spec.RunID, SessionID: st.sessionID, At: now})
			}
		}

	case "rate_limit_event":
		if q := quotaFromRateLimitEvent(msg.RateLimitInfo); len(q) > 0 {
			c.rememberQuota(q)
			st.sink(Event{Kind: EvQuotaUpdate, RunID: st.spec.RunID, SessionID: st.sessionID, Quota: q, At: now})
		}

	case "assistant":
		var am assistantMessage
		if json.Unmarshal(msg.Message, &am) != nil {
			return
		}
		for _, block := range am.Content {
			switch block.Type {
			case "text":
				if strings.TrimSpace(block.Text) != "" {
					st.finalText.Reset()
					st.finalText.WriteString(block.Text)
					st.meaningful = true
					st.sink(Event{Kind: EvMessageDelta, RunID: st.spec.RunID, SessionID: st.sessionID,
						Text: block.Text, Progress: true, Model: am.Model, At: now})
				}
			case "tool_use":
				// Real evidence that the agent began work. This is what drives
				// the resume-vs-restart decision after a quota wait; it used to
				// be derived from hooks that were never registered.
				st.meaningful = true
				st.sink(Event{Kind: EvMessageDelta, RunID: st.spec.RunID, SessionID: st.sessionID,
					Text: "tool:" + block.Name, Progress: true, At: now})
			}
		}

	case "user":
		// Tool results flowing back: the agent is working.
		st.meaningful = true

	case "result":
		st.completed = true
		final := msg.Result
		if strings.TrimSpace(final) == "" {
			final = st.finalText.String()
		}
		if msg.SessionID != "" {
			st.sessionID = msg.SessionID
		}
		switch {
		case msg.Subtype == "success" && !msg.IsError:
			st.sink(Event{Kind: EvTurnCompleted, RunID: st.spec.RunID, SessionID: st.sessionID,
				Status: TurnCompletedOK, FinalMsg: final, Progress: st.meaningful, At: now})
		case isInterruptedResult(msg):
			st.sink(Event{Kind: EvTurnCompleted, RunID: st.spec.RunID, SessionID: st.sessionID,
				Status: TurnInterrupted, FinalMsg: final, Progress: st.meaningful, At: now})
		default:
			detail := strings.TrimSpace(final)
			if detail == "" {
				detail = msg.Subtype
			}
			if len(msg.APIErrorStatus) > 0 && string(msg.APIErrorStatus) != "null" {
				detail += " " + string(msg.APIErrorStatus)
			}
			st.sink(Event{Kind: EvTurnCompleted, RunID: st.spec.RunID, SessionID: st.sessionID,
				Status: TurnFailed, FinalMsg: final, Progress: st.meaningful,
				Error: &ProviderError{Class: classifyClaudeText(detail + " " + msg.Subtype), Message: detail},
				At:    now})
		}
	}
}

func isInterruptedResult(msg streamMessage) bool {
	for _, s := range []string{msg.Subtype, msg.StopReason, msg.TerminalReason} {
		l := strings.ToLower(s)
		if strings.Contains(l, "interrupt") || strings.Contains(l, "cancel") || strings.Contains(l, "abort") {
			return true
		}
	}
	return false
}

// classifyClaudeText maps free-form CLI error text onto the normalized
// taxonomy. Only used when no structured error type is available.
func classifyClaudeText(s string) model.ErrorClass {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "rate limit") || strings.Contains(l, "usage limit") || strings.Contains(l, "rate_limit"):
		return model.ErrRateLimit
	case strings.Contains(l, "overloaded"):
		return model.ErrProviderOverloaded
	case strings.Contains(l, "authentication") || strings.Contains(l, "unauthorized") || strings.Contains(l, "login"):
		return model.ErrAuth
	case strings.Contains(l, "billing") || strings.Contains(l, "payment") || strings.Contains(l, "credit"):
		return model.ErrBilling
	case strings.Contains(l, "model") && strings.Contains(l, "not found"):
		return model.ErrModelUnavailable
	case strings.Contains(l, "context") && (strings.Contains(l, "limit") || strings.Contains(l, "too long")):
		return model.ErrContextLimit
	case strings.Contains(l, "permission") || strings.Contains(l, "denied"):
		return model.ErrPermission
	case strings.Contains(l, "network") || strings.Contains(l, "connection") || strings.Contains(l, "timeout"):
		return model.ErrNetwork
	case strings.Contains(l, "trust"):
		return model.ErrWorkspace
	default:
		return model.ErrUnknown
	}
}

// gracefulStop interrupts the child, escalating only if it ignores SIGINT.
func gracefulStop(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	cmd.Process.Signal(os.Interrupt)
	time.AfterFunc(5*time.Second, func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			cmd.Process.Kill()
		}
	})
}

// tailBuffer keeps only the last `limit` bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
