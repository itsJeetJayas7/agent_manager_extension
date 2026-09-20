package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	return New("/usr/local/bin/agent-conductor", t.TempDir())
}

func TestClaudeInstallIdempotentPreserves(t *testing.T) {
	m := testManager(t)
	path := m.claudeSettingsPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	existing := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"matcher": "*",
				"hooks":   []any{map[string]any{"type": "command", "command": "echo user-hook"}},
			}},
		},
	}
	b, _ := json.Marshal(existing)
	os.WriteFile(path, b, 0o600)

	r1, err := m.InstallClaude()
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Changed || r1.Backup == "" {
		t.Fatalf("first install must change + backup: %+v", r1)
	}
	r2, err := m.InstallClaude()
	if err != nil {
		t.Fatal(err)
	}
	if r2.Changed {
		t.Fatal("second install must be idempotent")
	}

	var doc map[string]any
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &doc)
	// user hook preserved
	stops := doc["hooks"].(map[string]any)["Stop"].([]any)
	if len(stops) != 2 {
		t.Fatalf("user Stop hook lost: %v", stops)
	}
	if stops[0].(map[string]any)["matcher"] != "*" {
		t.Fatal("user entry mutated")
	}
	// our entry present
	ours := stops[1].(map[string]any)
	if ours[Namespace] != true {
		t.Fatal("namespaced entry missing")
	}
	if doc["model"] != "opus" {
		t.Fatal("unrelated setting lost")
	}
	// uninstall removes only ours
	r3, err := m.UninstallClaude()
	if err != nil || !r3.Changed {
		t.Fatalf("uninstall: %+v %v", r3, err)
	}
	data, _ = os.ReadFile(path)
	json.Unmarshal(data, &doc)
	stops = doc["hooks"].(map[string]any)["Stop"].([]any)
	if len(stops) != 1 {
		t.Fatalf("our entry not removed surgically: %v", stops)
	}
}

func TestClaudeInstallRejectsBrokenJSON(t *testing.T) {
	m := testManager(t)
	path := m.claudeSettingsPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := m.InstallClaude(); err == nil {
		t.Fatal("must refuse broken settings")
	}
}

func TestClaudeInstallCreatesWhenMissing(t *testing.T) {
	m := testManager(t)
	r, err := m.InstallClaude()
	if err != nil || !r.Changed {
		t.Fatalf("%+v %v", r, err)
	}
	data, _ := os.ReadFile(m.claudeSettingsPath())
	if !strings.Contains(string(data), "hook claude") {
		t.Fatal("hook command missing")
	}
}

func TestCodexInstallUninstall(t *testing.T) {
	m := testManager(t)
	path := m.codexConfigPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("model = \"gpt-5.1-codex\"\n\n[hooks.Stop]\ncommand = \"echo hi\"\n"), 0o600)

	r1, err := m.InstallCodex()
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Changed {
		t.Fatalf("expected change: %+v", r1)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)
	if !strings.Contains(s, "hook codex") || !strings.Contains(s, "model = \"gpt-5.1-codex\"") {
		t.Fatalf("codex merge wrong:\n%s", s)
	}
	// user's existing [hooks.Stop] table must survive untouched
	if !strings.Contains(s, "command = \"echo hi\"") {
		t.Fatalf("user hook table lost:\n%s", s)
	}
	r2, _ := m.InstallCodex()
	if r2.Changed {
		t.Fatal("codex install must be idempotent")
	}
	r3, err := m.UninstallCodex()
	if err != nil || !r3.Changed {
		t.Fatalf("uninstall: %+v %v", r3, err)
	}
	raw, _ = os.ReadFile(path)
	s = string(raw)
	if strings.Contains(s, "hook codex") || !strings.Contains(s, "model = \"gpt-5.1-codex\"") || !strings.Contains(s, "command = \"echo hi\"") {
		t.Fatalf("codex uninstall wrong:\n%s", s)
	}
}

func TestBackupsCreated(t *testing.T) {
	m := testManager(t)
	path := m.claudeSettingsPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(`{"a":1}`), 0o600)
	r, err := m.InstallClaude()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Backup); err != nil {
		t.Fatal("backup file missing")
	}
	b, _ := os.ReadFile(r.Backup)
	if string(b) != `{"a":1}` {
		t.Fatal("backup content wrong")
	}
}
