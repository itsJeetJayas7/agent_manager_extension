package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := v.Seal("sudo pw: hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, "hunter2") {
		t.Fatal("ciphertext contains plaintext")
	}
	pt, err := v.Open(ct)
	if err != nil {
		t.Fatal(err)
	}
	if pt != "sudo pw: hunter2" {
		t.Fatalf("got %q", pt)
	}
}

func TestWrongKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	v1, _ := Open(filepath.Join(dir, "k1"))
	v2, _ := Open(filepath.Join(dir, "k2"))
	ct, _ := v1.Seal("secret")
	if _, err := v2.Open(ct); err == nil {
		t.Fatal("expected auth failure with wrong key")
	}
}

func TestTamperFails(t *testing.T) {
	dir := t.TempDir()
	v, _ := Open(filepath.Join(dir, "k"))
	ct, _ := v.Seal("secret")
	tampered := ct[:len(ct)-4] + "AAAA"
	if _, err := v.Open(tampered); err == nil {
		t.Fatal("expected tamper detection")
	}
}

func TestKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	_, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key perms = %v, want 0600", fi.Mode().Perm())
	}
}

func TestCorruptKeySizeRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	os.WriteFile(p, []byte("short"), 0o600)
	if _, err := Open(p); err == nil {
		t.Fatal("expected size validation error")
	}
}
