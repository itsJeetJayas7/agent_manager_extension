package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-conductor/internal/model"
)

// Live sessions make a managed run viewable in the official Claude Code
// VS Code extension while it executes.
//
// The extension never attaches to a process it did not start: opening a
// session reads its transcript once and then spawns `claude --resume` of its
// own (after an "Open here anyway" prompt if another process holds it). The
// only supported seam is the claudeCode.claudeProcessWrapper setting, which
// replaces that spawn. So the daemon stays the one owner of a long-lived Claude
// process speaking the Agent SDK stream-json protocol (initialize, then user
// messages on stdin), and Agent Conductor's wrapper relays the extension's
// stdin/stdout to that process instead of starting a second one. Continuation
// turns of the same run are written into the same process, so an attached
// conversation keeps updating across them.
//
// Verified against Claude Code 2.1.288; see docs/claude-native-live-view.md.

// conductorEntrypoint labels managed transcripts. In print mode the CLI tags
// every transcript entry "sdk-cli" unless CLAUDE_CODE_ENTRYPOINT says
// otherwise, and the VS Code extension deliberately leaves sdk-cli/sdk-ts/
// sdk-py sessions out of the conversations it can open, so a managed run
// opened by id became a fresh, empty conversation. A distinct label keeps the
// session openable without claiming to be the extension.
const conductorEntrypoint = "agent-conductor"

// liveParkTimeout is how long an idle process with no attached viewer is kept
// for the next turn of the same run before it is ended.
var liveParkTimeout = 2 * time.Minute

// liveInitTimeout bounds the wait for the CLI's initialize response.
var liveInitTimeout = 45 * time.Second

// liveBacklogLimit bounds the stdout lines retained for attach replay.
const liveBacklogLimit = 4000

// LiveStatus is the observable state of a run's live process.
type LiveStatus struct {
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Viewers   int    `json:"viewers"`  // currently attached native clients
	Attaches  int    `json:"attaches"` // attachments since the process started
	Busy      string `json:"busy"`     // conductor | native | ""
}

// LiveViewer is implemented by adapters whose runs can be shown live in a
// provider's own UI.
type LiveViewer interface {
	LiveStatus(runID string) (LiveStatus, bool)
}

type liveSession struct {
	c         *Claude
	sessionID string
	cwd       string
	cmd       *exec.Cmd
	pid       int
	stderr    *tailBuffer

	writeMu sync.Mutex
	stdin   io.WriteCloser

	initDone chan struct{}
	initResp json.RawMessage
	exited   chan struct{}

	mu            sync.Mutex
	runID         string // run whose turns this process serves
	turn          *liveTurn
	nativeBusy    bool
	pendingNative int // native user messages not yet echoed by the CLI
	idle          chan struct{}
	clients       map[*liveClient]struct{}
	latest        *liveClient
	clientReqs    map[string]clientRequest
	reqSeq        int
	attaches      int
	backlog       []backlogLine
	closing       bool
	parkTimer     *time.Timer
	frozen        bool
	freezeTimer   *time.Timer
}

type clientRequest struct {
	client *liveClient
	id     json.RawMessage
}

type backlogLine struct {
	line []byte
	uuid string
}

type liveTurn struct {
	uuid        string
	st          *headlessState
	done        chan struct{}
	started     bool   // the CLI echoed this turn's prompt
	interrupted string // "" | "daemon" | "native"
	reported    bool
}

type liveClient struct {
	mu  sync.Mutex
	w   func([]byte) error
	end func()
}

func (cl *liveClient) send(line []byte) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.w != nil && cl.w(line) != nil {
		cl.w = nil
		cl.end()
	}
}

// supportsStreamInput reports whether the CLI accepts SDK messages on stdin.
// Without it the adapter falls back to one prompt per print-mode process,
// which cannot be attached to.
func (c *Claude) supportsStreamInput(ctx context.Context) bool {
	c.mu.Lock()
	cached := c.streamInputOK
	c.mu.Unlock()
	if cached != nil {
		return *cached
	}
	bin := c.bin()
	if bin == "" {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probe, bin, "--help").Output()
	if err != nil {
		return false // a failed probe is not evidence; retry next turn
	}
	fields := strings.Fields(string(out))
	ok := slices.Contains(fields, "--input-format") && slices.Contains(fields, "--replay-user-messages")
	c.mu.Lock()
	c.streamInputOK = &ok
	c.mu.Unlock()
	return ok
}

// liveArgs builds the command line of a long-lived process. Prompts arrive on
// stdin as user messages, never in argv.
func (c *Claude) liveArgs(ctx context.Context, spec ExecSpec, sessionID, settingsJSON string, resume bool) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		// An attached Claude Code tab learns that its own messages were
		// accepted from these echoes, and every viewer sees Conductor's prompts.
		"--replay-user-messages",
	}
	c.mu.Lock()
	partial := c.partialOK != nil && *c.partialOK
	c.mu.Unlock()
	if partial {
		args = append(args, "--include-partial-messages")
	}
	if c.supportsPermissionPrompts(ctx) {
		args = append(args, "--permission-prompts", "none")
	}
	if resume {
		args = append(args, "--resume", sessionID)
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

// liveFor returns the running process for a session, if any.
func (c *Claude) liveFor(sessionID string) *liveSession {
	if sessionID == "" {
		return nil
	}
	c.mu.Lock()
	ls := c.live[sessionID]
	c.mu.Unlock()
	if ls == nil {
		return nil
	}
	ls.mu.Lock()
	closing := ls.closing
	ls.mu.Unlock()
	select {
	case <-ls.exited:
		return nil
	default:
	}
	if closing {
		return nil
	}
	return ls
}

// obtainLive returns the session's live process, starting one if needed. Starts
// are serialized so a turn and an attaching viewer can never both spawn one.
func (c *Claude) obtainLive(ctx context.Context, spec ExecSpec) (*liveSession, error) {
	c.liveStart.Lock()
	defer c.liveStart.Unlock()
	if ls := c.liveFor(spec.SessionID); ls != nil {
		return ls, nil
	}
	return c.startLive(ctx, spec)
}

func (c *Claude) startLive(ctx context.Context, spec ExecSpec) (*liveSession, error) {
	bin := c.bin()
	if bin == "" {
		return nil, fmt.Errorf("claude CLI not found")
	}
	settingsJSON, err := c.managedSettingsJSON()
	if err != nil {
		return nil, err
	}
	resume := spec.SessionID != ""
	sessionID := spec.SessionID
	if sessionID == "" {
		sessionID = spec.Env["AC_SESSION_ID"]
		if sessionID == "" {
			sessionID = newUUID()
		}
	}
	cmd := exec.Command(bin, c.liveArgs(ctx, spec, sessionID, settingsJSON, resume)...)
	cmd.Dir = spec.Cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	extra := map[string]string{"AGENT_CONDUCTOR_RUN_ID": spec.RunID, "CLAUDE_CODE_ENTRYPOINT": conductorEntrypoint}
	if spec.Effort != "" {
		extra["CLAUDE_CODE_EFFORT_LEVEL"] = spec.Effort
	}
	if c.socketPath != "" {
		extra["AC_SOCKET"] = c.socketPath
	}
	c.mu.Lock()
	apiMode := c.apiMode
	c.mu.Unlock()
	cmd.Env = claudeChildEnv(apiMode, extra)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	ls := &liveSession{
		c: c, sessionID: sessionID, cwd: spec.Cwd, cmd: cmd, stdin: stdin,
		stderr: &tailBuffer{limit: 4096}, runID: spec.RunID,
		initDone: make(chan struct{}), exited: make(chan struct{}),
		clients: map[*liveClient]struct{}{}, clientReqs: map[string]clientRequest{},
	}
	cmd.Stderr = ls.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude start: %w", err)
	}
	ls.pid = cmd.Process.Pid
	c.mu.Lock()
	c.live[sessionID] = ls
	c.mu.Unlock()
	go ls.readLoop(stdout)
	ls.write(map[string]any{"type": "control_request", "request_id": "ac-init",
		"request": map[string]any{"subtype": "initialize"}})
	c.log.Infof("claude live session %s started (pid %d, run %s, resume=%v)", sessionID, ls.pid, spec.RunID, resume)
	return ls, nil
}

func (ls *liveSession) write(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return ls.writeLine(b)
}

func (ls *liveSession) writeLine(b []byte) error {
	ls.writeMu.Lock()
	defer ls.writeMu.Unlock()
	if _, err := ls.stdin.Write(append(append([]byte(nil), b...), '\n')); err != nil {
		return err
	}
	return nil
}

// envelope is the routing view of one protocol line.
type envelope struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	UUID      string          `json:"uuid"`
	IsReplay  bool            `json:"isReplay"`
	RequestID json.RawMessage `json:"request_id"`
	Request   *struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
	Response *struct {
		Subtype   string          `json:"subtype"`
		RequestID json.RawMessage `json:"request_id"`
		Response  json.RawMessage `json:"response"`
	} `json:"response"`
}

func rawString(r json.RawMessage) string {
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return string(r)
}

func (ls *liveSession) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		line := append([]byte(nil), raw...)
		var env envelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case "control_response":
			ls.onControlResponse(line, env)
			continue
		case "control_request":
			ls.onProcessRequest(line, env)
			continue
		case "control_cancel_request":
			ls.toLatest(line)
			continue
		}
		var msg streamMessage
		_ = json.Unmarshal(line, &msg)

		ls.mu.Lock()
		ls.remember(line, env.UUID)
		turn := ls.turn
		feed, finish := false, false
		echo := env.Type == "user" && env.IsReplay
		if echo && turn != nil && env.UUID == turn.uuid {
			turn.started = true
		} else if echo {
			if ls.pendingNative > 0 {
				ls.pendingNative--
			}
			if turn == nil || !turn.started {
				ls.nativeBusy = true
			}
		} else if turn == nil && env.Type == "system" && env.Subtype == "init" {
			// Claude starts turns of its own too, e.g. when a background task
			// finishes. A Conductor prompt waits for those like any other.
			ls.nativeBusy = true
		}
		switch {
		case turn != nil && turn.started:
			feed = true
			finish = env.Type == "result"
		case turn != nil && env.Type != "result":
			// Our prompt is written but not yet echoed. Its system/init
			// belongs to it; nothing native can be ahead of it.
			feed = !ls.nativeBusy
		}
		nativeDone := env.Type == "result" && !finish
		if nativeDone {
			ls.nativeBusy = false
			ls.signalIdleLocked()
		}
		clients := ls.clientList()
		ls.mu.Unlock()
		if nativeDone {
			ls.park()
		}

		for _, cl := range clients {
			cl.send(line)
		}
		if feed {
			ls.c.handleStreamMessage(turn.st, msg)
		}
		if finish {
			ls.finishTurn(turn)
		}
	}
	err := ls.cmd.Wait()
	_ = err
	ls.c.mu.Lock()
	if ls.c.live[ls.sessionID] == ls {
		delete(ls.c.live, ls.sessionID)
	}
	ls.c.mu.Unlock()
	ls.mu.Lock()
	ls.closing = true
	if ls.parkTimer != nil {
		ls.parkTimer.Stop()
	}
	if ls.freezeTimer != nil {
		ls.freezeTimer.Stop() // never signal a pid that may be reused
		ls.freezeTimer = nil
	}
	ls.frozen = false
	clients := ls.clientList()
	ls.clients = map[*liveClient]struct{}{}
	ls.signalIdleLocked()
	ls.mu.Unlock()
	close(ls.exited)
	select {
	case <-ls.initDone:
	default:
		close(ls.initDone)
	}
	for _, cl := range clients {
		cl.mu.Lock()
		cl.w = nil
		cl.mu.Unlock()
		cl.end()
	}
	ls.c.log.Infof("claude live session %s exited (pid %d)", ls.sessionID, ls.pid)
}

func (ls *liveSession) clientList() []*liveClient {
	out := make([]*liveClient, 0, len(ls.clients))
	for cl := range ls.clients {
		out = append(out, cl)
	}
	return out
}

// remember retains recent output for viewers that attach mid-turn.
func (ls *liveSession) remember(line []byte, uuid string) {
	ls.backlog = append(ls.backlog, backlogLine{line: line, uuid: uuid})
	if n := len(ls.backlog); n > liveBacklogLimit {
		ls.backlog = append([]backlogLine(nil), ls.backlog[n-liveBacklogLimit:]...)
	}
}

func (ls *liveSession) signalIdleLocked() {
	if ls.idle != nil && !ls.nativeBusy && ls.pendingNative == 0 {
		close(ls.idle)
		ls.idle = nil
	}
}

func (ls *liveSession) onControlResponse(line []byte, env envelope) {
	if env.Response == nil {
		return
	}
	id := rawString(env.Response.RequestID)
	if id == "ac-init" {
		ls.mu.Lock()
		if env.Response.Subtype == "success" && len(env.Response.Response) > 0 {
			ls.initResp = append(json.RawMessage(nil), env.Response.Response...)
		}
		ls.mu.Unlock()
		select {
		case <-ls.initDone:
		default:
			close(ls.initDone)
		}
		return
	}
	ls.mu.Lock()
	req, ok := ls.clientReqs[id]
	delete(ls.clientReqs, id)
	ls.mu.Unlock()
	if !ok {
		return // the daemon's own request
	}
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if resp, _ := m["response"].(map[string]any); resp != nil {
		resp["request_id"] = req.id
	}
	if b, err := json.Marshal(m); err == nil {
		req.client.send(b)
	}
}

// onProcessRequest routes a request from the CLI to its host. With nobody
// attached the daemon answers as print mode always did: nothing is approved
// interactively.
func (ls *liveSession) onProcessRequest(line []byte, env envelope) {
	if ls.toLatest(line) {
		return
	}
	sub := ""
	if env.Request != nil {
		sub = env.Request.Subtype
	}
	resp := map[string]any{"subtype": "error", "request_id": env.RequestID, "error": "no interactive host is attached to this Agent Conductor run"}
	if sub == "can_use_tool" {
		resp = map[string]any{"subtype": "success", "request_id": env.RequestID, "response": map[string]any{
			"behavior": "deny", "message": "No interactive approver is attached to this Agent Conductor run."}}
	}
	_ = ls.write(map[string]any{"type": "control_response", "response": resp})
}

func (ls *liveSession) toLatest(line []byte) bool {
	ls.mu.Lock()
	cl := ls.latest
	ls.mu.Unlock()
	if cl == nil {
		return false
	}
	cl.send(line)
	return true
}

// runTurn writes one prompt into the process and waits for its result.
func (ls *liveSession) runTurn(ctx, deadline context.Context, spec ExecSpec, sink Sink) error {
	turn := &liveTurn{uuid: newUUID(), done: make(chan struct{})}
	turn.st = &headlessState{spec: spec, sessionID: ls.sessionID, sink: func(ev Event) {
		if ev.Kind == EvTurnCompleted {
			ls.mu.Lock()
			why := turn.interrupted
			turn.reported = true
			ls.mu.Unlock()
			if ev.Status != TurnCompletedOK {
				switch {
				case why == "native":
					ev.Status = TurnInterrupted
					ev.Error = &ProviderError{Class: model.ErrUserInterrupt, Message: "Stopped from Claude Code"}
				case why == "daemon" && deadline.Err() == context.DeadlineExceeded:
					ev.Status = TurnFailed
					ev.Error = &ProviderError{Class: model.ErrNetwork, Message: "provider turn deadline exceeded"}
				case why == "daemon":
					ev.Status, ev.Error = TurnInterrupted, nil
				}
			}
		}
		sink(ev)
	}}

	// A conversation turn started from an attached Claude Code tab finishes
	// first; two prompts must not interleave in one turn.
	if err := ls.waitIdle(ctx); err != nil {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: ls.sessionID, Status: TurnInterrupted, At: time.Now().UTC()})
		return nil
	}
	ls.mu.Lock()
	if ls.parkTimer != nil {
		ls.parkTimer.Stop()
		ls.parkTimer = nil
	}
	ls.runID = spec.RunID
	ls.turn = turn
	ls.mu.Unlock()
	if err := ls.write(map[string]any{"type": "user", "uuid": turn.uuid, "session_id": "", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": spec.Prompt}}); err != nil {
		ls.mu.Lock()
		ls.turn = nil
		ls.mu.Unlock()
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: ls.sessionID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude stdin: " + err.Error() + " " + ls.stderr.String()}})
		return nil
	}

	select {
	case <-turn.done:
		ls.park()
		return nil
	case <-ls.exited:
		ls.reportExit(turn, spec, deadline, ctx.Err() != nil)
		return nil
	case <-ctx.Done():
	}
	// Cancellation, pause or deadline: interrupt the turn, then end the
	// process. Cancelling a run stops its execution, not just this prompt.
	ls.mu.Lock()
	if turn.interrupted == "" {
		turn.interrupted = "daemon"
	}
	ls.mu.Unlock()
	_ = ls.write(map[string]any{"type": "control_request", "request_id": "ac-interrupt-" + newUUID(),
		"request": map[string]any{"subtype": "interrupt"}})
	select {
	case <-turn.done:
	case <-ls.exited:
	case <-time.After(5 * time.Second):
	}
	ls.stopGroup()
	ls.reportExit(turn, spec, deadline, true)
	return nil
}

// stopGroup ends the process and every tool process it started. Cancelling
// a run has always signalled the whole process group; an interrupted tool can
// otherwise outlive Claude.
func (ls *liveSession) stopGroup() {
	ls.shutdown()
	_ = syscall.Kill(-ls.pid, syscall.SIGINT)
	select {
	case <-ls.exited:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-ls.pid, syscall.SIGKILL)
		select {
		case <-ls.exited:
		case <-time.After(10 * time.Second):
		}
	}
	// Claude has exited; anything still in its group is a tool it started.
	_ = syscall.Kill(-ls.pid, syscall.SIGTERM)
}

// reportExit emits the terminal event of a turn that ended without a result.
func (ls *liveSession) reportExit(turn *liveTurn, spec ExecSpec, deadline context.Context, cancelled bool) {
	ls.mu.Lock()
	reported := turn.reported
	turn.reported = true
	if ls.turn == turn {
		ls.turn = nil
	}
	ls.mu.Unlock()
	if reported {
		return
	}
	st := turn.st
	switch {
	case deadline.Err() == context.DeadlineExceeded:
		st.sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: ls.sessionID, Status: TurnFailed,
			Progress: st.meaningful, Error: &ProviderError{Class: model.ErrNetwork, Message: "provider turn deadline exceeded"}})
	case cancelled:
		st.sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: ls.sessionID, Status: TurnInterrupted,
			Progress: st.meaningful, At: time.Now().UTC()})
	case st.meaningful:
		st.sink(Event{Kind: EvProcessExited, RunID: spec.RunID, SessionID: ls.sessionID,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: "claude exited mid-turn: " + ls.stderr.String()}})
	default:
		msg := "claude exited before completing the turn"
		if tail := ls.stderr.String(); tail != "" {
			msg += ": " + tail
		}
		st.sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: ls.sessionID, Status: TurnFailed,
			Error: &ProviderError{Class: classifyClaudeText(msg), Message: msg}, At: time.Now().UTC()})
	}
}

func (ls *liveSession) finishTurn(turn *liveTurn) {
	ls.mu.Lock()
	if ls.turn == turn {
		ls.turn = nil
	}
	ls.mu.Unlock()
	select {
	case <-turn.done:
	default:
		close(turn.done)
	}
}

// waitIdle blocks while a turn started from an attached Claude Code tab runs.
func (ls *liveSession) waitIdle(ctx context.Context) error {
	for {
		ls.mu.Lock()
		if !ls.nativeBusy && ls.pendingNative == 0 {
			ls.mu.Unlock()
			return nil
		}
		if ls.idle == nil {
			ls.idle = make(chan struct{})
		}
		idle := ls.idle
		ls.mu.Unlock()
		select {
		case <-idle:
		case <-ls.exited:
			return fmt.Errorf("claude exited")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// park keeps an idle process for the run's next turn and for attached viewers,
// ending it once nothing needs it.
func (ls *liveSession) park() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closing || ls.turn != nil || ls.nativeBusy || len(ls.clients) > 0 {
		return
	}
	if ls.parkTimer != nil {
		ls.parkTimer.Stop()
	}
	ls.parkTimer = time.AfterFunc(liveParkTimeout, func() {
		ls.mu.Lock()
		idle := ls.turn == nil && !ls.nativeBusy && ls.pendingNative == 0 && len(ls.clients) == 0
		ls.mu.Unlock()
		if idle {
			ls.shutdown()
		}
	})
}

// shutdown ends the process: EOF first, so Claude finishes writing its
// transcript, then signals to the process group if it does not exit.
func (ls *liveSession) shutdown() {
	ls.mu.Lock()
	if ls.closing {
		ls.mu.Unlock()
		return
	}
	ls.closing = true
	if ls.parkTimer != nil {
		ls.parkTimer.Stop()
	}
	ls.thawLocked() // a stopped process cannot see EOF
	ls.mu.Unlock()
	ls.writeMu.Lock()
	ls.stdin.Close()
	ls.writeMu.Unlock()
	go func() {
		select {
		case <-ls.exited:
			return
		case <-time.After(10 * time.Second):
		}
		_ = syscall.Kill(-ls.pid, syscall.SIGINT)
		select {
		case <-ls.exited:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-ls.pid, syscall.SIGKILL)
		}
	}()
}

// executeLive runs one turn of a run in its long-lived process.
func (c *Claude) executeLive(ctx context.Context, spec ExecSpec, sink Sink) error {
	ls, err := c.obtainLive(ctx, spec)
	if err != nil {
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
			Error: &ProviderError{Class: model.ErrProcessCrash, Message: err.Error()}})
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cr := &claudeRun{runID: spec.RunID, sessionID: ls.sessionID, cancel: cancel, proc: ls.cmd.Process, exited: ls.exited}
	c.mu.Lock()
	c.running[spec.RunID] = cr
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.running[spec.RunID] == cr {
			delete(c.running, spec.RunID)
		}
		c.mu.Unlock()
	}()
	sink(Event{Kind: EvProcessStarted, RunID: spec.RunID, ProcessID: ls.pid, At: time.Now().UTC()})
	return ls.runTurn(runCtx, ctx, spec, sink)
}

// AttachResult tells the wrapper what to do.
type AttachResult struct {
	Attached  bool   `json:"attached"`
	Reason    string `json:"reason,omitempty"`
	RunID     string `json:"runId,omitempty"`
	PID       int    `json:"pid,omitempty"`
	HolderPID int    `json:"holderPid,omitempty"`
}

// PrepareNativeAttach finds or starts the daemon-owned process a Claude Code
// tab should attach to. host is nil for sessions Agent Conductor does not
// manage, which the wrapper then starts normally.
func (c *Claude) PrepareNativeAttach(ctx context.Context, sessionID string, host *ExecSpec) (*liveSession, AttachResult) {
	if ls := c.liveFor(sessionID); ls != nil {
		ls.mu.Lock()
		run := ls.runID
		ls.mu.Unlock()
		return ls, AttachResult{Attached: true, RunID: run, PID: ls.pid}
	}
	if host == nil {
		return nil, AttachResult{Reason: "not a managed session"}
	}
	if !c.supportsStreamInput(ctx) {
		return nil, AttachResult{Reason: "this Claude Code CLI cannot host an attachable session"}
	}
	// Someone else already runs this conversation (for example a terminal).
	// Starting another process would fork it.
	if pid := liveHolder(sessionID); pid > 0 {
		return nil, AttachResult{Reason: "held", HolderPID: pid}
	}
	spec := *host
	spec.SessionID = sessionID
	ls, err := c.obtainLive(ctx, spec)
	if err != nil {
		return nil, AttachResult{Reason: err.Error()}
	}
	return ls, AttachResult{Attached: true, RunID: spec.RunID, PID: ls.pid}
}

// ServeNative relays one Claude Code tab until either side closes. send
// writes one line to the tab; lines read from the tab arrive on in.
func (ls *liveSession) ServeNative(in *bufio.Reader, send func([]byte) error, end func()) {
	cl := &liveClient{w: send, end: end}
	ls.mu.Lock()
	if ls.closing {
		ls.mu.Unlock()
		end()
		return
	}
	ls.clients[cl] = struct{}{}
	ls.latest = cl
	ls.attaches++
	if ls.parkTimer != nil {
		ls.parkTimer.Stop()
		ls.parkTimer = nil
	}
	ls.mu.Unlock()
	ls.c.log.Infof("claude live session %s: Claude Code attached", ls.sessionID)
	defer func() {
		ls.mu.Lock()
		delete(ls.clients, cl)
		if ls.latest == cl {
			ls.latest = nil
			for other := range ls.clients {
				ls.latest = other
			}
		}
		for id, req := range ls.clientReqs {
			if req.client == cl {
				delete(ls.clientReqs, id)
			}
		}
		ls.mu.Unlock()
		ls.c.log.Infof("claude live session %s: Claude Code detached", ls.sessionID)
		ls.park()
	}()

	for {
		raw, err := in.ReadBytes('\n')
		if line := bytes.TrimSpace(raw); len(line) > 0 {
			ls.fromNative(cl, append([]byte(nil), line...))
		}
		if err != nil {
			return
		}
	}
}

func (ls *liveSession) fromNative(cl *liveClient, line []byte) {
	var env envelope
	if json.Unmarshal(line, &env) != nil {
		return
	}
	switch {
	case env.Type == "control_request" && env.Request != nil && env.Request.Subtype == "initialize":
		// The process was initialized when it started; a second initialize
		// would be refused. Answer from that response, then bring the tab up to
		// date with what it could not have read from the transcript.
		select {
		case <-ls.initDone:
		case <-time.After(liveInitTimeout):
		}
		ls.mu.Lock()
		resp := ls.initResp
		ls.mu.Unlock()
		if resp == nil {
			resp = json.RawMessage(`{}`)
		}
		b, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "success", "request_id": env.RequestID, "response": resp}})
		cl.send(b)
		for _, l := range ls.replay() {
			cl.send(l)
		}
		// The tab now has everything up to this point; let the run continue.
		ls.mu.Lock()
		ls.thawLocked()
		ls.mu.Unlock()
	case env.Type == "control_request":
		ls.mu.Lock()
		ls.reqSeq++
		id := fmt.Sprintf("ac-cli-%d", ls.reqSeq)
		ls.clientReqs[id] = clientRequest{client: cl, id: env.RequestID}
		if env.Request != nil && env.Request.Subtype == "interrupt" && ls.turn != nil && ls.turn.interrupted == "" {
			ls.turn.interrupted = "native"
		}
		ls.mu.Unlock()
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			return
		}
		m["request_id"] = id
		_ = ls.write(m)
	case env.Type == "user":
		ls.mu.Lock()
		ls.pendingNative++
		ls.mu.Unlock()
		_ = ls.writeLine(line)
	default:
		// control_response to the CLI's own requests, and anything newer.
		_ = ls.writeLine(line)
	}
}

// freezeForAttach pauses the process while a Claude Code tab opens.
//
// A tab shows the transcript as read when it opens, then attaches a moment
// later (about 50 ms to several seconds). Anything Claude writes in between
// would be in neither the snapshot nor the replay — or in both, since the tab
// does not de-duplicate. Stopping the CLI process (not its tool processes)
// makes the transcript the tab reads and the one replay compares against
// identical. The tab's initialize resumes it; max bounds the pause if no tab
// attaches.
func (ls *liveSession) freezeForAttach(max time.Duration) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.closing || ls.pid <= 0 || len(ls.clients) > 0 {
		return false
	}
	if !ls.frozen {
		if err := syscall.Kill(ls.pid, syscall.SIGSTOP); err != nil {
			return false
		}
		ls.frozen = true
		ls.c.log.Infof("claude live session %s: paused for a Claude Code tab to open", ls.sessionID)
	}
	if ls.freezeTimer != nil {
		ls.freezeTimer.Stop()
	}
	ls.freezeTimer = time.AfterFunc(max, func() {
		ls.mu.Lock()
		ls.thawLocked()
		ls.mu.Unlock()
	})
	return true
}

func (ls *liveSession) thawLocked() {
	if ls.freezeTimer != nil {
		ls.freezeTimer.Stop()
		ls.freezeTimer = nil
	}
	if ls.frozen {
		ls.frozen = false
		_ = syscall.Kill(ls.pid, syscall.SIGCONT)
	}
}

// FreezeForAttach pauses a session's process until a Claude Code tab
// attaches or max elapses. False means nothing was paused (no process, or a
// tab is already attached and opening only focuses it).
func (c *Claude) FreezeForAttach(sessionID string, max time.Duration) bool {
	ls := c.liveFor(sessionID)
	return ls != nil && ls.freezeForAttach(max)
}

// ThawAttach resumes a paused session's process. Idempotent.
func (c *Claude) ThawAttach(sessionID string) {
	if ls := c.liveFor(sessionID); ls != nil {
		ls.mu.Lock()
		ls.thawLocked()
		ls.mu.Unlock()
	}
}

// replay returns output a tab could not have loaded: everything after the
// newest line already in the transcript. The tab shows the transcript as it
// was when it opened; replaying lines it already shows duplicates them.
func (ls *liveSession) replay() [][]byte {
	persisted := transcriptUUIDs(ls.sessionID)
	ls.mu.Lock()
	defer ls.mu.Unlock()
	start := 0
	for i := len(ls.backlog) - 1; i >= 0; i-- {
		if u := ls.backlog[i].uuid; u != "" && persisted[u] {
			start = i + 1
			break
		}
	}
	out := make([][]byte, 0, len(ls.backlog)-start)
	for _, b := range ls.backlog[start:] {
		out = append(out, b.line)
	}
	return out
}

// claudeConfigDir mirrors the CLI's resolution of its state directory.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// transcriptUUIDs reads the message ids already persisted for a session.
func transcriptUUIDs(sessionID string) map[string]bool {
	out := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*", sessionID+".jsonl"))
	for _, p := range matches {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
		for sc.Scan() {
			var e struct {
				UUID string `json:"uuid"`
			}
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.UUID != "" {
				out[e.UUID] = true
			}
		}
		f.Close()
	}
	return out
}

// liveHolder returns the pid of another live Claude process registered for a
// session in Claude Code's session registry (<config>/sessions/<pid>.json).
func liveHolder(sessionID string) int {
	matches, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "sessions", "*.json"))
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(data, &rec) != nil || rec.SessionID != sessionID || rec.PID <= 0 {
			continue
		}
		if err := syscall.Kill(rec.PID, 0); err == nil || err == syscall.EPERM {
			return rec.PID
		}
	}
	return 0
}

// LiveStatus reports the live process serving a run.
func (c *Claude) LiveStatus(runID string) (LiveStatus, bool) {
	c.mu.Lock()
	var found *liveSession
	for _, ls := range c.live {
		ls.mu.Lock()
		if ls.runID == runID {
			found = ls
		}
		ls.mu.Unlock()
	}
	c.mu.Unlock()
	if found == nil {
		return LiveStatus{}, false
	}
	found.mu.Lock()
	defer found.mu.Unlock()
	st := LiveStatus{SessionID: found.sessionID, PID: found.pid, Viewers: len(found.clients), Attaches: found.attaches}
	switch {
	case found.turn != nil:
		st.Busy = "conductor"
	case found.nativeBusy:
		st.Busy = "native"
	}
	return st, true
}

// LiveStatusForSession reports the live process of a session, if any.
func (c *Claude) LiveStatusForSession(sessionID string) (LiveStatus, string, bool) {
	ls := c.liveFor(sessionID)
	if ls == nil {
		return LiveStatus{}, "", false
	}
	ls.mu.Lock()
	run := ls.runID
	ls.mu.Unlock()
	st, ok := c.LiveStatus(run)
	return st, run, ok
}

// endLive ends every process serving a run (cancellation).
func (c *Claude) endLive(runID string) bool {
	c.mu.Lock()
	var hits []*liveSession
	for _, ls := range c.live {
		ls.mu.Lock()
		if ls.runID == runID {
			hits = append(hits, ls)
		}
		ls.mu.Unlock()
	}
	c.mu.Unlock()
	for _, ls := range hits {
		go ls.stopGroup()
	}
	return len(hits) > 0
}
