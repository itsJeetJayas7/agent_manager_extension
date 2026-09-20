package api

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// InstanceLock is an exclusive, advisory lock on one daemon root.
//
// Without it, two daemons could serve the same ~/.agent-conductor: the second
// one removes the socket file and listens in its place, so the first becomes
// unreachable while its scheduler keeps running. Both then dispatch from the
// same database — the older one, unaware of anything the newer one was told,
// happily started runs while the queue was paused.
type InstanceLock struct {
	file *os.File
	path string
}

// AcquireInstanceLock takes the lock for root, or reports who holds it.
func AcquireInstanceLock(root string) (*InstanceLock, error) {
	dir := filepath.Join(root, "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "conductor.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := "another process"
		if b, rerr := os.ReadFile(path); rerr == nil && len(b) > 0 {
			holder = "pid " + string(b)
		}
		f.Close()
		return nil, fmt.Errorf("another agent-conductor daemon already owns %s (%s)", root, holder)
	}
	// Record who we are, for the message above and for diagnostics.
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "%d", os.Getpid())
	f.Sync()
	return &InstanceLock{file: f, path: path}, nil
}

// Release drops the lock. The file is left behind on purpose: it carries the
// last owner's pid, and removing it would race another daemon starting up.
func (l *InstanceLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	l.file.Close()
	l.file = nil
}
