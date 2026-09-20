// Package api implements the daemon's Unix-socket NDJSON protocol with
// machine-local token auth. Protocol version 1.
package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-conductor/internal/db"
	"agent-conductor/internal/logging"
	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
	"agent-conductor/internal/quota"
	"agent-conductor/internal/scheduler"
)

// Protocol version.
const ProtocolVersion = 1

// Paths computes daemon filesystem locations from a root (~/.agent-conductor).
type Paths struct {
	Root      string
	Socket    string
	Token     string
	DB        string
	Key       string
	Logs      string
	Worktrees string
}

// DefaultPaths builds standard paths for root.
func DefaultPaths(root string) Paths {
	return Paths{
		Root:      root,
		Socket:    filepath.Join(root, "run", "conductor.sock"),
		Token:     filepath.Join(root, "run", "conductor.token"),
		DB:        filepath.Join(root, "state", "conductor.db"),
		Key:       filepath.Join(root, "state", "vault.key"),
		Logs:      filepath.Join(root, "logs"),
		Worktrees: filepath.Join(root, "worktrees"),
	}
}

// Server serves the protocol.
type Server struct {
	paths     Paths
	store     *db.Store
	conductor *scheduler.Conductor
	registry  *providers.Registry
	guardian  *quota.Guardian
	hookBus   *providers.HookBus
	log       *logging.Logger
	version   string
	mu        sync.Mutex
	listener  net.Listener
	clients   map[net.Conn]*clientConn
	startedAt time.Time

	capsMu    sync.Mutex
	capsCache map[string]providers.Caps
	capsAt    time.Time
}

const capsTTL = 30 * time.Second

// clientConn tracks one connection's writer channel and its lifetime.
// `out` is never closed: the subscription goroutine may still be selecting on
// it, and closing a channel from anywhere but the sole sender panics the
// daemon. Shutting `done` instead stops every writer cleanly.
type clientConn struct {
	conn     net.Conn
	out      chan any
	done     chan struct{}
	shutdown func()
}

// NewServer wires the API server.
func NewServer(paths Paths, store *db.Store, conductor *scheduler.Conductor, registry *providers.Registry, guardian *quota.Guardian, hookBus *providers.HookBus, log *logging.Logger, version string) *Server {
	return &Server{
		paths: paths, store: store, conductor: conductor, registry: registry,
		guardian: guardian, hookBus: hookBus, log: log, version: version,
		clients: map[net.Conn]*clientConn{}, startedAt: time.Now().UTC(),
		capsCache: map[string]providers.Caps{},
	}
}

// ensureToken loads or creates the 0600 auth token file.
func (s *Server) ensureToken() (string, error) {
	data, err := os.ReadFile(s.paths.Token)
	if err == nil && len(data) >= 32 {
		return strings.TrimSpace(string(data)), nil
	}
	if err := os.MkdirAll(filepath.Dir(s.paths.Token), 0o700); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := os.WriteFile(s.paths.Token, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// Listen starts listening on the Unix socket.
func (s *Server) Listen() error {
	tok, err := s.ensureToken()
	if err != nil {
		return err
	}
	_ = tok
	if err := os.MkdirAll(filepath.Dir(s.paths.Socket), 0o700); err != nil {
		return err
	}
	os.Remove(s.paths.Socket)
	ln, err := net.Listen("unix", s.paths.Socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.paths.Socket, 0o600); err != nil {
		ln.Close()
		return err
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	s.log.Infof("listening on %s", s.paths.Socket)
	return nil
}

// Serve accepts connections until Close.
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

// Close stops the server and removes the socket file.
func (s *Server) Close() {
	s.mu.Lock()
	ln := s.listener
	s.listener = nil
	clients := s.clients
	s.clients = map[net.Conn]*clientConn{}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for c, cc := range clients {
		cc.shutdown()
		c.Close()
	}
	os.Remove(s.paths.Socket)
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	out := make(chan any, 256)
	done := make(chan struct{})
	var closeOnce sync.Once
	shutdown := func() { closeOnce.Do(func() { close(done) }) }
	defer shutdown()

	// One subscription per connection, unsubscribed when the connection ends.
	// Previously every reconnect leaked a goroutine plus a 256-slot channel
	// that the conductor kept fanning out to forever.
	var subCh chan scheduler.OutEvent
	defer func() {
		if subCh != nil {
			s.conductor.Unsubscribe(subCh)
		}
	}()

	s.mu.Lock()
	s.clients[conn] = &clientConn{conn: conn, out: out, done: done, shutdown: shutdown}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
	}()

	var writerMu sync.Mutex
	writeMsg := func(msg any) {
		b, err := json.Marshal(msg)
		if err != nil {
			return
		}
		writerMu.Lock()
		writer.Write(b)
		writer.WriteByte('\n')
		writer.Flush()
		writerMu.Unlock()
	}

	tok, _ := os.ReadFile(s.paths.Token)
	want := strings.TrimSpace(string(tok))
	authed := false

	go func() {
		for {
			select {
			case <-done:
				return
			case msg := <-out:
				writeMsg(msg)
			}
		}
	}()

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		if len(line) == 0 {
			continue
		}
		var req struct {
			Protocol int             `json:"protocol"`
			ID       json.RawMessage `json:"id"`
			Method   string          `json:"method"`
			Auth     string          `json:"auth"`
			Params   json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			s.reply(writeMsg, nil, nil, map[string]string{"code": "BAD_JSON", "message": err.Error()})
			continue
		}
		if req.Protocol != ProtocolVersion {
			s.reply(writeMsg, req.ID, nil, map[string]string{"code": "PROTOCOL", "message": fmt.Sprintf("unsupported protocol %d", req.Protocol)})
			continue
		}
		if !authed {
			// token accepted from top-level `auth` or `params.auth`
			auth := req.Auth
			if auth == "" {
				var p struct {
					Auth string `json:"auth"`
				}
				json.Unmarshal(req.Params, &p)
				auth = p.Auth
			}
			if req.Method != "hello" {
				s.reply(writeMsg, req.ID, nil, map[string]string{"code": "UNAUTHORIZED", "message": "send hello with auth token first"})
				return
			}
			if subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
				s.reply(writeMsg, req.ID, nil, map[string]string{"code": "UNAUTHORIZED", "message": "invalid token"})
				return
			}
			authed = true
			s.reply(writeMsg, req.ID, map[string]any{"protocol": ProtocolVersion, "version": s.version, "pid": os.Getpid()}, nil)
			continue
		}
		if req.Method == "subscribe" {
			// Idempotent: a repeated subscribe on one connection used to
			// multiply the fan-out instead of being a no-op.
			if subCh == nil {
				subCh = s.conductor.Subscribe()
				go func(ch chan scheduler.OutEvent) {
					for {
						select {
						case <-done:
							return
						case ev, ok := <-ch:
							if !ok {
								return
							}
							s.send(out, done, map[string]any{"event": ev.Type, "payload": ev.Payload})
						}
					}
				}(subCh)
			}
			s.reply(writeMsg, req.ID, map[string]any{"ok": true}, nil)
			continue
		}
		result, apiErr := s.dispatch(req.Method, req.Params)
		if apiErr != nil {
			s.reply(writeMsg, req.ID, nil, map[string]string{"code": apiErr.code, "message": apiErr.message})
			continue
		}
		s.reply(writeMsg, req.ID, result, nil)
	}
}

// send never blocks and never writes to a connection that has gone away.
func (s *Server) send(out chan any, done <-chan struct{}, msg any) {
	select {
	case <-done:
	case out <- msg:
	default: // slow consumer: drop rather than stall the conductor
	}
}

func (s *Server) reply(write func(any), id json.RawMessage, result any, errObj map[string]string) {
	resp := map[string]any{}
	if len(id) > 0 {
		resp["id"] = id
	}
	if errObj != nil {
		resp["error"] = errObj
	} else {
		resp["result"] = result
	}
	write(resp)
}

type apiError struct{ code, message string }

func e(code, msg string) *apiError { return &apiError{code, msg} }

func (s *Server) dispatch(method string, params json.RawMessage) (any, *apiError) {
	switch method {
	case "getSnapshot":
		return s.snapshot(), nil
	case "enqueueRun":
		var in scheduler.EnqueueRunInput
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		batchID, runID, err := s.conductor.EnqueueRun(in)
		if err != nil {
			return nil, e("ENQUEUE", err.Error())
		}
		return map[string]any{"batchId": batchID, "runId": runID}, nil
	case "enqueueBatch":
		var spec scheduler.BatchSpecInput
		if err := json.Unmarshal(params, &spec); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		id, err := s.conductor.ApplyBatch(spec)
		if err != nil {
			return nil, e("BATCH", err.Error())
		}
		return map[string]any{"batchId": id}, nil
	case "prepareBatch":
		var spec scheduler.BatchSpecInput
		if err := json.Unmarshal(params, &spec); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		if err := s.conductor.PrepareBatch(spec); err != nil {
			return nil, e("PRECHECK", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "reorderQueue":
		var in struct {
			RunID       string `json:"runId"`
			BeforeRunID string `json:"beforeRunId"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.ReorderQueue(in.RunID, in.BeforeRunID); err != nil {
			return nil, e("REORDER", err.Error())
		}
		return map[string]any{"ok": true}, nil
	// ---- pause (§5.3) ----
	case "pauseQueue":
		var in struct {
			Scope      string `json:"scope"`
			DurationMs int64  `json:"durationMs"`
			ResumeAt   string `json:"resumeAt"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		req := scheduler.PauseRequest{
			Scope:      scheduler.PauseScope(in.Scope),
			DurationMs: in.DurationMs,
			Reason:     in.Reason,
			Source:     scheduler.PauseSourceUser,
		}
		if in.ResumeAt != "" {
			t, err := time.Parse(time.RFC3339, in.ResumeAt)
			if err != nil {
				return nil, e("BAD_PARAMS", "resumeAt must be RFC3339: "+err.Error())
			}
			req.ResumeAt = &t
		}
		st, err := s.conductor.PauseQueue(req)
		if err != nil {
			return nil, e("PAUSE", err.Error())
		}
		return st, nil
	case "resumeQueue":
		st, err := s.conductor.ResumeQueue()
		if err != nil {
			return nil, e("RESUME", err.Error())
		}
		return st, nil
	case "extendPause":
		var in struct {
			DeltaMs int64 `json:"deltaMs"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		st, err := s.conductor.ExtendPause(in.DeltaMs)
		if err != nil {
			return nil, e("PAUSE", err.Error())
		}
		return st, nil
	case "clearPauseTimer":
		st, err := s.conductor.ClearPauseTimer()
		if err != nil {
			return nil, e("PAUSE", err.Error())
		}
		return st, nil
	case "getPauseState":
		return s.conductor.PauseState(), nil
	// Thin aliases so a mid-migration extension build keeps working.
	case "pauseScheduler":
		if err := s.conductor.PauseScheduler(); err != nil {
			return nil, e("PAUSE", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "resumeScheduler":
		if err := s.conductor.ResumeScheduler(); err != nil {
			return nil, e("RESUME", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "cancelRun":
		var in struct {
			RunID string `json:"runId"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.CancelRun(in.RunID); err != nil {
			return nil, e("CANCEL", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "retryRun":
		var in struct {
			RunID string `json:"runId"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.RetryRun(in.RunID); err != nil {
			return nil, e("RETRY", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "continueRun":
		var in struct {
			RunID   string `json:"runId"`
			Message string `json:"message"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.ContinueRun(in.RunID, in.Message); err != nil {
			return nil, e("CONTINUE", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "skipRun":
		var in struct {
			RunID string `json:"runId"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.SkipRun(in.RunID); err != nil {
			return nil, e("SKIP", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "updateRun":
		var in struct {
			RunID string                   `json:"runId"`
			Run   scheduler.UpdateRunInput `json:"run"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		if err := s.conductor.UpdateRun(in.RunID, in.Run); err != nil {
			return nil, e("UPDATE", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "markComplete":
		var in struct {
			RunID string `json:"runId"`
		}
		json.Unmarshal(params, &in)
		if err := s.store.TransitionRun(in.RunID, model.StateCompleted, &model.RunEvent{Type: model.EventRunCompleted}, func(x *model.Run) {
			x.FinalOutcome = "COMPLETE"
			x.FinishedAt = time.Now().UTC()
		}); err != nil {
			return nil, e("MARK", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "updateSettings":
		var st scheduler.Settings
		if err := json.Unmarshal(params, &st); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		if err := s.conductor.UpdateSettings(st); err != nil {
			return nil, e("SETTINGS", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "patchSettings":
		// Partial update applied server-side under the settings lock, so two
		// concurrent edits cannot lose one another.
		st, err := s.conductor.PatchSettings(params)
		if err != nil {
			return nil, e("SETTINGS", err.Error())
		}
		return st, nil
	case "getSettings":
		return s.conductor.Settings(), nil
	case "updateSecretBlock":
		var in struct {
			Text string `json:"text"`
		}
		json.Unmarshal(params, &in)
		if _, err := s.conductor.UpdateSecretBlock(in.Text); err != nil {
			return nil, e("SECRET", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "revealSecretBlock":
		text, id, err := s.conductor.RevealSecretBlock()
		if err != nil {
			return nil, e("SECRET", err.Error())
		}
		return map[string]any{"text": text, "versionId": id}, nil
	case "deleteSecretBlock":
		if err := s.conductor.DeleteSecretBlock(); err != nil {
			return nil, e("SECRET", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "trustWorkspace":
		var in struct {
			Path string `json:"path"`
		}
		json.Unmarshal(params, &in)
		if in.Path == "" {
			return nil, e("BAD_PARAMS", "path required")
		}
		adapter, ok := s.registry.Get(model.ProviderClaude)
		if !ok {
			return nil, e("TRUST", "claude adapter unavailable")
		}
		cl, ok := adapter.(*providers.Claude)
		if !ok {
			return nil, e("TRUST", "unexpected adapter")
		}
		if err := cl.TrustWorkspace(in.Path); err != nil {
			return nil, e("TRUST", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "revealEffectivePrompt":
		var in struct {
			RunID string `json:"runId"`
		}
		json.Unmarshal(params, &in)
		text, err := s.conductor.EffectivePromptForRun(in.RunID)
		if err != nil {
			return nil, e("PROMPT", err.Error())
		}
		return map[string]any{"text": text, "containsSecrets": s.conductor.RunAttachesSecrets(in.RunID)}, nil
	case "getProviderCapabilities":
		return s.capabilities(), nil
	case "getQuotaState":
		return s.quotaState(), nil
	case "clearStaleExternalSession":
		var in struct {
			Provider string `json:"provider"`
			NativeID string `json:"nativeId"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.ClearStaleExternalSession(in.Provider, in.NativeID); err != nil {
			return nil, e("CLEAR", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "applyBatch":
		var in struct {
			BatchID string `json:"batchId"`
		}
		json.Unmarshal(params, &in)
		out, err := s.conductor.ApplyBatchToBranch(in.BatchID)
		if err != nil {
			return nil, e("APPLY", err.Error())
		}
		return map[string]any{"output": out}, nil
	case "cleanupWorktrees":
		var in struct {
			BatchID string `json:"batchId"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.CleanupWorktrees(in.BatchID); err != nil {
			return nil, e("CLEANUP", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "batchReport":
		var in struct {
			BatchID string `json:"batchId"`
		}
		json.Unmarshal(params, &in)
		rep, err := s.conductor.CopyBatchReport(in.BatchID)
		if err != nil {
			return nil, e("REPORT", err.Error())
		}
		return map[string]any{"report": rep}, nil
	case "getBatchDetail":
		var in struct {
			BatchID string `json:"batchId"`
		}
		json.Unmarshal(params, &in)
		return s.batchDetail(in.BatchID)
	case "getRunEvents":
		var in struct {
			RunID string `json:"runId"`
			Limit int    `json:"limit"`
		}
		json.Unmarshal(params, &in)
		if in.Limit == 0 {
			in.Limit = 500
		}
		evs, err := s.store.ListRunEvents(in.RunID, in.Limit)
		if err != nil {
			return nil, e("EVENTS", err.Error())
		}
		return map[string]any{"events": evs}, nil
	case "hookEvent":
		// internal: hook bridge forwarding (auth via socket + token is enough)
		var p providers.HookPayload
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, e("BAD_PARAMS", err.Error())
		}
		s.hookBus.Publish(p)
		return map[string]any{"ok": true}, nil
	case "compact":
		n, err := s.store.Compact(db.DefaultRetention(), time.Now().UTC())
		if err != nil {
			return nil, e("COMPACT", err.Error())
		}
		return map[string]any{"prunedEvents": n}, nil
	case "diagnostics":
		return s.diagnostics(), nil
	case "daemonHealth":
		return map[string]any{
			"version": s.version, "pid": os.Getpid(),
			"uptime":   time.Since(s.startedAt).String(),
			"protocol": ProtocolVersion,
		}, nil
	case "goodbye":
		return map[string]any{"ok": true}, nil
	default:
		return nil, e("UNKNOWN_METHOD", "unknown method "+method)
	}
}

// snapshot builds the full UI state.
//
// Three queries regardless of batch count. It used to issue ListRunsByBatch +
// ListRows per batch — about 200 SQLite round-trips per snapshot, on a path
// the webview hit on every daemon event, every window focus and every 15s.
func (s *Server) snapshot() map[string]any {
	batches, _ := s.store.ListBatches(100)
	ids := make([]string, 0, len(batches))
	for _, b := range batches {
		ids = append(ids, b.ID)
	}
	runsByBatch, _ := s.store.ListRunsForBatches(ids)
	rows, _ := s.store.ListRowsForBatches(ids)
	var runs []*model.Run
	for _, b := range batches {
		runs = append(runs, runsByBatch[b.ID]...)
		if rows[b.ID] == nil {
			rows[b.ID] = []*model.Row{}
		}
	}
	ext := map[string][]*model.ProviderSession{}
	for _, p := range []string{model.ProviderClaude, model.ProviderCodex} {
		sessions, _ := s.store.ListProviderSessions(p)
		ext[p] = sessions
	}
	pause := s.conductor.PauseState()
	return map[string]any{
		"daemon":    map[string]any{"version": s.version, "pid": os.Getpid(), "protocol": ProtocolVersion, "uptime": time.Since(s.startedAt).String()},
		"batches":   batches,
		"runs":      runs,
		"rows":      rows,
		"external":  ext,
		"quota":     s.quotaState(),
		"providers": s.capabilities(),
		"settings":  s.conductor.Settings(),
		"pause":     pause,
		// Legacy mirror for anything still reading the flat flag.
		"paused": pause.Paused,
		// Existence only. The composer used to call revealSecretBlock on every
		// mount just to answer this, decrypting the secret to compute a bool.
		"hasSecretBlock": s.conductor.HasSecretBlock(),
		"host":           hostInfo(),
	}
}

func hostInfo() map[string]any {
	host, _ := os.Hostname()
	return map[string]any{"hostname": host, "platform": platformName()}
}

func platformName() string {
	return runtimeGOOS()
}

// capabilities returns cached capability data, refreshing synchronously only
// when the cache is cold, and kicking a background refresh when stale. Probes
// spawn provider subprocesses and must never stall snapshots.
func (s *Server) capabilities() map[string]providers.Caps {
	s.capsMu.Lock()
	fresh := time.Since(s.capsAt) < capsTTL
	cache := s.capsCache
	warm := len(cache) > 0
	s.capsMu.Unlock()
	if fresh && warm {
		return cache
	}
	if !warm {
		return s.refreshCapabilities()
	}
	go s.refreshCapabilities()
	return cache
}

func (s *Server) refreshCapabilities() map[string]providers.Caps {
	out := map[string]providers.Caps{}
	for _, a := range s.registry.All() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		caps := a.Capabilities(ctx)
		cancel()
		out[a.Name()] = caps
	}
	s.capsMu.Lock()
	s.capsCache = out
	s.capsAt = time.Now()
	s.capsMu.Unlock()
	return out
}

func (s *Server) quotaState() map[string]any {
	out := map[string]any{}
	for _, p := range []string{model.ProviderClaude, model.ProviderCodex} {
		ws := s.guardian.Windows(p)
		if ws == nil {
			ws = []providers.QuotaWindow{}
		}
		now := time.Now().UTC()
		blocked, ne, ht, missing := s.guardian.Blocked(p, now)
		st := map[string]any{
			"windows":    ws,
			"blocked":    blocked,
			"confidence": quota.Confidence(ws),
		}
		if ht {
			st["nextEligibleAt"] = ne.UTC().Format(time.RFC3339)
		}
		if missing {
			st["missingReset"] = true
		}
		rows, _ := s.store.ListQuotaWindows(p)
		if len(rows) > 0 && len(ws) == 0 {
			// persisted observations survive daemon restarts
			var loaded []providers.QuotaWindow
			for _, r := range rows {
				w := providers.QuotaWindow{ID: r.WindowID, Name: r.Name, UsedPercent: r.UsedPercent,
					HasPercent: true, Exhausted: r.Exhausted, Source: r.Source, Confidence: r.Confidence}
				if r.ResetsAt != "" {
					if t, err := time.Parse(time.RFC3339, r.ResetsAt); err == nil {
						w.ResetsAt, w.HasReset = t, true
					}
				}
				loaded = append(loaded, w)
			}
			s.guardian.Observe(p, loaded, now)
			st["windows"] = loaded
		}
		out[p] = st
	}
	return out
}

func (s *Server) batchDetail(batchID string) (any, *apiError) {
	b, err := s.store.GetBatch(batchID)
	if err != nil || b == nil {
		return nil, e("NOT_FOUND", "batch not found")
	}
	rows, _ := s.store.ListRows(batchID)
	runs, _ := s.store.ListRunsByBatch(batchID)
	wts, _ := s.store.ListWorktrees(batchID)
	events := map[string][]*model.RunEvent{}
	for _, r := range runs {
		evs, _ := s.store.ListRunEvents(r.ID, 200)
		events[r.ID] = evs
	}
	return map[string]any{"batch": b, "rows": rows, "runs": runs, "worktrees": wts, "events": events}, nil
}

func (s *Server) diagnostics() map[string]any {
	diag := map[string]any{
		"daemon": map[string]any{"version": s.version, "pid": os.Getpid(), "socket": s.paths.Socket},
		"host":   hostInfo(),
	}
	// Bounded: a hung `claude --help` or `codex app-server` must not wedge
	// diagnostics indefinitely. capabilities() carries a 15s per-probe timeout
	// and a short cache.
	diag["providers"] = s.capabilities()
	diag["quota"] = s.quotaState()
	ext := map[string][]*model.ProviderSession{}
	for _, p := range []string{model.ProviderClaude, model.ProviderCodex} {
		sessions, _ := s.store.ListProviderSessions(p)
		ext[p] = sessions
	}
	diag["external"] = ext
	diag["settings"] = s.conductor.Settings()
	return diag
}
