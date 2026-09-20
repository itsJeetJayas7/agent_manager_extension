package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A second daemon on the same root used to remove the socket and listen in the
// first one's place, leaving two schedulers on one database.
func TestInstanceLockIsExclusive(t *testing.T) {
	root := t.TempDir()

	first, err := AcquireInstanceLock(root)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Release()

	if _, err := AcquireInstanceLock(root); err == nil {
		t.Fatal("a second daemon acquired the same root")
	} else if !strings.Contains(err.Error(), "already owns") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestInstanceLockNamesTheHolder(t *testing.T) {
	root := t.TempDir()
	l, err := AcquireInstanceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	b, err := os.ReadFile(filepath.Join(root, "run", "conductor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) == "" {
		t.Fatal("the lock file does not record the owning pid")
	}
}

func TestInstanceLockIsReleasable(t *testing.T) {
	root := t.TempDir()
	l, err := AcquireInstanceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	second, err := AcquireInstanceLock(root)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	second.Release()
}

// Different roots are independent: a user may run an isolated instance.
func TestInstanceLockIsPerRoot(t *testing.T) {
	a, err := AcquireInstanceLock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := AcquireInstanceLock(t.TempDir())
	if err != nil {
		t.Fatalf("a second root should be independent: %v", err)
	}
	b.Release()
}

// The flock must be held by the PROCESS, so a second daemon started while the
// first is alive exits instead of stealing the socket.
func TestSecondDaemonProcessExitsCleanly(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent-conductor")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/agent-conductor")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build the daemon here: %v\n%s", err, out)
	}
	root := t.TempDir()

	first, err := AcquireInstanceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "AC_ROOT="+root, "AC_MOCK=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a second daemon should exit 0, not error: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "already owns") {
		t.Fatalf("the second daemon did not explain itself:\n%s", out)
	}
}
