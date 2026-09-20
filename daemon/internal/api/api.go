// Package api implements the daemon's Unix-socket NDJSON protocol with
// machine-local token auth. Protocol version 1.
package api

import (
	"bufio"
	"context"
	"crypto/rand"
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
	Root     string
	Socket   string
	Token    string
	DB       string
	Key      string
	Logs     string
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

	capsMu      sync.Mutex
	capsCache   map[string]providers.Caps
	capsAt      time.Time
}

const capsTTL = 30 * time.Second

type clientConn struct {
	conn net.Conn
	out  chan any
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
		c.Close()
		close(cc.out)
	}
	os.Remove(s.paths.Socket)
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	out := make(chan any, 256)

	s.mu.Lock()
	s.clients[conn] = &clientConn{conn: conn, out: out}
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
		for msg := range out {
			writeMsg(msg)
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
			ID        json.RawMessage `json:"id"`
			Method    string          `json:"method"`
			Auth      string          `json:"auth"`
			Params    json.RawMessage `json:"params"`
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
			if auth != want {
				s.reply(writeMsg, req.ID, nil, map[string]string{"code": "UNAUTHORIZED", "message": "invalid token"})
				return
			}
			authed = true
			s.reply(writeMsg, req.ID, map[string]any{"protocol": ProtocolVersion, "version": s.version, "pid": os.Getpid()}, nil)
			continue
		}
		if req.Method == "subscribe" {
			ch := s.conductor.Subscribe()
			go func() {
				for ev := range ch {
					s.send(out, map[string]any{"event": ev.Type, "payload": ev.Payload})
				}
			}()
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

func (s *Server) send(out chan any, msg any) {
	select {
	case out <- msg:
	default:
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
			RunID        string `json:"runId"`
			BeforeRunID  string `json:"beforeRunId"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.ReorderQueue(in.RunID, in.BeforeRunID); err != nil {
			return nil, e("REORDER", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "pauseScheduler":
		if err := s.conductor.PauseScheduler(); err != nil {
			return nil, e("PAUSE", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "resumeScheduler":
		if err := s.conductor.ResumeScheduler(); err != nil {
			return nil, e("RESUME", err.Error())
		}
		return map[string]any{"ok": any(true)}, nil
	case "cancelRun":
		var in struct{ RunID string `json:"runId"` }
		json.Unmarshal(params, &in)
		if err := s.conductor.CancelRun(in.RunID); err != nil {
			return nil, e("CANCEL", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "retryRun":
		var in struct{ RunID string `json:"runId"` }
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
		var in struct{ RunID string `json:"runId"` }
		json.Unmarshal(params, &in)
		if err := s.conductor.SkipRun(in.RunID); err != nil {
			return nil, e("SKIP", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "updateRun":
		var in struct {
			RunID string `json:"runId"`
			Run   scheduler.EnqueueRunInput `json:"run"`
		}
		json.Unmarshal(params, &in)
		if err := s.conductor.UpdateRun(in.RunID, in.Run); err != nil {
			return nil, e("UPDATE", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "markComplete":
		var in struct{ RunID string `json:"runId"` }
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
	case "getSettings":
		return s.conductor.Settings(), nil
	case "updateSecretBlock":
		var in struct{ Text string `json:"text"` }
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
		var in struct{ Path string `json:"path"` }
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
		var in struct{ RunID string `json:"runId"` }
		json.Unmarshal(params, &in)
		text, err := s.conductor.EffectivePromptForRun(in.RunID)
		if err != nil {
			return nil, e("PROMPT", err.Error())
		}
		return map[string]any{"text": text, "containsSecrets": strings.Contains(text, "SHARED TESTING / SUDO BLOCK")}, nil
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
		var in struct{ BatchID string `json:"batchId"` }
		json.Unmarshal(params, &in)
		out, err := s.conductor.ApplyBatchToBranch(in.BatchID)
		if err != nil {
			return nil, e("APPLY", err.Error())
		}
		return map[string]any{"output": out}, nil
	case "cleanupWorktrees":
		var in struct{ BatchID string `json:"batchId"` }
		json.Unmarshal(params, &in)
		if err := s.conductor.CleanupWorktrees(in.BatchID); err != nil {
			return nil, e("CLEANUP", err.Error())
		}
		return map[string]any{"ok": true}, nil
	case "batchReport":
		var in struct{ BatchID string `json:"batchId"` }
		json.Unmarshal(params, &in)
		rep, err := s.conductor.CopyBatchReport(in.BatchID)
		if err != nil {
			return nil, e("REPORT", err.Error())
		}
		return map[string]any{"report": rep}, nil
	case "getBatchDetail":
		var in struct{ BatchID string `json:"batchId"` }
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
	case "diagnostics":
		return s.diagnostics(), nil
	case "daemonHealth":
		return map[string]any{
			"version": s.version, "pid": os.Getpid(),
			"uptime": time.Since(s.startedAt).String(),
			"protocol": ProtocolVersion,
		}, nil
	case "goodbye":
		return map[string]any{"ok": true}, nil
	default:
		return nil, e("UNKNOWN_METHOD", "unknown method "+method)
	}
}

// snapshot builds the full UI state.
func (s *Server) snapshot() map[string]any {
	batches, _ := s.store.ListBatches(100)
	type runOut struct {
		*model.Run
		Events int64 `json:"eventCount"`
	}
	var runs []*model.Run
	for _, b := range batches {
		brs, _ := s.store.ListRunsByBatch(b.ID)
		runs = append(runs, brs...)
	}
	rows := map[string][]*model.Row{}
	for _, b := range batches {
		rs, _ := s.store.ListRows(b.ID)
		rows[b.ID] = rs
	}
	ext := map[string][]*model.ProviderSession{}
	for _, p := range []string{model.ProviderClaude, model.ProviderCodex} {
		sessions, _ := s.store.ListProviderSessions(p)
		ext[p] = sessions
	}
	return map[string]any{
		"daemon":     map[string]any{"version": s.version, "pid": os.Getpid(), "protocol": ProtocolVersion, "uptime": time.Since(s.startedAt).String()},
		"batches":    batches,
		"runs":       runs,
		"rows":       rows,
		"external":   ext,
		"quota":      s.quotaState(),
		"providers":  s.capabilities(),
		"settings":   s.conductor.Settings(),
		"paused":     s.conductor.Settings().Paused,
		"host":       hostInfo(),
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
			"windows":       ws,
			"blocked":       blocked,
			"confidence":    quota.Confidence(ws),
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
		"daemon":  map[string]any{"version": s.version, "pid": os.Getpid(), "socket": s.paths.Socket},
		"host":    hostInfo(),
	}
	caps := map[string]providers.Caps{}
	for _, a := range s.registry.All() {
		caps[a.Name()] = a.Capabilities(contextBG())
	}
	diag["providers"] = caps
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
