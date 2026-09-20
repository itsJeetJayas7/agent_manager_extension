package providers

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// binCache memoises discovery so the capability probe, which runs on every
// snapshot, does not re-scan the editor extension directories each time.
var binCache sync.Map // name -> path

// findBinary locates a provider executable. PATH is the happy path, but both
// Claude Code and Codex are commonly installed *only* as a binary bundled
// inside their official VS Code extension, which is never on PATH. Without
// looking there the daemon reports "runtime missing" for a provider the user
// can see working in the editor next to it.
func findBinary(name string) string {
	if p := os.Getenv(envOverride(name)); p != "" {
		if usable(p) {
			return p
		}
	}
	if cached, ok := binCache.Load(name); ok {
		if p, _ := cached.(string); p != "" && usable(p) {
			return p
		}
		binCache.Delete(name)
	}
	p := searchBinary(name)
	if p != "" {
		binCache.Store(name, p)
	}
	return p
}

// envOverride is the escape hatch for a non-standard install: AC_CODEX_BIN,
// AC_CLAUDE_BIN.
func envOverride(name string) string {
	return "AC_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name)) + "_BIN"
}

func searchBinary(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".local", "share", name, "bin", name),
		filepath.Join(home, "."+name, "bin", name),
		filepath.Join(home, ".cargo", "bin", name),
		filepath.Join(home, ".bun", "bin", name),
		filepath.Join(home, ".volta", "bin", name),
		"/opt/homebrew/bin/" + name,
		"/usr/local/bin/" + name,
		"/usr/bin/" + name,
	}
	for _, c := range candidates {
		if usable(c) {
			return c
		}
	}
	return findInEditorExtensions(name)
}

// editorExtensionRoots lists the extension directories of the editors that
// ship a provider CLI. Codex lives in openai.chatgpt, Claude Code in
// anthropic.claude-code; forks (Cursor, Windsurf) keep the same layout.
func editorExtensionRoots() []string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	var roots []string
	for _, d := range []string{".vscode", ".vscode-insiders", ".vscode-server", ".vscode-server-insiders", ".cursor", ".cursor-server", ".windsurf", ".windsurf-server", ".vscodium"} {
		roots = append(roots, filepath.Join(home, d, "extensions"))
	}
	return roots
}

// bundlePatterns are the in-extension locations a CLI is shipped at, relative
// to an extension directory. `bin/<platform-triple>/<name>` is the Codex
// layout; `resources/native-binary/<name>` is the Claude Code one.
func bundlePatterns(name string) []string {
	return []string{
		filepath.Join("bin", "*", name),
		filepath.Join("bin", name),
		filepath.Join("resources", "native-binary", name),
		filepath.Join("resources", name),
		filepath.Join("dist", "bin", name),
	}
}

// findInEditorExtensions returns the newest bundled copy of name. Newest, not
// first, because several versions of an extension can sit side by side and an
// outdated CLI may not speak the protocol the adapter expects.
func findInEditorExtensions(name string) string {
	type hit struct {
		path string
		mod  int64
	}
	var hits []hit
	for _, root := range editorExtensionRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			ext := filepath.Join(root, e.Name())
			for _, pat := range bundlePatterns(name) {
				matches, _ := filepath.Glob(filepath.Join(ext, pat))
				for _, m := range matches {
					if !usable(m) {
						continue
					}
					var mod int64
					if fi, err := os.Stat(m); err == nil {
						mod = fi.ModTime().UnixNano()
					}
					hits = append(hits, hit{path: m, mod: mod})
				}
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].mod != hits[j].mod {
			return hits[i].mod > hits[j].mod
		}
		return hits[i].path > hits[j].path
	})
	return hits[0].path
}

// usable reports whether p is an existing, executable regular file.
func usable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// FindRuntime exposes provider binary discovery outside this package so that
// every surface that answers "is this runtime installed?" — the capability
// probe and `agent-conductor diagnostics` — agrees. Diagnostics used to call
// exec.LookPath directly and so disagreed with the sidebar.
func FindRuntime(name string) string { return findBinary(name) }
