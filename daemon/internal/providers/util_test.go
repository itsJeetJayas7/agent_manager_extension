package providers

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Both Codex and Claude Code are frequently installed only as a binary bundled
// inside their official VS Code extension, which is never on PATH. Before this
// lookup the daemon reported "runtime missing" for a provider the user could
// see working in the editor beside it.
func TestFindBinaryLocatesEditorBundledCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binCache.Delete("acfakecodex")

	// The Codex layout: <ext>/bin/<platform-triple>/<name>.
	dir := filepath.Join(home, ".vscode", "extensions", "openai.chatgpt-1.2.3", "bin", "macos-aarch64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "acfakecodex")
	if err := os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := findBinary("acfakecodex"); got != want {
		t.Fatalf("findBinary = %q, want %q", got, want)
	}
}

// The Claude Code layout differs from the Codex one, so both are probed.
func TestFindBinaryLocatesNativeBinaryLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binCache.Delete("acfakeclaude")

	dir := filepath.Join(home, ".cursor", "extensions", "anthropic.claude-code-9.9.9", "resources", "native-binary")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "acfakeclaude")
	if err := os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := findBinary("acfakeclaude"); got != want {
		t.Fatalf("findBinary = %q, want %q", got, want)
	}
}

// A non-executable file is not a runtime.
func TestFindBinaryIgnoresNonExecutable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binCache.Delete("acfakenoexec")

	dir := filepath.Join(home, ".vscode", "extensions", "openai.chatgpt-1.2.3", "bin", "linux-x86_64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acfakenoexec"), []byte("not a binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := findBinary("acfakenoexec"); got != "" {
		t.Fatalf("findBinary = %q, want empty", got)
	}
}

// AC_<NAME>_BIN is the escape hatch for a non-standard install and must win.
func TestFindBinaryEnvOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binCache.Delete("acfakeoverride")

	custom := filepath.Join(home, "somewhere", "odd")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(custom, "acfakeoverride")
	if err := os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AC_ACFAKEOVERRIDE_BIN", want)

	if got := findBinary("acfakeoverride"); got != want {
		t.Fatalf("findBinary = %q, want %q", got, want)
	}
}

// Several versions of an extension can sit side by side; an outdated CLI may
// not speak the protocol the adapter expects, so the newest one wins.
func TestFindBinaryPrefersNewestBundle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binCache.Delete("acfakenewest")

	var newest string
	for i, ver := range []string{"1.0.0", "2.0.0"} {
		dir := filepath.Join(home, ".vscode", "extensions", "openai.chatgpt-"+ver, "bin", "macos-aarch64")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "acfakenewest")
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Set the times explicitly rather than relying on filesystem
		// timestamp granularity to separate two writes made microseconds apart.
		ts := time.Unix(int64(1_700_000_000+i*86_400), 0)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		newest = p
	}

	if got := findBinary("acfakenewest"); got != newest {
		t.Fatalf("findBinary = %q, want %q", got, newest)
	}
}

// permissionToCodex feeds two different protocol fields that spell the same
// concept differently. thread/start wants a kebab-case SandboxMode; turn/start
// wants a camelCase tagged SandboxPolicy. Sending the camelCase tag to
// thread/start made every real Codex run fail with
// "unknown variant `dangerFullAccess`". Values verified against
// `codex app-server generate-json-schema` from the installed binary.
func TestPermissionToCodexUsesTheProtocolSpellings(t *testing.T) {
	modes := map[string]bool{"read-only": true, "workspace-write": true, "danger-full-access": true}
	policies := map[string]bool{"readOnly": true, "workspaceWrite": true, "dangerFullAccess": true}
	approvals := map[string]bool{"untrusted": true, "on-request": true, "never": true}

	for _, perm := range []string{"bypass", "plan", "acceptEdits", "default", ""} {
		approval, mode, policy := permissionToCodex(perm)
		if !approvals[approval] {
			t.Errorf("%q: approvalPolicy %q is not an AskForApproval variant", perm, approval)
		}
		if !modes[mode] {
			t.Errorf("%q: sandbox %q is not a SandboxMode variant", perm, mode)
		}
		if !policies[policy] {
			t.Errorf("%q: sandboxPolicy type %q is not a SandboxPolicy variant", perm, policy)
		}
	}

	if _, mode, policy := permissionToCodex("bypass"); mode != "danger-full-access" || policy != "dangerFullAccess" {
		t.Fatalf("bypass = (%q, %q)", mode, policy)
	}
	if got := sandboxPolicyFor("dangerFullAccess"); got["type"] != "dangerFullAccess" {
		t.Fatalf("sandboxPolicyFor = %+v", got)
	}
}

// A model's own effort ladder wins over the generic one, with its default
// first and no duplicates — including when the default is not listed first.
func TestOrderEfforts(t *testing.T) {
	cases := []struct {
		name      string
		supported []string
		dflt      string
		want      []string
	}{
		{"default in the middle", []string{"low", "medium", "high"}, "medium", []string{"medium", "low", "high"}},
		{"default already first", []string{"low", "medium"}, "low", []string{"low", "medium"}},
		{"default last", []string{"low", "medium", "xhigh"}, "xhigh", []string{"xhigh", "low", "medium"}},
		{"default not supported", []string{"low", "high"}, "medium", []string{"low", "high"}},
		{"no default", []string{"low", "high"}, "", []string{"low", "high"}},
		{"nothing advertised", nil, "high", []string{"high", "low", "medium", "xhigh"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := orderEfforts(c.supported, c.dflt)
			if len(got) != len(c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			seen := map[string]bool{}
			for i, g := range got {
				if g != c.want[i] {
					t.Fatalf("got %v want %v", got, c.want)
				}
				if seen[g] {
					t.Fatalf("duplicate effort %q in %v", g, got)
				}
				seen[g] = true
			}
		})
	}
}
