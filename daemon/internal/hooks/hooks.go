// Package hooks installs Agent Conductor's namespaced awareness hooks into
// provider configuration NON-destructively: structural merge, backup with
// checksums, idempotent install, surgical uninstall.
package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	// Namespace marks every entry Agent Conductor owns.
	Namespace = "agent-conductor"
	backupExt = ".agent-conductor.bak"
)

// Manager installs/uninstalls hook bridge entries.
type Manager struct {
	SelfBin string // agent-conductor binary path for hook commands
	Home    string // user home
}

// New creates the manager.
func New(selfBin, home string) *Manager { return &Manager{SelfBin: selfBin, Home: home} }

func (m *Manager) claudeSettingsPath() string {
	return filepath.Join(m.Home, ".claude", "settings.json")
}

func (m *Manager) codexConfigPath() string {
	return filepath.Join(m.Home, ".codex", "config.toml")
}

// hookCommand builds the global (awareness-only) hook command.
//
// The completion gate is NOT installed globally. The Claude adapter passes its
// own hooks with --gate via --settings for managed runs; installing the gate
// here too made Claude Code merge both and fire the bridge twice for every
// event - two block decisions and a duplicated timeline.
func hookCommand(selfBin, provider string) string {
	return fmt.Sprintf("%s hook %s", shellQuote(selfBin), provider)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// InstallResult summarizes one install operation.
type InstallResult struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	Changed  bool   `json:"changed"`
	Backup   string `json:"backup,omitempty"`
	Note     string `json:"note,omitempty"`
}

// InstallClaude merges awareness hooks into ~/.claude/settings.json.
// Events: SessionStart, UserPromptSubmit, Stop, StopFailure, Notification,
// SessionEnd. Awareness-only; the completion gate activates solely for
// managed runs via AGENT_CONDUCTOR_RUN_ID.
func (m *Manager) InstallClaude() (InstallResult, error) {
	path := m.claudeSettingsPath()
	res := InstallResult{Provider: "claude", Path: path}

	var doc map[string]any
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &doc); err != nil {
			return res, fmt.Errorf("existing %s is not valid JSON; refusing to touch it: %w", path, err)
		}
	case os.IsNotExist(err):
		doc = map[string]any{}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return res, err
		}
	default:
		return res, err
	}

	if doc["hooks"] == nil {
		doc["hooks"] = map[string]any{}
	}
	hooksDoc, ok := doc["hooks"].(map[string]any)
	if !ok {
		if doc["hooks"] != nil {
			return res, fmt.Errorf("%s has a non-object hooks key; refusing to overwrite", path)
		}
		hooksDoc = map[string]any{}
		doc["hooks"] = hooksDoc
	}

	cmd := hookCommand(m.SelfBin, "claude")
	before, _ := json.Marshal(doc)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Notification", "SessionEnd"} {
		list, _ := hooksDoc[ev].([]any)
		if hasNamespacedClaudeEntry(list, cmd) {
			continue
		}
		list = append(list, map[string]any{
			Namespace: true,
			"hooks": []any{map[string]any{
				"type": "command", "command": cmd, "timeout": 10,
			}},
		})
		hooksDoc[ev] = list
	}
	after, _ := json.Marshal(doc)
	if string(before) == string(after) {
		res.Note = "already installed"
		return res, nil
	}

	if len(data) > 0 {
		backup, err := m.backup(path, data)
		if err != nil {
			return res, err
		}
		res.Backup = backup
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// hasNamespacedClaudeEntry detects our entry in a Claude hooks event list.
// Claude matcher groups carry {matcher?, hooks:[...]}. We tag ours with the
// extra "agent-conductor": true key so uninstall is surgical.
func hasNamespacedClaudeEntry(list []any, cmd string) bool {
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if _, marked := m[Namespace]; !marked {
			continue
		}
		hs, ok := m["hooks"].([]any)
		if !ok {
			return true
		}
		for _, h := range hs {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if c, _ := hm["command"].(string); strings.Contains(c, "hook claude") || strings.Contains(c, cmd) {
				return true
			}
		}
	}
	return false
}

// UninstallClaude removes only Agent Conductor entries.
func (m *Manager) UninstallClaude() (InstallResult, error) {
	path := m.claudeSettingsPath()
	res := InstallResult{Provider: "claude", Path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		res.Note = "no settings file"
		return res, nil
	}
	if err != nil {
		return res, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return res, err
	}
	hooksDoc, ok := doc["hooks"].(map[string]any)
	if !ok {
		res.Note = "no hooks section"
		return res, nil
	}
	removed := false
	for ev, listAny := range hooksDoc {
		list, ok := listAny.([]any)
		if !ok {
			continue
		}
		kept := []any{}
		for _, e := range list {
			if m2, ok := e.(map[string]any); ok {
				if _, marked := m2[Namespace]; marked {
					removed = true
					continue
				}
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(hooksDoc, ev)
		} else {
			hooksDoc[ev] = kept
		}
	}
	if !removed {
		res.Note = "nothing to remove"
		return res, nil
	}
	backup, err := m.backup(path, data)
	if err != nil {
		return res, err
	}
	res.Backup = backup
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// ---- Codex (config.toml) ----

// Codex hook entries are written as a single delimited block appended to
// config.toml. Everything outside the block is preserved byte for byte.
//
// The previous implementation decoded the whole file into map[string]any and
// re-encoded it with a hand-rolled writer. That destroyed every comment and
// all formatting, and its fallback branch emitted fmt.Sprintf("%v") - so a
// TOML datetime came back as an unquoted, syntactically invalid token. The
// function was documented as NON-destructive; it was the opposite.
const (
	codexBlockStart = "# >>> agent-conductor (managed) >>>"
	codexBlockEnd   = "# <<< agent-conductor (managed) <<<"
)

// codexHookEvents are the events we ask Codex to notify us about.
var codexHookEvents = []string{"SessionStart", "UserPromptSubmit", "Stop", "Interrupt", "SessionEnd"}

// InstallCodex appends Agent Conductor's awareness hooks to ~/.codex/config.toml.
//
// NOTE: the [hooks.*] surface has not been verified against an installed
// Codex build (see docs/provider-capabilities.md). The block is delimited and
// removable precisely so that writing it is harmless if Codex ignores it.
func (m *Manager) InstallCodex() (InstallResult, error) {
	path := m.codexConfigPath()
	res := InstallResult{Provider: "codex", Path: path}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return res, err
		}
		raw = nil
	} else if err != nil {
		return res, err
	}
	original := string(raw)

	if strings.Contains(original, codexBlockStart) {
		res.Note = "already installed"
		return res, nil
	}
	// Read-only parse: we never write this structure back, we only use it to
	// find events we cannot extend without producing invalid TOML.
	events := append([]string(nil), codexHookEvents...)
	if len(raw) > 0 {
		var doc map[string]any
		if _, err := toml.Decode(original, &doc); err != nil {
			return res, fmt.Errorf("existing %s is not valid TOML; refusing to touch it: %w", path, err)
		}
		if hooksTbl, ok := doc["hooks"].(map[string]any); ok {
			var usable []string
			var skipped []string
			for _, ev := range events {
				switch hooksTbl[ev].(type) {
				case nil, []map[string]any, []any:
					usable = append(usable, ev) // array of tables: extendable
				default:
					// The user defined [hooks.X] as a plain table. Appending
					// [[hooks.X]] would be invalid TOML, so leave theirs alone.
					skipped = append(skipped, ev)
				}
			}
			events = usable
			if len(skipped) > 0 {
				res.Note = "left your existing [hooks." + strings.Join(skipped, "], [hooks.") + "] untouched"
			}
		}
	}
	if len(events) == 0 {
		res.Note = "every hook event is already defined by you; nothing added"
		return res, nil
	}

	block := m.codexBlock(events)
	next := original
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += block

	// Refuse to write anything that would not parse.
	var check map[string]any
	if _, err := toml.Decode(next, &check); err != nil {
		return res, fmt.Errorf("refusing to write %s: the result would not be valid TOML (%w)", path, err)
	}

	if len(raw) > 0 {
		backup, err := m.backup(path, raw)
		if err != nil {
			return res, err
		}
		res.Backup = backup
	}
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

func (m *Manager) codexBlock(events []string) string {
	cmd := hookCommand(m.SelfBin, "codex")
	var b strings.Builder
	b.WriteString("\n" + codexBlockStart + "\n")
	b.WriteString("# Awareness hooks for Agent Conductor. Safe to delete by hand;\n")
	b.WriteString("# `agent-conductor uninstall-service` removes exactly this block.\n")
	for _, ev := range events {
		fmt.Fprintf(&b, "[[hooks.%s]]\n", ev)
		b.WriteString("agent_conductor = true\n")
		b.WriteString("type = \"command\"\n")
		fmt.Fprintf(&b, "command = %s\n", tomlString(cmd))
		b.WriteString("timeout = 10\n")
	}
	b.WriteString(codexBlockEnd + "\n")
	return b.String()
}

// tomlString renders a TOML basic string with the escapes TOML requires.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// UninstallCodex removes exactly the delimited block, leaving the rest of the
// file byte-identical.
func (m *Manager) UninstallCodex() (InstallResult, error) {
	path := m.codexConfigPath()
	res := InstallResult{Provider: "codex", Path: path}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		res.Note = "no config file"
		return res, nil
	}
	if err != nil {
		return res, err
	}
	original := string(raw)
	next, removed := stripCodexBlock(original)
	if !removed {
		res.Note = "nothing to remove"
		return res, nil
	}
	var check map[string]any
	if _, err := toml.Decode(next, &check); err != nil {
		return res, fmt.Errorf("refusing to write %s: removing the block would leave invalid TOML (%w)", path, err)
	}
	backup, err := m.backup(path, raw)
	if err != nil {
		return res, err
	}
	res.Backup = backup
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// stripCodexBlock removes every managed block, preserving all other bytes.
func stripCodexBlock(content string) (string, bool) {
	removed := false
	for {
		i := strings.Index(content, codexBlockStart)
		if i < 0 {
			break
		}
		j := strings.Index(content[i:], codexBlockEnd)
		if j < 0 {
			break // unterminated: leave it alone rather than guess
		}
		end := i + j + len(codexBlockEnd)
		if end < len(content) && content[end] == '\n' {
			end++
		}
		// Drop the blank line we added before the block.
		for i > 0 && content[i-1] == '\n' && (i < 2 || content[i-2] == '\n') {
			i--
		}
		content = content[:i] + content[end:]
		removed = true
	}
	return content, removed
}

// backup writes path.bak with timestamp suffix and returns the backup path.
func (m *Manager) backup(path string, data []byte) (string, error) {
	bak := fmt.Sprintf("%s.%s%s", path, time.Now().UTC().Format("20060102T150405Z"), backupExt)
	if err := os.WriteFile(bak, data, 0o600); err != nil {
		return "", err
	}
	pruneBackups(path, 5)
	return bak, nil
}

// pruneBackups keeps only the newest keep backups. They used to accumulate
// forever, one per install/uninstall.
func pruneBackups(path string, keep int) {
	matches, err := filepath.Glob(path + ".*" + backupExt)
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches) // timestamped names sort chronologically
	for _, old := range matches[:len(matches)-keep] {
		os.Remove(old)
	}
}
