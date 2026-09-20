package logging

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactionRemovesSecrets(t *testing.T) {
	r := NewRedactor()
	r.Add("hunter2")
	r.AddMany([]string{"sup3rs3cret"})
	got := r.Redact("password=hunter2 token=sup3rs3cret ok")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "sup3rs3cret") {
		t.Fatalf("leak: %s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("placeholder missing: %s", got)
	}
}

func TestRedactionShortValuesIgnored(t *testing.T) {
	r := NewRedactor()
	r.Add("ab")
	got := r.Redact("abc")
	if got != "abc" {
		t.Fatalf("over-redaction: %s", got)
	}
}

func TestLoggerRedactsBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	r := NewRedactor()
	lg, err := New(filepath.Join(dir, "log.jsonl"), Debug, r)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	r.Add("TOPSECRET")
	lg.Infof("running prompt TOPSECRET now")
	lg.WithFields(Info, "field test", map[string]any{"prompt": "TOPSECRET"})

	data, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if bytes.Contains(data, []byte("TOPSECRET")) {
		t.Fatal("secret appeared in log file")
	}
	var lines int
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ln == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("bad json line: %v", err)
		}
		lines++
	}
	if lines < 2 {
		t.Fatalf("expected 2 lines, got %d", lines)
	}
}

// §7.9 — logging.New opened one append-only file forever; the comment claimed
// the service rotated it and nothing did.
func TestLogRotationBySize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.jsonl")
	l, err := New(path, Debug, NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.SetRotation(2048, 3)

	for i := 0; i < 400; i++ {
		l.Infof("line %d %s", i, strings.Repeat("x", 100))
	}
	for _, suffix := range []string{"", ".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("expected rotated file %q: %v", path+suffix, err)
		}
	}
	// The retention limit must actually bound the set.
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Fatal("rotation kept more files than configured")
	}
	if st, err := os.Stat(path); err != nil || st.Size() > 4096 {
		t.Fatalf("active log was not rotated: %v", err)
	}
}

// §7.9 — redactPatterns used strings.Index and redacted only the first
// occurrence of each pattern per line.
func TestRedactsEveryOccurrenceOnALine(t *testing.T) {
	r := NewRedactor()
	got := r.Redact("first ghp_aaaa then ghp_bbbb and ghp_cccc")
	if strings.Count(got, "ghp_") != 0 {
		t.Fatalf("later occurrences left in the clear: %q", got)
	}
}
