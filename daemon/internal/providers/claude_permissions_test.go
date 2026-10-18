package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-conductor/internal/logging"
)

func TestHeadlessPermissionPromptsCompatibility(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(fmt.Sprintf("permission-prompts=%t", supported), func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "claude")
			help := "--print --output-format stream-json --permission-mode --permission-prompt-tool"
			if supported {
				help += " --permission-prompts <target>"
			}
			// Reproduce the VPS failure at the process boundary, before any
			// result frame is emitted, when an older CLI receives the flag.
			script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--help" ]; then
  printf 'probe\n' >> "$0.probes"
  printf '%%s\n' '%s'
  exit 0
fi
printf '%%s\n' "$@" > "$0.args"
for arg in "$@"; do
  if [ "$arg" = "--permission-prompts" ] && [ '%t' = 'false' ]; then
    echo "error: unknown option '--permission-prompts'" >&2
    exit 1
  fi
done
cat >/dev/null
printf '%%s\n' '%s' '%s'
`, help, supported, fxInit, fxResult)
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
			c.binPath = bin
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, permission := range []string{"", "default", "plan", "acceptEdits", "bypass"} {
				for _, sessionID := range []string{"", "existing-session"} {
					var events []Event
					err := c.Execute(ctx, ExecSpec{
						RunID: "compatibility", Cwd: dir, Prompt: "test",
						Permission: permission, SessionID: sessionID,
					}, func(e Event) { events = append(events, e) })
					if err != nil {
						t.Fatal(err)
					}
					done, ok := findEvent(events, EvTurnCompleted)
					if !ok || done.Status != TurnCompletedOK {
						t.Fatalf("permission=%q session=%q: turn failed: %+v (error: %v)", permission, sessionID, done, done.Error)
					}
					data, err := os.ReadFile(bin + ".args")
					if err != nil {
						t.Fatal(err)
					}
					args := strings.Split(strings.TrimSpace(string(data)), "\n")
					index := slices.Index(args, "--permission-prompts")
					if (index >= 0) != supported || (index >= 0 && (index+1 >= len(args) || args[index+1] != "none")) {
						t.Fatalf("unexpected permission prompt arguments: %v", args)
					}
					modeIndex := slices.Index(args, "--permission-mode")
					if permission == "" {
						if modeIndex >= 0 {
							t.Fatalf("overrode the configured permission mode: %v", args)
						}
					} else {
						want := permission
						if permission == "bypass" {
							want = "bypassPermissions"
						}
						if modeIndex < 0 || modeIndex+1 >= len(args) || args[modeIndex+1] != want {
							t.Fatalf("lost permission mode %q: %v", want, args)
						}
					}
					if sessionID != "" && !strings.Contains(string(data), "--resume\n"+sessionID+"\n") {
						t.Fatalf("lost resumed session: %v", args)
					}
				}
			}
			probes, err := os.ReadFile(bin + ".probes")
			if err != nil {
				t.Fatal(err)
			}
			// One probe each for stream-json output, permission prompts and
			// stream-json input; none should spawn another process on
			// subsequent turns. This CLI lacks stream input, so it keeps the
			// one-process-per-turn path.
			if strings.Count(string(probes), "probe\n") != 3 {
				t.Fatalf("capability probes not cached: %q", probes)
			}
		})
	}
}

func TestPermissionPromptsProbeRetriesAfterFailure(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
if [ ! -f "$0.retried" ]; then
  touch "$0.retried"
  echo "--permission-prompts probe failed" >&2
  exit 1
fi
echo '--permission-prompts <target>'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := NewClaude(logging.NewNop(), NewHookBus(), "/bin/true", "")
	c.binPath = bin
	if c.supportsPermissionPrompts(context.Background()) {
		t.Fatal("a failed help probe must not enable an optional flag")
	}
	if !c.supportsPermissionPrompts(context.Background()) {
		t.Fatal("a transient help failure must not be cached as unsupported")
	}
}
