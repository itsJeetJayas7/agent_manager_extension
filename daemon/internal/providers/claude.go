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
	streamOK   *bool  // capability cache for the headless stream-json protocol
	partialOK  *bool  // capability cache for --include-partial-messages
	running    map[string]*claudeRun

	permissionPromptsOK *bool // capability cache for --permission-prompts
	streamInputOK       *bool // capability cache for --input-format stream-json

	// live holds the long-lived process of each session (claude_live.go);
	// liveStart serializes starting one.
	live      map[string]*liveSession
	liveStart sync.Mutex

	// deadline bounds one turn. Configurable because "24 hours, hard-coded,
	// timer never stopped" was neither.
	deadline time.Duration

	// lastQuota caches the newest rate_limit_event so ProbeQuota can answer
	// from real structured data between runs.
	quotaMu   sync.Mutex
	lastQuota []QuotaWindow
	quotaAt   time.Time
}

type claudeRun struct {
	runID     string
	sessionID string
	cancel    context.CancelFunc
	ptty      *os.File
	proc      *os.Process
	exited    chan struct{}
}

// NewClaude creates the adapter. socketPath tells the hook bridge (installed
// into managed sessions) where to forward lifecycle events.
func NewClaude(log *logging.Logger, hooks *HookBus, selfBin, socketPath string) *Claude {
	return &Claude{
		hooks: hooks, log: log, selfBin: selfBin, socketPath: socketPath,
		running: map[string]*claudeRun{}, live: map[string]*liveSession{}, deadline: defaultTurnDeadline,
	}
}

// defaultTurnDeadline bounds one turn unless configured otherwise.
const defaultTurnDeadline = 24 * time.Hour

// SetTurnDeadline configures the per-turn deadline (0 restores the default).
func (c *Claude) SetTurnDeadline(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		d = defaultTurnDeadline
	}
	c.deadline = d
}

func (c *Claude) turnDeadline() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deadline <= 0 {
		return defaultTurnDeadline
	}
	return c.deadline
}

// rememberQuota caches structured windows observed during a run.
func (c *Claude) rememberQuota(q []QuotaWindow) {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	c.lastQuota = q
	c.quotaAt = time.Now()
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
	caps.Models = c.probeModels(ctx)
	caps.Notes = append(caps.Notes, "Explicit model IDs pin a version; aliases may change. Availability depends on your account. Opus 5.5 needs CLI 2.1.280+; Sonnet 5.5 needs 2.1.284+.")
	if c.supportsStreamJSON(ctx) {
		caps.Notes = append(caps.Notes, "Using the headless stream-json protocol.")
	} else {
		caps.Notes = append(caps.Notes, "This build lacks --output-format stream-json; falling back to the interactive PTY path.")
	}
	// Quota comes from rate_limit_event frames emitted during runs.
	c.quotaMu.Lock()
	caps.RealTimeQuota = len(c.lastQuota) > 0
	c.quotaMu.Unlock()
	return caps
}

// Explicit versions are pinned; aliases are deliberately labeled as moving.
// Sources: code.claude.com/docs/en/model-config and platform.claude.com/docs/en/models/overview.
// Account access is checked by the provider on launch, not inferred from --help.
var claudeModels = []ModelInfo{
	{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-fable-5-1", Name: "Claude Fable 5.1", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-fable-5", Name: "Claude Fable 5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-opus-5", Name: "Claude Opus 5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-opus-4-8", Name: "Claude Opus 4.8", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-opus-4-7", Name: "Claude Opus 4.7", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "claude-opus-4-6", Name: "Claude Opus 4.6", Efforts: []string{"low", "medium", "high", "max"}},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Efforts: []string{"low", "medium", "high", "max"}},
	{ID: "claude-haiku-4-5-20251001", Name: "Claude Haiku 4.5", Efforts: []string{}},
	{ID: "opus", Name: "Opus — latest alias (version may change)", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "sonnet", Name: "Sonnet — latest alias (version may change)", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "fable", Name: "Fable — latest alias (version may change)", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{ID: "haiku", Name: "Haiku — latest alias (version may change)", Efforts: []string{}},
}

func (c *Claude) probeModels(ctx context.Context) []ModelInfo {
	models := make([]ModelInfo, len(claudeModels))
	for i, m := range claudeModels {
		models[i] = m
		models[i].Efforts = append([]string{}, m.Efforts...)
		if !c.supportsEffort(ctx) {
			models[i].Efforts = []string{}
		}
	}
	return models
}

// managedSettings returns the --settings JSON installing Agent Conductor's
// namespaced hooks for managed sessions only. The hook bridge learns the
// daemon socket from the inherited AC_SOCKET environment variable.
func (c *Claude) managedSettingsJSON() (string, error) {
	// --gate enables the completion gate for managed runs only. The global
	// ~/.claude/settings.json entry is awareness-only, so Claude Code merging
	// both no longer fires two block decisions per stop.
	gateCmd := fmt.Sprintf("%s hook claude --gate", shellEscape(c.selfBin))
	plainCmd := fmt.Sprintf("%s hook claude", shellEscape(c.selfBin))
	entry := func(cmd string, timeout int) []any {
		return []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": cmd, "timeout": timeout,
		}}}}
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"SessionStart":     entry(plainCmd, 10),
			"UserPromptSubmit": entry(plainCmd, 10),
			// Only Stop carries the gate.
			"Stop":         entry(gateCmd, 10),
			"StopFailure":  entry(plainCmd, 10),
			"Notification": entry(plainCmd, 10),
			"SessionEnd":   entry(plainCmd, 5),
			// Registered so "did the agent actually start work?" is answered
			// by evidence. The handler already branched on these events, but
			// nothing ever registered them, so MeaningfulProgress could only
			// ever come from a non-empty final message.
			"PreToolUse":  entry(plainCmd, 5),
			"PostToolUse": entry(plainCmd, 5),
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
// (stored in ~/.claude.json). Used to prepare the legacy interactive fallback.
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
// ~/.claude.json with a backup. This personal installation trusts queued folders.
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

// Execute runs one turn to completion.
//
// The headless stream-json protocol is the default; the interactive PTY path
// survives only as a fallback for builds that lack it.
func (c *Claude) Execute(ctx context.Context, spec ExecSpec, sink Sink) error {
	if c.bin() == "" {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude CLI not found"}})
		return nil
	}
	if c.supportsStreamJSON(ctx) {
		ctx, cancel := context.WithTimeout(ctx, c.turnDeadline())
		defer cancel()
		if c.supportsStreamInput(ctx) {
			return c.executeLive(ctx, spec, sink)
		}
		return c.executeHeadless(ctx, spec, sink)
	}
	// Legacy interactive runtimes still display their own folder dialog.
	// The user authorizes all managed working folders in this installation.
	if !c.IsWorkspaceTrusted(spec.Cwd) {
		if err := c.TrustWorkspace(spec.Cwd); err != nil {
			return fmt.Errorf("prepare workspace for legacy Claude: %w", err)
		}
	}
	return c.executePTY(ctx, spec, sink)
}

// executePTY drives the interactive TUI by pasting the prompt into a PTY.
// Fragile and unobservable; retained only for runtimes without stream-json.
func (c *Claude) executePTY(ctx context.Context, spec ExecSpec, sink Sink) error {
	bin := c.bin()
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
	if spec.Effort != "" {
		extraEnv["CLAUDE_CODE_EFFORT_LEVEL"] = spec.Effort
	}
	c.mu.Lock()
	apiMode := c.apiMode
	c.mu.Unlock()
	cmd.Env = claudeChildEnv(apiMode, extraEnv)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "pty start: " + err.Error()}})
		return nil
	}
	cr.ptty = ptmx
	cr.proc = cmd.Process
	sink(Event{Kind: EvProcessStarted, RunID: spec.RunID, ProcessID: cmd.Process.Pid, At: time.Now().UTC()})
	c.mu.Lock()
	c.running[spec.RunID] = cr
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.running, spec.RunID)
		c.mu.Unlock()
	}()
	defer ptmx.Close()
	// Reap the child: pty.Start starts the process and nothing waited on it,
	// so every managed run left a zombie until the daemon exited.
	waitDone := make(chan struct{})
	go func() {
		err := cmd.Wait()
		if err != nil {
			c.log.Infof("claude run %s exited: %v", spec.RunID, err)
		}
		close(waitDone)
	}()
	defer func() {
		gracefulStop(cmd, waitDone, ptmx)
		<-waitDone
	}()
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

	sink(Event{Kind: EvSessionStarted, RunID: spec.RunID, SessionID: sessionID, At: time.Now().UTC()})

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
	// Both branches of the old `if spec.Continue` were identical; a
	// continuation is the same paste into an already-resumed session.
	paste := "\x1b[200~" + spec.Prompt + "\x1b[201~\r"
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
	deadline := time.NewTimer(c.turnDeadline())
	defer deadline.Stop() // one leaked timer per run otherwise
	idleOnStop := time.NewTimer(0)
	defer idleOnStop.Stop()
	if !idleOnStop.Stop() {
		<-idleOnStop.C
	}
	for {
		var stopIdle <-chan time.Time
		if stopped {
			stopIdle = idleOnStop.C
		}
		select {
		case <-runCtx.Done():
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnInterrupted})
			return nil
		case <-deadline.C:
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: sessionID, Status: TurnFailed,
				Error: &ProviderError{Class: model.ErrNetwork, Message: "execution deadline exceeded"}})
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

// billingRedirectVars would send a managed run somewhere other than the user's
// Claude subscription. Only ANTHROPIC_API_KEY used to be stripped, so an
// ANTHROPIC_BASE_URL or a Bedrock/Vertex switch silently redirected billing.
var billingRedirectVars = map[string]bool{
	"ANTHROPIC_API_KEY":             true,
	"ANTHROPIC_AUTH_TOKEN":          true,
	"ANTHROPIC_BASE_URL":            true,
	"ANTHROPIC_MODEL":               true,
	"ANTHROPIC_SMALL_FAST_MODEL":    true,
	"ANTHROPIC_CUSTOM_HEADERS":      true,
	"CLAUDE_CODE_USE_BEDROCK":       true,
	"CLAUDE_CODE_USE_VERTEX":        true,
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH": true,
	"CLAUDE_CODE_SKIP_VERTEX_AUTH":  true,
}

// billingRedirectPrefixes covers the cloud credential families.
var billingRedirectPrefixes = []string{"AWS_", "GOOGLE_", "GCLOUD_", "AZURE_"}

// claudeChildEnv builds the child environment with subscription protection.
func claudeChildEnv(apiMode bool, extra map[string]string) []string {
	out := []string{}
	for _, kv := range os.Environ() {
		// os.Environ() entries always contain '='; the old code also compared
		// the whole entry against a bare name, which could never match.
		name, _, _ := strings.Cut(kv, "=")
		if _, overridden := extra[name]; overridden {
			continue
		}
		if !apiMode && isBillingRedirect(name) {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func isBillingRedirect(name string) bool {
	if billingRedirectVars[name] {
		return true
	}
	for _, p := range billingRedirectPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Cancel asks the owning execution loop to interrupt and reap its process,
// and ends a process kept between turns of the run.
func (c *Claude) Cancel(runID string) error {
	c.mu.Lock()
	cr := c.running[runID]
	c.mu.Unlock()
	ended := c.endLive(runID)
	if cr == nil {
		if ended {
			return nil
		}
		return fmt.Errorf("no active claude run %s", runID)
	}
	cr.cancel()
	return nil
}

// CanResumeSession: `claude --resume <session-id>` reattaches to a session,
// on both the headless and the PTY path.
func (c *Claude) CanResumeSession() bool { return true }

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

// ProbeQuota returns the newest structured windows observed from a
// rate_limit_event frame.
//
// Claude Code has no standalone quota command, but the stream-json output does
// carry exact utilisation and reset times per window. The adapter used to
// report that no structured surface existed at all and inferred windows from
// notification text.
func (c *Claude) ProbeQuota(context.Context) ([]QuotaWindow, error) {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	if len(c.lastQuota) == 0 {
		return nil, fmt.Errorf("no Claude quota observation yet; windows are learned from the first run")
	}
	if time.Since(c.quotaAt) > 12*time.Hour {
		return nil, fmt.Errorf("Claude quota observation is stale (%s old)", time.Since(c.quotaAt).Round(time.Hour))
	}
	out := make([]QuotaWindow, len(c.lastQuota))
	copy(out, c.lastQuota)
	return out, nil
}

// Close cancels all runs and ends every live process.
func (c *Claude) Close() error {
	c.mu.Lock()
	live := make([]*liveSession, 0, len(c.live))
	for _, cr := range c.running {
		cr.cancel()
	}
	for _, ls := range c.live {
		live = append(live, ls)
	}
	c.mu.Unlock()
	for _, ls := range live {
		ls.shutdown()
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
