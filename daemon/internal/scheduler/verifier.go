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

// Verify executes commands; returns aggregated pass/fail with output.
func (ShellVerifier) Verify(ctx context.Context, cwd string, commands []string) (bool, string) {
	var sb strings.Builder
	okAll := true
	for _, cmdLine := range commands {
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdLine)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		pass := err == nil
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
	if len(s) > 8000 {
		return s[:8000] + "\n…truncated"
	}
	return s
}

func userHomeDir() string { home, _ := os.UserHomeDir(); return home }
