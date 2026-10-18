// Package claudewrap installs the executable Agent Conductor sets as the
// Claude Code extension's claudeCode.claudeProcessWrapper.
package claudewrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Name is the installed wrapper script's file name.
const Name = "agent-conductor-claude-wrapper"

// Install writes <root>/bin/agent-conductor-claude-wrapper, the
// executable the claudeCode.claudeProcessWrapper setting names. It is a shell
// script rather than a link so that it degrades safely: if Agent Conductor's
// binary is gone, Claude Code still starts every conversation normally.
func Install(root string) (string, error) {
	dir := filepath.Join(root, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, Name)
	body := Script(root, wrapperBinaries())
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return path, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// Script is the wrapper body for a daemon root and candidate binaries.
func Script(r string, bins []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Agent Conductor: lets the Claude Code extension show managed runs live.\n")
	b.WriteString("# Set as claudeCode.claudeProcessWrapper. Remove the setting to stop using it.\n")
	for _, bin := range bins {
		q := shQuote(bin)
		fmt.Fprintf(&b, "if [ -x %s ] && %s claude-wrapper --probe >/dev/null 2>&1; then exec %s claude-wrapper --root %s \"$@\"; fi\n", q, q, q, shQuote(r))
	}
	b.WriteString("# Agent Conductor is not installed: start Claude unchanged.\n")
	b.WriteString("case \"$1\" in /*) [ -x \"$1\" ] && exec \"$@\" ;; esac\n")
	b.WriteString("exec claude \"$@\"\n")
	return b.String()
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// wrapperBinaries lists the daemon binaries the script tries: this daemon's
// own binary first, then the stable installed one in case an extension update
// removed it. The script probes each, since an older binary lacks the wrapper.
func wrapperBinaries() []string {
	home, _ := os.UserHomeDir()
	out := []string{}
	if self, err := os.Executable(); err == nil {
		out = append(out, self)
	}
	stable := filepath.Join(home, ".local", "bin", "agent-conductor")
	if _, err := os.Stat(stable); err == nil && (len(out) == 0 || out[0] != stable) {
		out = append(out, stable)
	}
	return out
}

// Refresh rewrites an existing wrapper after an upgrade. It never creates
// one: enabling it is the user's decision.
func Refresh(root string) {
	if _, err := os.Stat(filepath.Join(root, "bin", Name)); err == nil {
		_, _ = Install(root)
	}
}
