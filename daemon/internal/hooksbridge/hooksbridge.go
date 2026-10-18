// Package hooksbridge implements the provider hook subcommand: read the
// provider JSON from stdin, forward an observation to the daemon socket with
// a tiny timeout, and apply the managed-run completion gate when required.
// The bridge NEVER stalls the provider: daemon unavailable => exit 0.
package hooksbridge

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-conductor/internal/model"
	"agent-conductor/internal/providers"
)

// Run executes the hook bridge for one provider event.
//
// args are the flags after the provider name. `--gate` enables the managed
// completion gate; without it the bridge is awareness-only. The gate is
// installed solely via the adapter's --settings for managed runs, so the
// global ~/.claude/settings.json entry never blocks anything.
func Run(provider string, args ...string) int {
	gate := false
	for _, a := range args {
		if a == "--gate" || a == "-gate" {
			gate = true
		}
	}
	data, err := readAllStdin()
	if err != nil {
		return 0
	}
	payload := parseProviderJSON(provider, data)
	payload.Provider = provider

	runID := os.Getenv("AGENT_CONDUCTOR_RUN_ID")
	payload.RunID = runID

	// Claude Code merges global settings with --settings, so the same event can
	// reach the bridge twice. Forwarding both would duplicate every timeline
	// entry and fire two block decisions.
	// External awareness is idempotent and uses only the global hook. Claude
	// may omit turn_id, so deduplicating it can swallow a genuinely new prompt
	// or stop in the same session within the ten-second window.
	duplicate := runID != "" && alreadySeen(payload)

	// Forward FIRST. The gate used to `return 0` before forwarding, so the
	// daemon never learned that the agent had tried to stop: the adapter's
	// stopped flag and the timeline both missed it.
	if !duplicate {
		// Never stall the provider: a missing daemon is not an error here.
		_ = forward(payload)
	}

	// Completion gate: managed runs only, Stop event, no result marker,
	// not already continuing from a stop hook.
	if gate && provider == model.ProviderClaude && payload.Event == "Stop" && runID != "" && !payload.StopHookActive {
		if outcome, _ := model.ParseResultMarker(payload.LastAssistantMessage); outcome == "" {
			if !model.HasStageMarker(payload.LastAssistantMessage) {
				emitBlockDecision()
			}
		}
	}
	return 0
}

// dedupWindow is how long an identical (provider, event, session, turn,
// message) tuple is treated as the same delivery.
const dedupWindow = 10 * time.Second

// alreadySeen records this delivery and reports whether an identical one
// arrived moments ago. The marker file is created with O_EXCL, so of two
// concurrent bridge processes exactly one wins.
func alreadySeen(p providers.HookPayload) bool {
	key := sha256.Sum256([]byte(strings.Join([]string{
		p.Provider, p.Event, p.SessionID, p.ThreadID, p.TurnID, p.RunID,
		fmt.Sprintf("%t", p.StopHookActive), p.LastAssistantMessage,
	}, "\x00")))
	name := hex.EncodeToString(key[:12])

	sock, _ := resolveDaemonPaths()
	dir := filepath.Join(filepath.Dir(sock), "hookdedup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false // cannot dedup: better a duplicate than a lost event
	}
	pruneDedup(dir)
	marker := filepath.Join(dir, name)
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		f.Close()
		return false
	}
	if !os.IsExist(err) {
		return false
	}
	st, statErr := os.Stat(marker)
	if statErr != nil || time.Since(st.ModTime()) > dedupWindow {
		// Stale marker from an earlier, genuinely different delivery.
		os.Chtimes(marker, time.Now(), time.Now())
		return false
	}
	return true
}

func pruneDedup(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > 5*time.Minute {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func emitBlockDecision() {
	out := map[string]any{
		"decision": "block",
		"reason":   "Before ending this managed run, finish any remaining work. Then end your final response with exactly one Agent Conductor result marker:\n<AC_RESULT>COMPLETE</AC_RESULT>\nor\n<AC_RESULT>BLOCKED: precise reason</AC_RESULT>",
	}
	b, _ := json.Marshal(out)
	fmt.Print(string(b))
}

func readAllStdin() ([]byte, error) {
	r := bufio.NewReaderSize(os.Stdin, 8*1024*1024)
	var sb strings.Builder
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
		if sb.Len() > 8*1024*1024 {
			break
		}
	}
	if sb.Len() == 0 {
		return nil, fmt.Errorf("empty stdin")
	}
	return []byte(sb.String()), nil
}

// parseProviderJSON extracts the common fields across providers, tolerating
// schema variation. Unknown shapes are forwarded with raw preserved.
func parseProviderJSON(provider string, data []byte) providers.HookPayload {
	var p providers.HookPayload
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return p
	}
	p.Raw = raw
	str := func(k string) string {
		if v, ok := raw[k].(string); ok {
			return v
		}
		return ""
	}
	strOf := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	p.Event = str("hook_event_name")
	if p.Event == "" {
		p.Event = str("event")
		if p.Event == "" && provider == model.ProviderCodex {
			// codex hooks may deliver {"type": "..."}
			p.Event = str("type")
		}
	}
	p.SessionID = firstNonEmpty(str("session_id"), str("sessionId"), str("session"), str("threadId"), str("thread_id"))
	p.ThreadID = firstNonEmpty(str("thread_id"), str("threadId"))
	p.TurnID = firstNonEmpty(str("turn_id"), str("turnId"))
	p.Cwd = firstNonEmpty(str("cwd"), str("current_dir"))
	p.Model = str("model")
	if v, ok := raw["stop_hook_active"].(bool); ok {
		p.StopHookActive = v
	}
	p.LastAssistantMessage = firstNonEmpty(str("last_assistant_message"), str("lastAssistantMessage"))
	// error shapes across versions: "error": "rate_limit" | {"type":...} |
	// {"message":...} | {"error": {...}} plus "error_details"
	if e, ok := raw["error"].(map[string]any); ok {
		p.ErrorType = firstNonEmpty(
			strOf(e["type"]), strOf(e["error_type"]), strOf(e["name"]),
		)
		p.ErrorMessage = firstNonEmpty(strOf(e["message"]), strOf(e["detail"]), strOf(e["details"]))
	} else if es, ok := raw["error"].(string); ok {
		p.ErrorType = es
		p.ErrorMessage = es
	}
	if d, ok := raw["error_details"].(map[string]any); ok {
		p.ErrorMessage = firstNonEmpty(p.ErrorMessage, strOf(d["message"]), strOf(d["detail"]))
	}
	if p.ErrorType == "" {
		p.ErrorType = str("error_type")
	}
	p.NotificationType = firstNonEmpty(str("notification_type"), str("notificationType"))
	p.NotificationMessage = firstNonEmpty(str("message"), str("notification_message"))
	return p
}

func resolveDaemonPaths() (sock, token string) {
	if s := os.Getenv("AC_SOCKET"); s != "" {
		return s, filepath.Join(filepath.Dir(s), "conductor.token")
	}
	root := os.Getenv("AC_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			root = filepath.Join(home, ".agent-conductor")
		}
	}
	return filepath.Join(root, "run", "conductor.sock"), filepath.Join(root, "run", "conductor.token")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// forward sends the payload to the daemon socket with a hard 2s timeout.
// Socket/token resolution: AC_SOCKET env, then AC_ROOT, then the default
// ~/.agent-conductor location.
func forward(p providers.HookPayload) error {
	sock, tokenPath := resolveDaemonPaths()
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	tokenB, err := os.ReadFile(tokenPath)
	if err != nil {
		return err
	}

	params, _ := json.Marshal(p)
	hello := fmt.Sprintf(`{"protocol":1,"id":"h","method":"hello","auth":%q}`+"\n", strings.TrimSpace(string(tokenB)))
	req := fmt.Sprintf(`{"protocol":1,"id":"e","method":"hookEvent","params":%s}`+"\n", params)
	if _, err := conn.Write([]byte(hello + req)); err != nil {
		return err
	}
	// read responses until we see the hookEvent reply or timeout
	buf := make([]byte, 4096)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := conn.Read(buf)
		if n > 0 {
			if strings.Contains(string(buf[:n]), `"id":"e"`) {
				return nil
			}
		}
		if err != nil {
			return nil // server closed early; delivery likely fine
		}
	}
	return nil
}
