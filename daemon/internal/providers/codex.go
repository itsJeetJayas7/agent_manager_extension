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

	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
)

// Codex speaks JSON-RPC 2.0 over the `codex app-server` child process stdio.
// Method surface per official app-server docs (see docs/provider-capabilities.md).
type Codex struct {
	// startMu serialises process startup. It is held across the initialize
	// handshake, which is why the handshake must NOT take mu: ensureStarted
	// used to hold mu for its whole body and then call call(), which takes mu
	// again. sync.Mutex is not reentrant, so the first thing that touched
	// Codex deadlocked forever — and because mu was never released, every
	// later Codex operation (including Capabilities from the snapshot path)
	// blocked with it.
	startMu sync.Mutex

	mu      sync.Mutex // guards the fields below
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	nextID  int
	pending map[int]chan rpcResult
	started bool
	// threads maps a run to the thread its turn is running on, so Cancel can
	// send turn/interrupt. This map was declared and never written, which made
	// Cancel a complete no-op.
	threads map[string]string
	cancels map[string]context.CancelFunc
	// backoff throttles restart attempts after the app-server dies.
	backoffUntil time.Time
	backoff      time.Duration

	writeMu sync.Mutex // serialises stdin writes
	subsMu  sync.Mutex
	subs    map[string][]chan map[string]any // threadID -> subscribers ("*" = all)

	probeMu sync.Mutex
	probe   map[string]string // JSON-RPC method -> "ok" | error text

	log     *logging.Logger
	binPath string
	crashed func(string) // optional: surfaced to the UI
}

type rpcResult struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// NewCodex creates the adapter; binPath may be empty to auto-detect.
func NewCodex(log *logging.Logger, binPath string) *Codex {
	return &Codex{
		pending: map[int]chan rpcResult{},
		subs:    map[string][]chan map[string]any{},
		log:     log,
		binPath: binPath,
		cancels: map[string]context.CancelFunc{},
		threads: map[string]string{},
		probe:   map[string]string{},
	}
}

// Name identifies the adapter.
func (c *Codex) Name() string { return model.ProviderCodex }

// Available probes for the codex binary.
func (c *Codex) Available(ctx context.Context) bool {
	bin := c.binPath
	if bin == "" {
		bin = findBinary("codex")
	}
	if bin == "" {
		return false
	}
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil {
		return false
	}
	c.log.Infof("codex runtime detected: %s", strings.TrimSpace(string(out)))
	return true
}

// Capabilities probes auth, models, and quota surface.
func (c *Codex) Capabilities(ctx context.Context) Caps {
	caps := Caps{AuthState: "unknown"}
	if !c.Available(ctx) {
		return caps
	}
	if err := c.ensureStarted(ctx); err != nil {
		caps.Notes = append(caps.Notes, "app-server start failed: "+err.Error())
		return caps
	}
	caps.Installed = true
	if v, err := c.version(ctx); err == nil {
		caps.Version = v
	}
	// account
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var acc struct {
		AuthMode string `json:"authMode"`
		Plan     string `json:"plan"`
	}
	if raw, err := c.call(ctx2, "account/read", map[string]any{}); err == nil {
		json.Unmarshal(raw, &acc)
		switch strings.ToLower(acc.AuthMode) {
		case "chatgpt":
			caps.AuthState = "ok"
		case "apikey", "api_key":
			caps.AuthState = "api_key"
		case "":
			caps.AuthState = "unknown"
		default:
			caps.AuthState = acc.AuthMode
		}
		caps.PlanLabel = acc.Plan
	}
	// models
	if raw, err := c.call(ctx2, "model/list", map[string]any{}); err == nil {
		var ml struct {
			Models []struct {
				ID              string `json:"id"`
				DisplayName     string `json:"displayName"`
				ReasoningEffort string `json:"reasoningEffort"`
				Hidden          bool   `json:"hidden"`
			} `json:"models"`
		}
		if json.Unmarshal(raw, &ml) == nil {
			for _, m := range ml.Models {
				caps.Models = append(caps.Models, ModelInfo{ID: m.ID, Name: m.DisplayName, Efforts: parseEfforts(m.ReasoningEffort), Hidden: m.Hidden})
			}
		}
	}
	if _, err := c.ProbeQuota(ctx2); err == nil {
		caps.RealTimeQuota = true
	}
	return caps
}

// parseEfforts returns the effort ladder with the model's own default first,
// so the UI offers the provider's preference rather than an arbitrary order.
// The parameter used to be ignored entirely.
func parseEfforts(defaultEffort string) []string {
	all := []string{"low", "medium", "high", "xhigh"}
	d := strings.ToLower(strings.TrimSpace(defaultEffort))
	if d == "" {
		return all
	}
	out := []string{d}
	for _, e := range all {
		if e != d {
			out = append(out, e)
		}
	}
	return out
}

func (c *Codex) version(ctx context.Context) (string, error) {
	bin := c.binPath
	if bin == "" {
		bin = findBinary("codex")
	}
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ---- process & rpc plumbing ----

// ensureStarted spawns `codex app-server` and completes the handshake.
//
// The lock is held only while mutating process state; the initialize call
// happens with mu released.
func (c *Codex) ensureStarted(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	if wait := time.Until(c.backoffUntil); wait > 0 {
		c.mu.Unlock()
		return fmt.Errorf("codex app-server is restarting; retry in %s", wait.Round(time.Second))
	}
	c.mu.Unlock()

	bin := c.binPath
	if bin == "" {
		bin = findBinary("codex")
	}
	if bin == "" {
		return fmt.Errorf("codex binary not found")
	}
	cmd := exec.Command(bin, "app-server")
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		c.noteFailure()
		return fmt.Errorf("codex app-server start: %w", err)
	}

	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.started = true
	c.mu.Unlock()

	reader := bufio.NewReaderSize(stdout, 16*1024*1024)
	go c.readLoop(cmd, reader)
	// Reap the child. Without this every app-server left a zombie until the
	// daemon exited.
	go func() {
		err := cmd.Wait()
		c.log.Infof("codex app-server exited: %v", err)
		c.failAllPending(fmt.Errorf("codex app-server exited: %v", err))
	}()

	// Handshake with mu released.
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := c.call(ctx2, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "agent-conductor", "title": "Agent Conductor", "version": "0.1.0"},
	}); err != nil {
		c.recordProbe("initialize", err)
		c.shutdownChild()
		c.noteFailure()
		return fmt.Errorf("initialize: %w", err)
	}
	c.recordProbe("initialize", nil)
	c.mu.Lock()
	c.backoff = 0
	c.backoffUntil = time.Time{}
	c.mu.Unlock()
	c.log.Infof("codex app-server initialized")
	return nil
}

// noteFailure applies exponential restart backoff (1s doubling to 60s) so a
// broken app-server is not respawned on every scheduling pass.
func (c *Codex) noteFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backoff == 0 {
		c.backoff = time.Second
	} else if c.backoff < time.Minute {
		c.backoff *= 2
	}
	c.backoffUntil = time.Now().Add(c.backoff)
	c.started = false
}

// shutdownChild stops the current app-server without touching startMu.
func (c *Codex) shutdownChild() {
	c.mu.Lock()
	cmd, stdin := c.cmd, c.stdin
	c.cmd, c.stdin, c.started = nil, nil, false
	c.mu.Unlock()
	if stdin != nil {
		stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Signal(os.Interrupt)
		go func() {
			time.Sleep(3 * time.Second)
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}()
	}
}

// recordProbe remembers whether a JSON-RPC method worked, so the Control panel
// can say which parts of the app-server surface this build actually supports
// instead of silently doing nothing.
func (c *Codex) recordProbe(method string, err error) {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	if err == nil {
		c.probe[method] = "ok"
		return
	}
	c.probe[method] = err.Error()
}

// MethodSupport reports the outcome of every JSON-RPC method tried so far.
func (c *Codex) MethodSupport() map[string]string {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	out := make(map[string]string, len(c.probe))
	for k, v := range c.probe {
		out[k] = v
	}
	return out
}

// OnCrash registers a callback invoked when the app-server dies unexpectedly.
func (c *Codex) OnCrash(fn func(string)) { c.crashed = fn }

func (c *Codex) readLoop(cmd *exec.Cmd, reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.failAllPending(fmt.Errorf("app-server stream closed: %w", err))
			return
		}
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.ID != nil && msg.Method == "" {
			// response
			c.mu.Lock()
			ch, ok := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.mu.Unlock()
			if ok {
				ch <- rpcResult{Result: msg.Result, Error: msg.Error}
			}
			continue
		}
		if msg.Method != "" {
			var params map[string]any
			json.Unmarshal(msg.Params, &params)
			c.dispatch(msg.Method, params)
		}
	}
}

func (c *Codex) failAllPending(err error) {
	c.mu.Lock()
	wasStarted := c.started
	for id, ch := range c.pending {
		select {
		case ch <- rpcResult{Error: &rpcError{Message: err.Error()}}:
		default:
		}
		delete(c.pending, id)
	}
	c.started = false
	c.mu.Unlock()
	if wasStarted {
		c.noteFailure()
		c.log.Errorf("codex app-server lost: %v", err)
		if c.crashed != nil {
			c.crashed(err.Error())
		}
	}
	// The dead child is released here; the next ensureStarted spawns a fresh
	// one after the backoff rather than writing into a closed pipe.
	c.shutdownChild()
}

func (c *Codex) dispatch(method string, params map[string]any) {
	threadID := ""
	if params != nil {
		if t, ok := params["threadId"].(string); ok {
			threadID = t
		}
	}
	c.subsMu.Lock()
	targets := append([]chan map[string]any{}, c.subs["*"]...)
	if threadID != "" {
		targets = append(targets, c.subs[threadID]...)
	}
	c.subsMu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- map[string]any{"method": method, "params": params}:
		default:
		}
	}
}

func (c *Codex) subscribe(threadID string) chan map[string]any {
	ch := make(chan map[string]any, 256)
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	c.subs[threadID] = append(c.subs[threadID], ch)
	return ch
}

func (c *Codex) unsubscribe(threadID string, ch chan map[string]any) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	list := c.subs[threadID]
	for i, s := range list {
		if s == ch {
			rest := append(list[:i], list[i+1:]...)
			if len(rest) == 0 {
				delete(c.subs, threadID)
			} else {
				c.subs[threadID] = rest
			}
			return
		}
	}
}

// call issues one JSON-RPC request. It never starts the process: callers run
// ensureStarted first, which keeps the startup path free of re-entrancy.
func (c *Codex) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil, fmt.Errorf("codex app-server is not running")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResult, 1)
	c.pending[id] = ch
	stdin := c.stdin
	c.mu.Unlock()

	payload := map[string]any{"jsonrpc": "2.0", "method": method, "id": id, "params": params}
	b, _ := json.Marshal(payload)
	c.writeMu.Lock()
	_, werr := stdin.Write(append(b, '\n'))
	c.writeMu.Unlock()
	if werr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("codex write: %w", werr)
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case res := <-ch:
		if res.Error != nil {
			err := fmt.Errorf("codex %s: %s", method, res.Error.Message)
			c.recordProbe(method, err)
			return nil, err
		}
		c.recordProbe(method, nil)
		return res.Result, nil
	}
}

// ---- Adapter interface ----

// permissionToCodex maps conductor permission to approvalPolicy + sandbox.
func permissionToCodex(p string) (approval, sandbox string) {
	switch p {
	case "bypass":
		return "never", "dangerFullAccess"
	case "plan":
		return "never", "readOnly"
	case "acceptEdits":
		return "never", "workspaceWrite"
	default:
		return "on-request", "workspaceWrite"
	}
}

// Execute runs one turn on a (new or existing) thread.
func (c *Codex) Execute(ctx context.Context, spec ExecSpec, sink Sink) error {
	if err := c.ensureStarted(ctx); err != nil {
		return err
	}
	approval, sandbox := permissionToCodex(spec.Permission)

	// Register the execution so Cancel can reach it. Without this the cancels
	// map stayed empty and cancelling a Codex run did nothing.
	ctx, cancelRun := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancels[spec.RunID] = cancelRun
	c.mu.Unlock()
	defer func() {
		cancelRun()
		c.mu.Lock()
		delete(c.cancels, spec.RunID)
		delete(c.threads, spec.RunID)
		c.mu.Unlock()
	}()

	threadID := spec.SessionID
	noteThread := func(id string) {
		c.mu.Lock()
		c.threads[spec.RunID] = id
		c.mu.Unlock()
	}
	if threadID != "" {
		noteThread(threadID)
	}
	if threadID == "" {
		params := map[string]any{
			"cwd":            spec.Cwd,
			"serviceName":    "agent_conductor",
			"approvalPolicy": approval,
			"sandbox":        sandbox,
		}
		if spec.Model != "" {
			params["model"] = spec.Model
		}
		ctxSt, cancel := context.WithTimeout(ctx, 30*time.Second)
		raw, err := c.call(ctxSt, "thread/start", params)
		cancel()
		if err != nil {
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
				Error: &ProviderError{Class: model.ErrNetwork, Message: err.Error()}})
			return nil
		}
		var tr struct {
			Thread struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionId"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &tr); err != nil || tr.Thread.ID == "" {
			// tolerate alternative shapes
			var alt struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionId"`
			}
			json.Unmarshal(raw, &alt)
			if alt.ID == "" {
				sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, Status: TurnFailed,
					Error: &ProviderError{Class: model.ErrUnknown, Message: "thread/start: unreadable response"}})
				return nil
			}
			tr.Thread.ID, tr.Thread.SessionID = alt.ID, alt.SessionID
		}
		threadID = tr.Thread.ID
		noteThread(threadID)
		sink(Event{Kind: EvSessionStarted, RunID: spec.RunID, SessionID: threadID, SubSession: tr.Thread.SessionID, Model: spec.Model, At: time.Now().UTC()})
	} else {
		// resume existing thread for continuation / staged build phase
		ctxRs, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := c.call(ctxRs, "thread/resume", map[string]any{"threadId": threadID, "cwd": spec.Cwd})
		cancel()
		if err != nil {
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: threadID, Status: TurnFailed,
				Error: &ProviderError{Class: model.ErrNetwork, Message: "thread/resume: " + err.Error()}})
			return nil
		}
	}

	events := c.subscribe(threadID)
	defer c.unsubscribe(threadID, events)

	turnParams := map[string]any{
		"threadId":       threadID,
		"input":          []map[string]any{{"type": "text", "text": spec.Prompt}},
		"cwd":            spec.Cwd,
		"approvalPolicy": approval,
		"sandbox":        sandbox,
	}
	if spec.Model != "" {
		turnParams["model"] = spec.Model
	}
	if spec.Effort != "" {
		turnParams["effort"] = spec.Effort
	}
	ctxTs, cancel := context.WithTimeout(ctx, 60*time.Second)
	raw, err := c.call(ctxTs, "turn/start", turnParams)
	cancel()
	if err != nil {
		cls := model.ClassifyCodexError("", err.Error())
		sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: threadID, Status: TurnFailed,
			Error: &ProviderError{Class: cls, Message: err.Error()}})
		return nil
	}
	turnID := ""
	var tr struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &tr) == nil && tr.Turn.ID != "" {
		turnID = tr.Turn.ID
	}

	sink(Event{Kind: EvTurnStarted, RunID: spec.RunID, SessionID: threadID, TurnID: turnID, At: time.Now().UTC()})

	// consume notifications until our turn completes
	agentText := &strings.Builder{}
	for {
		select {
		case <-ctx.Done():
			sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: threadID, TurnID: turnID, Status: TurnInterrupted})
			return nil
		case ev := <-events:
			method, _ := ev["method"].(string)
			params, _ := ev["params"].(map[string]any)
			switch method {
			case "turn/started":
				if t, ok := params["turn"].(map[string]any); ok {
					if id, ok := t["id"].(string); ok && turnID == "" {
						turnID = id
					}
				}
			case "item/started", "item/completed":
				if it, ok := params["item"].(map[string]any); ok {
					if kind, _ := it["type"].(string); kind != "" && kind != "agentMessage" && kind != "reasoning" {
						sink(Event{Kind: EvMessageDelta, RunID: spec.RunID, SessionID: threadID, TurnID: turnID,
							Text: "item:" + kind, Progress: true, At: time.Now().UTC()})
					}
				}
			case "item/agentMessage/delta":
				if d, ok := params["delta"].(string); ok {
					agentText.WriteString(d)
					sink(Event{Kind: EvMessageDelta, RunID: spec.RunID, SessionID: threadID, TurnID: turnID, Text: d, Progress: true, At: time.Now().UTC()})
				}
			case "account/rateLimits/updated":
				if qw := codexParseRateLimits(params); len(qw) > 0 {
					sink(Event{Kind: EvQuotaUpdate, RunID: spec.RunID, Quota: qw, At: time.Now().UTC()})
				}
			case "turn/completed":
				t, _ := params["turn"].(map[string]any)
				id, _ := t["id"].(string)
				if turnID != "" && id != "" && id != turnID {
					continue
				}
				status, _ := t["status"].(string)
				finalMsg := agentText.String()
				switch status {
				case "completed":
					sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: threadID, TurnID: id,
						Status: TurnCompletedOK, FinalMsg: finalMsg, Progress: true, At: time.Now().UTC()})
				case "interrupted":
					sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: threadID, TurnID: id,
						Status: TurnInterrupted, At: time.Now().UTC()})
				default: // failed
					msg := "turn failed"
					cls := model.ErrUnknown
					if t != nil {
						if e, ok := t["error"].(map[string]any); ok {
							if m, ok := e["message"].(string); ok {
								msg = m
							}
							if ci, ok := e["codexErrorInfo"].(map[string]any); ok {
								if code, _ := ci["type"].(string); code != "" {
									cls = model.ClassifyCodexError(code, msg)
								}
							}
						}
					}
					sink(Event{Kind: EvTurnCompleted, RunID: spec.RunID, SessionID: threadID, TurnID: id,
						Status: TurnFailed, Error: &ProviderError{Class: cls, Message: msg}, At: time.Now().UTC()})
				}
				return nil
			}
		}
	}
}

// Cancel interrupts the active turn for a run's thread.
//
// This used to look up a map that was never written to, so cancelling a Codex
// run did nothing at all. It now sends turn/interrupt for the run's thread so
// the agent stops at a safe point, then cancels the execution context.
func (c *Codex) Cancel(runID string) error {
	c.mu.Lock()
	cancel := c.cancels[runID]
	threadID := c.threads[runID]
	c.mu.Unlock()

	var interruptErr error
	if threadID != "" {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		interruptErr = c.InterruptThread(ctx, threadID)
		done()
		if interruptErr != nil {
			c.log.Errorf("codex turn/interrupt for %s: %v", runID, interruptErr)
		}
	}
	if cancel != nil {
		cancel()
	}
	if threadID == "" && cancel == nil {
		return fmt.Errorf("no active codex turn for run %s", runID)
	}
	return nil
}

// InterruptThread sends turn/interrupt for a thread's active turn.
func (c *Codex) InterruptThread(ctx context.Context, threadID string) error {
	if err := c.ensureStarted(ctx); err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := c.call(ctx2, "turn/interrupt", map[string]any{"threadId": threadID})
	return err
}

// ProbeQuota reads structured rate limits.
func (c *Codex) ProbeQuota(ctx context.Context) ([]QuotaWindow, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := c.call(ctx2, "account/rateLimits/read", map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	qw := codexParseRateLimits(parsed)
	if len(qw) == 0 {
		return nil, fmt.Errorf("rate limits present but no windows parsed")
	}
	return qw, nil
}

// codexParseRateLimits tolerates current response shapes:
// { rateLimits: { primary: {windowMinutes, usedPercent, resetsAt, ...}, secondary: ... } }
// or a flat map of windows.
func codexParseRateLimits(payload map[string]any) []QuotaWindow {
	var out []QuotaWindow
	appendWindow := func(id string, m map[string]any) {
		w := QuotaWindow{ID: id, Source: "provider_structured", Confidence: "exact"}
		if v, ok := m["usedPercent"].(float64); ok {
			w.UsedPercent = v
			w.HasPercent = true
		}
		if s, ok := m["resetsAt"].(string); ok && s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				w.ResetsAt = t
				w.HasReset = true
			}
		}
		if v, ok := m["windowMinutes"].(float64); ok && !w.HasReset {
			w.Name = fmt.Sprintf("%v-hour window", v/60)
		}
		if v, ok := m["isAtLimit"].(bool); ok {
			w.Exhausted = v
		}
		if !w.Exhausted && w.HasPercent && w.UsedPercent >= 100 {
			w.Exhausted = true
		}
		if n, ok := m["title"].(string); ok && n != "" {
			w.Name = n
		}
		if w.Name == "" {
			w.Name = id
		}
		out = append(out, w)
	}
	if rl, ok := payload["rateLimits"].(map[string]any); ok {
		for id, v := range rl {
			if m, ok := v.(map[string]any); ok {
				appendWindow(id, m)
			}
		}
		return out
	}
	for id, v := range payload {
		if m, ok := v.(map[string]any); ok {
			if _, has := m["usedPercent"]; has {
				appendWindow(id, m)
			}
		}
	}
	return out
}

// Close shuts the app-server down.
func (c *Codex) Close() error {
	c.shutdownChild()
	return nil
}
