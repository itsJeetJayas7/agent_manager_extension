package main

import (
	"agent-conductor/internal/claudewrap"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Argument shapes observed from Claude Code 2.1.288 launching through
// claudeCode.claudeProcessWrapper.
func TestWrapperFindsResumedSession(t *testing.T) {
	cases := map[string][]string{
		"abc": {"--output-format", "stream-json", "--resume=abc", "--verbose"},
		"def": {"--resume", "def", "--model", "x"},
		"ghi": {"-r", "ghi"},
		"":    {"auth", "status", "--json"},
	}
	for want, args := range cases {
		if got := resumeSessionID(args); got != want {
			t.Fatalf("%v: got %q want %q", args, got, want)
		}
	}
}

func TestWrapperSeparatesBundledBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	got, rest := splitWrapperArgs([]string{bin, "--resume=x"})
	if got != bin || len(rest) != 1 || rest[0] != "--resume=x" {
		t.Fatalf("bundled binary not separated: %q %v", got, rest)
	}
	_, rest = splitWrapperArgs([]string{"--resume=x"})
	if len(rest) != 1 {
		t.Fatalf("arguments without a bundled binary must pass unchanged: %v", rest)
	}
}

// If Agent Conductor is uninstalled, or only an older binary without the
// wrapper is left, Claude Code must still start every conversation.
func TestWrapperScriptDegradesToPlainClaude(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old-agent-conductor")
	os.WriteFile(old, []byte("#!/bin/sh\necho 'unknown command' >&2\nexit 1\n"), 0o755)
	claude := filepath.Join(dir, "claude")
	os.WriteFile(claude, []byte("#!/bin/sh\necho \"claude $*\"\n"), 0o755)
	script := filepath.Join(dir, "wrapper")
	os.WriteFile(script, []byte(claudewrap.Script(dir, []string{filepath.Join(dir, "missing"), old})), 0o755)
	out, err := exec.Command(script, claude, "--resume=abc").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "claude --resume=abc" {
		t.Fatalf("wrapper did not fall back to Claude: %v %q", err, out)
	}
}
