// Package logging provides structured JSON logging with mandatory
// pre-write redaction of secret material.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level enumerates log levels.
type Level string

const (
	Debug Level = "debug"
	Info  Level = "info"
	Warn  Level = "warn"
	Error Level = "error"
)

var levelOrder = map[Level]int{Debug: 0, Info: 1, Warn: 2, Error: 3}

// Redactor holds known secret values that must never reach output.
type Redactor struct {
	mu      sync.RWMutex
	secrets map[string]struct{}
}

// NewRedactor returns an empty redactor.
func NewRedactor() *Redactor { return &Redactor{secrets: map[string]struct{}{}} }

// Add registers a secret value to redact. Values shorter than 4 runes are
// ignored (redacting "ok" would mangle everything).
func (r *Redactor) Add(secret string) {
	s := strings.TrimSpace(secret)
	if len([]rune(s)) < 4 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets[s] = struct{}{}
}

// AddMany registers several secrets at once.
func (r *Redactor) AddMany(secrets []string) {
	for _, s := range secrets {
		r.Add(s)
	}
}

// Clear removes all registered secrets (used by tests).
func (r *Redactor) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = map[string]struct{}{}
}

// Redact replaces every registered secret occurrence and common credential
// patterns with a fixed placeholder.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Deterministic order avoids depending on map iteration.
	keys := make([]string, 0, len(r.secrets))
	for k := range r.secrets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if strings.Contains(s, k) {
			s = strings.ReplaceAll(s, k, "[REDACTED]")
		}
	}
	return redactPatterns(s)
}

var patternPairs = [][2]string{
	{`Authorization: Bearer `, `Authorization: Bearer [REDACTED]`},
	{`"api_key"`, `"[REDACTED_KEY]"`},
	{`sk-ant-`, `[REDACTED-TOKEN-PREFIX]`},
	{`sk-proj-`, `[REDACTED-TOKEN-PREFIX]`},
	{`ghp_`, `[REDACTED-TOKEN-PREFIX]`},
}

func redactPatterns(s string) string {
	for _, p := range patternPairs {
		if i := strings.Index(s, p[0]); i >= 0 {
			s = s[:i] + p[1] + s[i+len(p[0]):]
		}
	}
	return s
}

// Logger writes JSON lines.
type Logger struct {
	mu       sync.Mutex
	out      io.Writer
	file     *os.File
	min      Level
	redactor *Redactor
}

// New creates a logger writing to path (rolling by size is handled by the
// service; here we keep one file plus stderr passthrough at info+).
func New(path string, min Level, redactor *Redactor) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{out: io.MultiWriter(f, os.Stderr), file: f, min: min, redactor: redactor}, nil
}

// NewNop returns a logger that discards everything (tests).
func NewNop() *Logger { return &Logger{out: io.Discard, min: Debug, redactor: NewRedactor()} }

// Close closes the underlying file.
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Log emits one JSON line at the given level. msg and fields are redacted.
func (l *Logger) Log(level Level, msg string, fields map[string]any) {
	if levelOrder[level] < levelOrder[l.min] {
		return
	}
	msg = l.redactor.Redact(msg)
	if fields != nil {
		for k, v := range fields {
			fields[k] = l.redactor.Redact(fmt.Sprintf("%v", v))
		}
	}
	rec := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "level": string(level), "msg": msg}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out.Write(append(b, '\n'))
}

// Debugf/Infof/Warnf/Errorf are convenience wrappers.
func (l *Logger) Debugf(msg string, args ...any) { l.Logf(Debug, msg, args...) }
func (l *Logger) Infof(msg string, args ...any)  { l.Logf(Info, msg, args...) }
func (l *Logger) Warnf(msg string, args ...any)  { l.Logf(Warn, msg, args...) }
func (l *Logger) Errorf(msg string, args ...any) { l.Logf(Error, msg, args...) }

func (l *Logger) Logf(level Level, msg string, args ...any) {
	l.Log(level, fmt.Sprintf(msg, args...), nil)
}

// WithFields logs with structured fields.
func (l *Logger) WithFields(level Level, msg string, fields map[string]any) {
	l.Log(level, msg, fields)
}

// Redactor accessor (shared registration).
func (l *Logger) Redactor() *Redactor { return l.redactor }
