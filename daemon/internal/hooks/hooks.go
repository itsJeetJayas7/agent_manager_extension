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

	if !changed(doc) {
		if doc["hooks"] == nil {
			doc["hooks"] = map[string]any{}
		}
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

func changed(map[string]any) bool { return false }

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

// codexHookConfig is the TOML shape merged into [hooks.*] tables.
type codexHookConfig map[string]map[string]any

// InstallCodex merges awareness hooks into ~/.codex/config.toml.
// Entries are marked with agent_conductor = true for surgical removal.
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

	var doc map[string]any
	if len(raw) > 0 {
		if _, err := toml.Decode(string(raw), &doc); err != nil {
			return res, fmt.Errorf("existing %s is not valid TOML; refusing to touch it: %w", path, err)
		}
	}
	// deep-copy check: build with our table
	cmd := hookCommand(m.SelfBin, "codex")
	hooksTbl, _ := doc["hooks"].(map[string]any)
	if hooksTbl == nil {
		hooksTbl = map[string]any{}
		doc["hooks"] = hooksTbl
	}
	changedFlag := false
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "Interrupt", "SessionEnd"} {
		var list []map[string]any
		switch v := hooksTbl[ev].(type) {
		case []map[string]any:
			list = v
		case []any:
			for _, item := range v {
				if mm, ok := item.(map[string]any); ok {
					list = append(list, mm)
				}
			}
		case map[string]any:
			// Existing table form ([hooks.Stop] command = "..."): never
			// touch it; appending an array-of-tables would be invalid TOML.
			res.Note = "existing [hooks." + ev + "] table left untouched; add awareness entries manually if desired"
			continue
		case nil:
		default:
			res.Note = "unsupported [hooks." + ev + "] shape left untouched"
			continue
		}
		already := false
		for _, e := range list {
			if t, _ := e["type"].(string); t == "command" {
				if c, _ := e["command"].(string); strings.Contains(c, "hook codex") {
					if mk, _ := e["agent_conductor"].(bool); mk {
						already = true
					}
				}
			}
		}
		if already {
			continue
		}
		list = append(list, map[string]any{
			"agent_conductor": true,
			"type":            "command",
			"command":         cmd,
			"timeout":         10,
		})
		hooksTbl[ev] = list
		changedFlag = true
	}
	if !changedFlag {
		res.Note = "already installed"
		return res, nil
	}
	if len(raw) > 0 {
		backup, err := m.backup(path, raw)
		if err != nil {
			return res, err
		}
		res.Backup = backup
	}
	var sb strings.Builder
	if err := encodeTOML(&sb, doc); err != nil {
		return res, err
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// UninstallCodex removes only Agent Conductor [hooks.*] entries.
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
	var doc map[string]any
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		return res, err
	}
	hooksTbl, ok := doc["hooks"].(map[string]any)
	if !ok {
		res.Note = "no hooks section"
		return res, nil
	}
	removed := false
	for ev, v := range hooksTbl {
		var list []map[string]any
		switch t := v.(type) {
		case []map[string]any:
			list = t
		case []any:
			for _, item := range t {
				if mm, ok := item.(map[string]any); ok {
					list = append(list, mm)
				}
			}
		default:
			continue
		}
		kept := []map[string]any{}
		for _, e := range list {
			if mk, _ := e["agent_conductor"].(bool); mk {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(hooksTbl, ev)
		} else {
			hooksTbl[ev] = kept
		}
	}
	if !removed {
		res.Note = "nothing to remove"
		return res, nil
	}
	backup, err := m.backup(path, raw)
	if err != nil {
		return res, err
	}
	res.Backup = backup
	if len(hooksTbl) == 0 {
		delete(doc, "hooks")
	}
	var sb strings.Builder
	if err := encodeTOML(&sb, doc); err != nil {
		return res, err
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// backup writes path.bak with timestamp suffix and returns the backup path.
func (m *Manager) backup(path string, data []byte) (string, error) {
	bak := fmt.Sprintf("%s.%s%s", path, time.Now().UTC().Format("20060102T150405Z"), backupExt)
	if err := os.WriteFile(bak, data, 0o600); err != nil {
		return "", err
	}
	return bak, nil
}

// encodeTOML writes a deterministic TOML document with fully-qualified
// nested table headers ([hooks.Stop] etc.).
func encodeTOML(sb *strings.Builder, doc map[string]any) error {
	encodeTablePath(sb, "", doc)
	return nil
}

func encodeTablePath(sb *strings.Builder, path string, tbl map[string]any) {
	keys := sortedKeys(tbl)
	// simple values first
	for _, k := range keys {
		if isTable(tbl[k]) {
			continue
		}
		sb.WriteString(quoteKey(k) + " = " + encodeValue(tbl[k]) + "\n")
	}
	// sub-tables with full paths
	for _, k := range keys {
		v := tbl[k]
		if sub, ok := v.(map[string]any); ok {
			full := k
			if path != "" {
				full = path + "." + k
			}
			if len(sub) > 0 {
				sb.WriteString("\n[" + full + "]\n")
			}
			encodeTablePath(sb, full, sub)
		}
	}
}

func isTable(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func quoteKey(k string) string {
	if k == "" {
		return `""`
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return fmt.Sprintf("%q", k)
		}
	}
	return k
}

func encodeValue(v any) string {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("%q", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int64:
		return fmt.Sprintf("%d", t)
	case int:
		return fmt.Sprintf("%d", t)
	case float64:
		return fmt.Sprintf("%g", t)
	case []map[string]any:
		parts := make([]string, len(t))
		for i, e := range t {
			inner := encodeInlineTable(e)
			parts[i] = inner
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = encodeValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		return encodeInlineTable(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func encodeInlineTable(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = quoteKey(k) + " = " + encodeValue(m[k])
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}
