package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agent-conductor/internal/providers"
)

// The Claude Code VS Code extension starts every conversation's process as
// `<claudeCode.claudeProcessWrapper> <bundled claude> <args...>` when that
// setting is present (documented at code.claude.com/docs/en/vs-code). Agent
// Conductor installs this wrapper so that opening a managed run's session
// attaches the extension's tab to the daemon-owned process instead of starting
// a second `claude --resume` against the same conversation. Every other launch
// is replaced, unchanged, by the real binary.

// splitWrapperArgs separates the binary the extension passed from Claude's
// own arguments. The extension passes its bundled binary first when it has
// one; otherwise the arguments start directly.
func splitWrapperArgs(args []string) (bin string, rest []string) {
	if len(args) > 0 && filepath.IsAbs(args[0]) && !strings.HasPrefix(args[0], "-") {
		if st, err := os.Stat(args[0]); err == nil && !st.IsDir() {
			return args[0], args[1:]
		}
	}
	return providers.FindRuntime("claude"), args
}

// resumeSessionID returns the conversation the extension asked to resume.
func resumeSessionID(args []string) string {
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--resume="):
			return strings.TrimPrefix(a, "--resume=")
		case a == "--resume" || a == "-r":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
		}
	}
	return ""
}

func wrapperLog(r, format string, a ...any) {
	f, err := os.OpenFile(filepath.Join(r, "logs", "claude-wrapper.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s pid=%d %s\n", time.Now().UTC().Format(time.RFC3339Nano), os.Getpid(), fmt.Sprintf(format, a...))
}

// runClaudeWrapper handles `agent-conductor claude-wrapper [--root <dir>]
// <bundled claude> <args...>`, as invoked by the installed wrapper script.
func runClaudeWrapper(args []string) int {
	if len(args) == 1 && args[0] == "--probe" {
		fmt.Println("ok") // the wrapper script checks for this before relying on a binary
		return 0
	}
	r := root()
	if len(args) >= 2 && args[0] == "--root" {
		r, args = args[1], args[2:]
	}
	bin, rest := splitWrapperArgs(args)
	if sid := resumeSessionID(rest); sid != "" {
		conn, rd, res, err := attachViaDaemon(r, sid)
		switch {
		case err != nil:
			wrapperLog(r, "session=%s passthrough (daemon unavailable: %v)", sid, err)
		case res.Attached:
			wrapperLog(r, "session=%s attached run=%s pid=%d", sid, res.RunID, res.PID)
			return relay(conn, rd)
		case res.Reason == "held":
			// Starting Claude here would put a second process on a conversation
			// Agent Conductor manages; the extension skips its own check for
			// wrapped launches.
			wrapperLog(r, "session=%s refused (held by pid %d)", sid, res.HolderPID)
			fmt.Fprintf(os.Stderr, "This Agent Conductor conversation is still open in another Claude process (pid %d). Close it there, then open it here again.\n", res.HolderPID)
			return 1
		default:
			wrapperLog(r, "session=%s passthrough (%s)", sid, res.Reason)
		}
	}
	if bin == "" {
		fmt.Fprintln(os.Stderr, "agent-conductor: claude executable not found")
		return 127
	}
	err := syscall.Exec(bin, append([]string{bin}, rest...), os.Environ())
	fmt.Fprintf(os.Stderr, "agent-conductor: exec %s: %v\n", bin, err)
	return 127
}

// attachViaDaemon asks the daemon to take over this launch.
func attachViaDaemon(r, sessionID string) (net.Conn, *bufio.Reader, providers.AttachResult, error) {
	var res providers.AttachResult
	tok, err := os.ReadFile(filepath.Join(r, "run", "conductor.token"))
	if err != nil {
		return nil, nil, res, err
	}
	conn, err := net.DialTimeout("unix", filepath.Join(r, "run", "conductor.sock"), 2*time.Second)
	if err != nil {
		return nil, nil, res, err
	}
	rd := bufio.NewReader(conn)
	call := func(id, method string, params any, timeout time.Duration, out any) error {
		b, _ := json.Marshal(map[string]any{"protocol": 1, "id": id, "method": method, "params": params})
		_ = conn.SetDeadline(time.Now().Add(timeout))
		if _, err := conn.Write(append(b, '\n')); err != nil {
			return err
		}
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return err
		}
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
	if err := call("1", "hello", map[string]string{"auth": strings.TrimSpace(string(tok))}, 5*time.Second, nil); err != nil {
		conn.Close()
		return nil, nil, res, err
	}
	// Starting a process for an idle managed session includes Claude's own
	// startup, so this can take a while.
	if err := call("2", "attachClaudeSession", map[string]string{"sessionId": sessionID}, 90*time.Second, &res); err != nil {
		conn.Close()
		return nil, nil, res, err
	}
	_ = conn.SetDeadline(time.Time{})
	if !res.Attached {
		conn.Close()
	}
	return conn, rd, res, nil
}

// relay connects the extension's pipes to the daemon until the daemon ends
// the session or the extension closes the tab. Closing the tab only detaches:
// the run keeps executing in the daemon.
func relay(conn net.Conn, rd *bufio.Reader) int {
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	_, _ = io.Copy(os.Stdout, rd)
	conn.Close()
	return 0
}
