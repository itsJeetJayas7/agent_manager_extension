// Package hooksbridge implements the provider hook subcommand: read the
// provider JSON from stdin, forward an observation to the daemon socket with
// a tiny timeout, and apply the managed-run completion gate when required.
// The bridge NEVER stalls the provider: daemon unavailable => exit 0.
package hooksbridge

import (
	"bufio"
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
func Run(provider string) int {
	data, err := readAllStdin()
	if err != nil {
		return 0
	}
	payload := parseProviderJSON(provider, data)
	payload.Provider = provider

	runID := os.Getenv("AGENT_CONDUCTOR_RUN_ID")
	payload.RunID = runID

	// Completion gate: managed runs only, Stop event, no result marker,
	// not already continuing from a stop hook.
	if provider == model.ProviderClaude && payload.Event == "Stop" && runID != "" && !payload.StopHookActive {
		if outcome, _ := model.ParseResultMarker(payload.LastAssistantMessage); outcome == "" {
			if !model.HasStageMarker(payload.LastAssistantMessage) {
				emitBlockDecision()
				return 0
			}
		}
	}

	if err := forward(payload); err != nil {
		// awareness-only: silently succeed to never stall the provider
		return 0
	}
	return 0
}

func emitBlockDecision() {
	out := map[string]any{
		"decision": "block",
		"reason": "Before ending this managed run, finish any remaining work. Then end your final response with exactly one Agent Conductor result marker:\n<AC_RESULT>COMPLETE</AC_RESULT>\nor\n<AC_RESULT>BLOCKED: precise reason</AC_RESULT>",
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
