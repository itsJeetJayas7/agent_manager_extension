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
