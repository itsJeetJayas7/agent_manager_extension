package scheduler

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ShellVerifier runs configured commands sequentially in the run's worktree.
type ShellVerifier struct{}

// verifierEnvDeny lists variables a user-supplied verification command has no
// business seeing: provider credentials and the daemon's own state paths (the
// vault key lives under AC_ROOT).
var verifierEnvDeny = []string{
	"ANTHROPIC_", "CLAUDE_CODE_", "OPENAI_", "AWS_", "GOOGLE_", "GCLOUD_", "AZURE_",
	"AC_ROOT", "AC_SOCKET", "AC_MOCK", "AGENT_CONDUCTOR_",
}

// sanitizedVerifierEnv strips credentials before running `sh -c <user string>`.
func sanitizedVerifierEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		blocked := false
		for _, p := range verifierEnvDeny {
			if strings.HasPrefix(name, p) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, kv)
		}
	}
	return out
}

// Verify executes commands; returns aggregated pass/fail with output.
func (ShellVerifier) Verify(ctx context.Context, cwd string, commands []string) (bool, string) {
	var sb strings.Builder
	okAll := true
	env := sanitizedVerifierEnv()
	for _, cmdLine := range commands {
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdLine)
		cmd.Dir = cwd
		cmd.Env = env
		cmd.Stdin = nil
		out, err := cmd.CombinedOutput()
		pass := err == nil
		if ctx.Err() != nil {
			sb.WriteString(fmt.Sprintf("$ %s → TIMEOUT\n", cmdLine))
			return false, sb.String()
		}
		if !pass {
			okAll = false
		}
		status := "PASS"
		if !pass {
			status = "FAIL"
		}
		sb.WriteString(fmt.Sprintf("$ %s → %s\n%s\n", cmdLine, status, truncateOut(string(out))))
	}
	return okAll, sb.String()
}

func truncateOut(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 8000 {
		return string(r[:8000]) + "\n…truncated"
	}
	return s
}

func userHomeDir() string { home, _ := os.UserHomeDir(); return home }
