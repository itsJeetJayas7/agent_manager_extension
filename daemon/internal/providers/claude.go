package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"

	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
)

// Claude runs managed Claude Code sessions in a daemon-owned PTY using the
// official interactive runtime, driven by lifecycle hooks for state.
// Subscription mode by default: ANTHROPIC_API_KEY is stripped from the child
// environment so runs bill against the user's Claude subscription.
type Claude struct {
	mu         sync.Mutex
	binPath    string
	hooks      *HookBus
	log        *logging.Logger
	selfBin    string // path to agent-conductor binary used by hook bridge
	socketPath string // daemon socket the hook bridge forwards to
	apiMode    bool   // advanced opt-in to keep ANTHROPIC_API_KEY
	effortOK   *bool  // capability cache for --effort
	running    map[string]*claudeRun
}

type claudeRun struct {
	runID     string
	sessionID string
	cancel    context.CancelFunc
	ptty      *os.File
	exited    chan struct{}
}

// NewClaude creates the adapter. socketPath tells the hook bridge (installed
// into managed sessions) where to forward lifecycle events.
func NewClaude(log *logging.Logger, hooks *HookBus, selfBin, socketPath string) *Claude {
	return &Claude{hooks: hooks, log: log, selfBin: selfBin, socketPath: socketPath, running: map[string]*claudeRun{}}
}

// SetAPIMode enables keeping ANTHROPIC_API_KEY (advanced setting).
func (c *Claude) SetAPIMode(on bool) { c.mu.Lock(); c.apiMode = on; c.mu.Unlock() }

// Name identifies the adapter.
func (c *Claude) Name() string { return model.ProviderClaude }

// Available probes the claude binary.
func (c *Claude) Available(ctx context.Context) bool {
	bin := c.bin()
	if bin == "" {
		return false
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, bin, "--version").CombinedOutput()
	if err != nil {
		return false
	}
	c.log.Infof("claude runtime detected: %s", strings.TrimSpace(string(out)))
	return true
}

func (c *Claude) bin() string {
	if c.binPath != "" {
		return c.binPath
	}
	return findBinary("claude")
}

// supportsEffort probes --effort support once.
func (c *Claude) supportsEffort(ctx context.Context) bool {
	c.mu.Lock()
	cached := c.effortOK
	c.mu.Unlock()
	if cached != nil {
		return *cached
	}
	bin := c.bin()
	ok := false
	if bin != "" {
		ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx2, bin, "--help").CombinedOutput()
		ok = strings.Contains(string(out), "--effort")
	}
	c.mu.Lock()
	c.effortOK = &ok
	c.mu.Unlock()
	return ok
}

// Capabilities probes runtime details.
func (c *Claude) Capabilities(ctx context.Context) Caps {
	caps := Caps{AuthState: "unknown", Notes: []string{}}
	if !c.Available(ctx) {
		caps.Notes = append(caps.Notes, "claude CLI not found")
		return caps
	}
	caps.Installed = true
	bin := c.bin()
	out, _ := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	caps.Version = strings.TrimSpace(string(out))
	if _, hasKey := os.LookupEnv("ANTHROPIC_API_KEY"); hasKey {
		c.mu.Lock()
		api := c.apiMode
		c.mu.Unlock()
		if api {
			caps.AuthState = "api_key_enabled"
			caps.Notes = append(caps.Notes, "ANTHROPIC_API_KEY present and API mode enabled: runs may bill to the API key instead of your subscription.")
		} else {
			caps.AuthState = "ok"
			caps.Notes = append(caps.Notes, "ANTHROPIC_API_KEY detected in this environment; Agent Conductor strips it from managed runs (subscription protection). Disable protection only if you intentionally want API billing.")
		}
	} else {
		caps.AuthState = "ok"
	}
	// Editable aliases; dynamic discovery is not exposed structurally today.
	caps.Models = []ModelInfo{
		{ID: "opus", Name: "Opus (planning-class)", Efforts: []string{"high", "max"}},
		{ID: "sonnet", Name: "Sonnet (execution-class)", Efforts: []string{"low", "medium", "high"}},
		{ID: "haiku", Name: "Haiku (lightweight)", Efforts: []string{"low", "medium"}},
	}
	caps.Notes = append(caps.Notes, "Model IDs are Claude Code aliases resolved by the runtime at launch.")
	return caps
}

// managedSettings returns the --settings JSON installing Agent Conductor's
// namespaced hooks for managed sessions only. The hook bridge learns the
// daemon socket from the inherited AC_SOCKET environment variable.
func (c *Claude) managedSettingsJSON() (string, error) {
	hookCmd := fmt.Sprintf("%s hook claude", shellEscape(c.selfBin))
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart":     []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 10}}}},
			"UserPromptSubmit": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 10}}}},
			"Stop":             []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 10}}}},
			"StopFailure":      []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 10}}}},
			"Notification":     []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 10}}}},
			"SessionEnd":       []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 5}}}},
		},
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func shellEscape(s string) string {
	if s == "" {
		return `''`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// claudePermissionMode maps conductor labels to CLI values.
func claudePermissionMode(p string) string {
	switch p {
	case "bypass":
		return "bypassPermissions"
	case "acceptEdits":
		return "acceptEdits"
	case "plan":
		return "plan"
	case "default", "":
		return "default"
	default:
		return p
	}
}

// IsWorkspaceTrusted reports whether the folder's trust dialog was accepted
// (stored in ~/.claude.json). Interactive managed runs require it; the
// daemon never silently accepts trust on the user's behalf.
func (c *Claude) IsWorkspaceTrusted(cwd string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}
	p, ok := cfg.Projects[abs]
	return ok && p.HasTrustDialogAccepted
}

// TrustWorkspace merges hasTrustDialogAccepted=true for one folder into
// ~/.claude.json with a backup. Explicit user action only.
func (c *Claude) TrustWorkspace(cwd string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude.json")
	var doc map[string]any
	data, _ := os.ReadFile(path)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("refusing to touch unreadable ~/.claude.json: %w", err)
		}
	} else {
		doc = map[string]any{}
	}
	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		doc["projects"] = projects
	}
	entry, _ := projects[abs].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["hasTrustDialogAccepted"] = true
	projects[abs] = entry
	if len(data) > 0 {
		bak := path + ".agent-conductor." + time.Now().UTC().Format("20060102T150405Z") + ".bak"
		if err := os.WriteFile(bak, data, 0o600); err != nil {
			return err
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}

// Execute launches the managed interactive session in a PTY and blocks until
// the turn reaches a terminal observation.
func (c *Claude) Execute(ctx context.Context, spec ExecSpec, sink Sink) error {
	bin := c.bin()
	if bin == "" {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude CLI not found"}})
		return nil
	}
	settingsJSON, err := c.managedSettingsJSON()
	if err != nil {
		return err
	}
	sessionID := spec.SessionID
	args := []string{}
	if sessionID == "" {
		sessionID = spec.Env["AC_SESSION_ID"]
		if sessionID == "" {
			sessionID = newUUID()
		}
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
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
	args = append(args, "--settings", settingsJSON)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cr := &claudeRun{runID: spec.RunID, sessionID: sessionID, cancel: cancel, exited: make(chan struct{})}
	c.mu.Lock()
	c.running[spec.RunID] = cr
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.running, spec.RunID)
		c.mu.Unlock()
	}()

	hookCh := c.hooks.Subscribe()
	defer c.hooks.Unsubscribe(hookCh)

	cmd := exec.Command(bin, args...)
	cmd.Dir = spec.Cwd
	extraEnv := map[string]string{
		"AGENT_CONDUCTOR_RUN_ID": spec.RunID,
		"TERM":                   "xterm-256color",
	}
	if c.socketPath != "" {
		extraEnv["AC_SOCKET"] = c.socketPath
	}
	cmd.Env = claudeChildEnv(c.apiMode, extraEnv)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "pty start: " + err.Error()}})
		return nil
	}
	cr.ptty = ptmx
	defer ptmx.Close()
	// TUIs refuse to render on 0x0 terminals: give the PTY a sane size.
	pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})
	var outTail = struct {
		mu   sync.Mutex
		tail []byte
	}{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				outTail.mu.Lock()
				outTail.tail = append(outTail.tail, buf[:n]...)
				if len(outTail.tail) > 4096 {
					outTail.tail = outTail.tail[len(outTail.tail)-4096:]
				}
				outTail.mu.Unlock()
				// TUI output is not parsed for state (fragile); drained to keep PTY healthy.
			}
			if err != nil {
				close(cr.exited)
				return
			}
		}
	}()
	tailText := func() string {
		outTail.mu.Lock()
		defer outTail.mu.Unlock()
		return string(outTail.tail)
	}

	sink(Event{Kind: EvSessionStarted, RunID: spec.RunID, SessionID: sessionID, Model: spec.Model, At: time.Now().UTC()})

	// Submit the prompt: wait for the TUI to come up, then bracketed-paste
	// the prompt and press enter. One shot; hooks report the actual turn.
	select {
	case <-time.After(2500 * time.Millisecond):
	case <-runCtx.Done():
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnInterrupted})
		return nil
	case <-cr.exited:
		// The TUI died during startup — surface its last output for diagnosis.
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude exited during startup; last output: " + truncateTail(tailText(), 400)}})
		return nil
	}
	paste := "\x1b[200~" + spec.Prompt + "\x1b[201~\r"
	if spec.Continue {
		paste = "\x1b[200~" + spec.Prompt + "\x1b[201~\r"
	}
	_, werr := ptmx.Write([]byte(paste))
	if werr != nil {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "pty write: " + werr.Error() + "; last output: " + truncateTail(tailText(), 400)}})
		return nil
	}

	var (
		sawTurn      bool
		meaningful   bool
		lastMsg      string
		stopped      bool
		nativeResume bool
	)
	deadline := time.NewTimer(24 * time.Hour)
	idleOnStop := time.NewTimer(0)
	if !idleOnStop.Stop() {
		<-idleOnStop.C
	}
	for {
		var stopIdle <-chan time.Time
		if stopped {
			stopIdle = idleOnStop.C
		}
		select {
		case <-ctx.Done():
			cmd.Process.Signal(os.Interrupt)
			time.Sleep(1500 * time.Millisecond)
			cmd.Process.Kill()
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnInterrupted})
			return nil
		case <-deadline.C:
			cmd.Process.Kill()
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnFailed,
				Error: &ProviderError{Class: model.ErrUnknown, Message: "24h execution deadline exceeded"}})
			return nil
		case <-cr.exited:
			// Process gone. If we never saw a Stop, the run died mid-turn.
			if !stopped {
				if sawTurn && meaningful {
					sink(Event{Kind: EvProcessExited, RunID: spec.RunID, SessionID: sessionID,
						Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude process exited mid-turn"}})
				} else {
					sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnFailed,
						Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude process exited before completing turn"}})
				}
			}
			return nil
		case <-stopIdle:
			// Stop observed and no native auto-resume followed: report stopped.
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnCompletedOK,
				FinalMsg: lastMsg, Progress: meaningful})
			return nil
		case hp := <-hookCh:
			if hp.RunID != spec.RunID {
				continue
			}
			switch hp.Event {
			case "SessionStart":
				if hp.SessionID != "" {
					cr.sessionID = hp.SessionID
					sink(Event{Kind: EvSessionStarted, RunID: spec.RunID, SessionID: hp.SessionID, Model: hp.Model, At: time.Now().UTC()})
				}
			case "UserPromptSubmit":
				sawTurn = true
				stopped = false
				if !idleOnStop.Stop() {
					select {
					case <-idleOnStop.C:
					default:
					}
				}
				sink(Event{Kind: EvTurnStarted, RunID: spec.RunID, SessionID: hp.SessionID, At: time.Now().UTC()})
			case "PostToolUse", "PostToolUseFailure", "PostToolBatch", "PreToolUse":
				meaningful = true
			case "Notification":
				nt := hp.NotificationType
				nm := strings.ToLower(hp.NotificationMessage)
				if strings.Contains(nt, "quota_auto_resume") || strings.Contains(nm, "auto-resume") || strings.Contains(nm, "auto resume") {
					nativeResume = true
					stopped = false
					if !idleOnStop.Stop() {
						select {
						case <-idleOnStop.C:
						default:
						}
					}
					sink(Event{Kind: EvMessageDelta, RunID: spec.RunID, SessionID: hp.SessionID,
						Text: "native quota auto-resume active", Progress: meaningful, At: time.Now().UTC()})
				}
			case "Stop":
				stopped = true
				lastMsg = hp.LastAssistantMessage
				meaningful = meaningful || strings.TrimSpace(hp.LastAssistantMessage) != ""
				sink(Event{Kind: EvMessageDelta, RunID: spec.RunID, SessionID: hp.SessionID,
					Text: "agent stopped", Progress: meaningful, At: time.Now().UTC()})
				// If native auto-resume already fired once, don't wait again.
				wait := 90 * time.Second
				if nativeResume {
					wait = 10 * time.Second
				}
				if !idleOnStop.Stop() {
					select {
					case <-idleOnStop.C:
					default:
					}
				}
				idleOnStop.Reset(wait)
			case "StopFailure":
				cls := model.ClassifyClaudeStopFailure(hp.ErrorType)
				if cls == model.ErrRateLimit {
					// Let native auto-resume run first; scheduler takes over if idle.
					stopped = false
					if !idleOnStop.Stop() {
						select {
						case <-idleOnStop.C:
						default:
						}
					}
					idleOnStop.Reset(90 * time.Second)
					sink(Event{Kind: EvMessageDelta, RunID: spec.RunID, SessionID: hp.SessionID,
						Text: "rate limited; observing native auto-resume", At: time.Now().UTC()})
				} else {
					sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: hp.SessionID, Status: TurnFailed,
						Error: &ProviderError{Class: cls, Message: hp.ErrorMessage}, At: time.Now().UTC()})
					cmd.Process.Signal(os.Interrupt)
					return nil
				}
			case "SessionEnd":
				if sawTurn {
					sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: hp.SessionID, Status: TurnCompletedOK,
						FinalMsg: lastMsg, Progress: meaningful})
				} else {
					sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: hp.SessionID, Status: TurnFailed,
						Error: &ProviderError{Class: model.ErrUnknown, Message: "session ended before turn"}})
				}
				return nil
			}
		}
	}
}

// claudeChildEnv builds the child environment with subscription protection.
func claudeChildEnv(apiMode bool, extra map[string]string) []string {
	out := []string{}
	for _, kv := range os.Environ() {
		if kv == "ANTHROPIC_API_KEY" || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			if apiMode {
				out = append(out, kv)
			}
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// Cancel sends a graceful interrupt to the PTY (Ctrl+C), escalating to SIGINT.
func (c *Claude) Cancel(runID string) error {
	c.mu.Lock()
	cr := c.running[runID]
	c.mu.Unlock()
	if cr == nil {
		return fmt.Errorf("no active claude run %s", runID)
	}
	cr.cancel()
	if cr.ptty != nil {
		cr.ptty.Write([]byte("\x03")) // Ctrl+C in the TUI = interrupt turn
	}
	go func() {
		time.Sleep(5 * time.Second)
		c.mu.Lock()
		defer c.mu.Unlock()
		if still := c.running[runID]; still != nil && still.ptty != nil {
			still.ptty.Close()
		}
	}()
	return nil
}

// ActiveRuns lists run ids currently executing.
func (c *Claude) ActiveRuns() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.running))
	for id := range c.running {
		out = append(out, id)
	}
	return out
}

// ProbeQuota: Claude has no structured quota read surface today; windows are
// populated from StopFailure/notification events (source provider_event).
func (c *Claude) ProbeQuota(context.Context) ([]QuotaWindow, error) {
	return nil, fmt.Errorf("claude exposes no structured quota read; quota tracked from lifecycle events")
}

// Close cancels all runs.
func (c *Claude) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cr := range c.running {
		cr.cancel()
		if cr.ptty != nil {
			cr.ptty.Write([]byte("\x03"))
		}
	}
	return nil
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := readRandom(b); err != nil {
		return fmt.Sprintf("ac-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// truncateTail strips ANSI escapes and trims for a compact error tail.
func truncateTail(s string, n int) string {
	s = stripANSI(s)
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			// skip CSI and OSC sequences
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				i = j + 1
				continue
			}
			i = j + 1
			continue
		}
		if s[i] == '\r' {
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
