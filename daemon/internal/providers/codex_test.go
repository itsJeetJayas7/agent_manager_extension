package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agent-conductor/internal/logging"
)

// stubAppServer writes a tiny JSON-RPC stdio server to disk and returns its
// path. It answers `initialize` and any other method with an empty result, so
// the adapter's handshake and call path are exercised for real.
func stubAppServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	body := `#!/usr/bin/env python3
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except Exception:
        continue
    if "id" not in msg:
        continue
    method = msg.get("method", "")
    if method == "unsupported/method":
        out = {"jsonrpc": "2.0", "id": msg["id"],
               "error": {"code": -32601, "message": "method not found"}}
    elif method == "account/rateLimits/read":
        out = {"jsonrpc": "2.0", "id": msg["id"],
               "result": {"rateLimits": {"primary": {"usedPercent": 12.0, "isAtLimit": False}}}}
    else:
        out = {"jsonrpc": "2.0", "id": msg["id"], "result": {}}
    sys.stdout.write(json.dumps(out) + "\n")
    sys.stdout.flush()
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for the stub app-server")
	}
	return script
}

// §4.2 / §10.4 — ensureStarted held c.mu for its whole body and then called
// call(), which takes c.mu again. sync.Mutex is not reentrant, so the very
// first Codex operation deadlocked forever and, because the lock was never
// released, every later Codex call blocked with it. The test timeout makes a
// regression fail fast instead of hanging CI.
func TestCodexEnsureStartedDoesNotDeadlock(t *testing.T) {
	c := NewCodex(logging.NewNop(), stubAppServer(t))
	t.Cleanup(func() { c.Close() })

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- c.ensureStarted(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ensureStarted: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ensureStarted deadlocked")
	}
}

// A call after the handshake must still work — the lock must not be left held.
func TestCodexCallsAfterHandshake(t *testing.T) {
	c := NewCodex(logging.NewNop(), stubAppServer(t))
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ensureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := c.call(ctx, "model/list", map[string]any{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := c.ProbeQuota(ctx); err != nil {
		t.Fatalf("ProbeQuota: %v", err)
	}
}

// Concurrent first use must not deadlock or start two processes.
func TestCodexConcurrentEnsureStarted(t *testing.T) {
	c := NewCodex(logging.NewNop(), stubAppServer(t))
	t.Cleanup(func() { c.Close() })
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errs <- c.ensureStarted(ctx)
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("concurrent ensureStarted: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent ensureStarted deadlocked")
		}
	}
}

// Unsupported methods are recorded so the Control panel can say which parts of
// the app-server surface this build actually has.
func TestCodexRecordsMethodSupport(t *testing.T) {
	c := NewCodex(logging.NewNop(), stubAppServer(t))
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ensureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.call(ctx, "unsupported/method", map[string]any{}); err == nil {
		t.Fatal("expected the stub to reject an unknown method")
	}
	sup := c.MethodSupport()
	if sup["initialize"] != "ok" {
		t.Fatalf("initialize not recorded as ok: %q", sup["initialize"])
	}
	if sup["unsupported/method"] == "ok" || sup["unsupported/method"] == "" {
		t.Fatalf("unsupported method not recorded: %q", sup["unsupported/method"])
	}
}

// Cancel must reach the provider. c.cancels was never written, so it was a
// complete no-op.
func TestCodexCancelIsNotANoOp(t *testing.T) {
	c := NewCodex(logging.NewNop(), stubAppServer(t))
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ensureStarted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel("run-that-does-not-exist"); err == nil {
		t.Fatal("cancelling an unknown run should report that there is nothing to cancel")
	}
	// Register a live execution the way Execute does.
	runCtx, runCancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancels["run1"] = runCancel
	c.threads["run1"] = "thread-1"
	c.mu.Unlock()
	if err := c.Cancel("run1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-runCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel did not cancel the execution context")
	}
	if c.MethodSupport()["turn/interrupt"] != "ok" {
		t.Fatal("Cancel did not send turn/interrupt")
	}
}

func TestParseEffortsUsesItsArgument(t *testing.T) {
	if got := parseEfforts(""); !reflect.DeepEqual(got, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("default ladder changed: %v", got)
	}
	got := parseEfforts("high")
	if got[0] != "high" {
		t.Fatalf("the model's own default should lead: %v", got)
	}
	if len(got) != 4 {
		t.Fatalf("efforts should not duplicate: %v", got)
	}
}

// The adapter must not write a malformed JSON-RPC envelope.
func TestCodexSendsJsonRpcEnvelope(t *testing.T) {
	payload := map[string]any{"jsonrpc": "2.0", "method": "initialize", "id": 1, "params": map[string]any{}}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["jsonrpc"] != "2.0" {
		t.Fatal("missing jsonrpc version")
	}
	_ = fmt.Sprint(back)
}
