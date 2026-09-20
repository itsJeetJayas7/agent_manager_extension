package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
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

// §4.3 — InstallCodex used to decode config.toml into a map and re-encode it,
// destroying every comment and all formatting. Everything outside the managed
// block must now survive byte for byte.
func TestCodexInstallPreservesCommentsAndFormatting(t *testing.T) {
	m := testManager(t)
	path := m.codexConfigPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	original := `# My Codex configuration.
# Please keep these comments.

model    = "gpt-5.1-codex"   # trailing comment
approval_policy = "on-request"

[history]
# how much to keep
persistence = "save-all"
`
	os.WriteFile(path, []byte(original), 0o600)

	if _, err := m.InstallCodex(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(after), original) {
		t.Fatalf("the original bytes were not preserved:\n%s", after)
	}
	for _, want := range []string{
		"# My Codex configuration.",
		"# Please keep these comments.",
		`model    = "gpt-5.1-codex"   # trailing comment`,
		"# how much to keep",
	} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("lost %q from the user's config", want)
		}
	}

	// And uninstall restores the file exactly.
	if _, err := m.UninstallCodex(); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != original {
		t.Fatalf("uninstall did not restore the original bytes:\ngot:\n%q\nwant:\n%q", restored, original)
	}
}

// A TOML datetime used to come back through fmt.Sprintf("%v") as an unquoted,
// syntactically invalid token.
func TestCodexInstallSurvivesExoticValues(t *testing.T) {
	m := testManager(t)
	path := m.codexConfigPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	original := "created = 1979-05-27T07:32:00Z\nratio = 0.75\ntags = [\"a\", \"b\"]\n"
	os.WriteFile(path, []byte(original), 0o600)
	if _, err := m.InstallCodex(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "created = 1979-05-27T07:32:00Z") {
		t.Fatalf("datetime mangled:\n%s", after)
	}
	var doc map[string]any
	if _, err := toml.Decode(string(after), &doc); err != nil {
		t.Fatalf("wrote invalid TOML: %v\n%s", err, after)
	}
}

func TestCodexInstallRefusesInvalidToml(t *testing.T) {
	m := testManager(t)
	path := m.codexConfigPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("this is not [ valid toml\n"), 0o600)
	if _, err := m.InstallCodex(); err == nil {
		t.Fatal("must refuse a config it cannot parse")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "this is not [ valid toml\n" {
		t.Fatal("the unparseable config was modified anyway")
	}
}

// Backups used to accumulate forever, one per install/uninstall.
func TestBackupsArePruned(t *testing.T) {
	m := testManager(t)
	path := m.claudeSettingsPath()
	os.MkdirAll(filepath.Dir(path), 0o700)
	for i := 0; i < 9; i++ {
		os.WriteFile(path, []byte(`{"a":1}`), 0o600)
		// Distinct timestamps so the names differ.
		bak := path + "." + time.Now().UTC().Add(time.Duration(i)*time.Second).Format("20060102T150405Z") + backupExt
		os.WriteFile(bak, []byte("old"), 0o600)
	}
	os.WriteFile(path, []byte(`{"a":2}`), 0o600)
	if _, err := m.InstallClaude(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(path + ".*" + backupExt)
	if len(matches) > 5 {
		t.Fatalf("backups not pruned: %d remain", len(matches))
	}
}
