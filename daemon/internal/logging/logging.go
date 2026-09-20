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

// Clear removes all registered secrets. Called when the shared secret block
// is deleted, so a value that no longer exists is no longer tracked.
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

// redactPatterns replaces EVERY occurrence of each pattern. It used to use
// strings.Index and therefore redacted only the first hit per line, leaving
// later tokens on the same line in the clear.
func redactPatterns(s string) string {
	for _, p := range patternPairs {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	return s
}

// Rotation defaults: the comment used to claim rotation was "handled by the
// service"; nothing rotated anything and the file grew without bound.
const (
	DefaultMaxLogBytes = 10 * 1024 * 1024
	DefaultMaxLogFiles = 3
)

// Logger writes JSON lines.
type Logger struct {
	mu       sync.Mutex
	out      io.Writer
	file     *os.File
	path     string
	size     int64
	maxBytes int64
	maxFiles int
	min      Level
	redactor *Redactor
}

// New creates a logger writing to path, rotating by size (10MB x 3).
func New(path string, min Level, redactor *Redactor) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	return &Logger{
		out: io.MultiWriter(f, os.Stderr), file: f, path: path, size: size,
		maxBytes: DefaultMaxLogBytes, maxFiles: DefaultMaxLogFiles,
		min: min, redactor: redactor,
	}, nil
}

// SetRotation configures size-based rotation (0 keeps the defaults).
func (l *Logger) SetRotation(maxBytes int64, maxFiles int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if maxBytes > 0 {
		l.maxBytes = maxBytes
	}
	if maxFiles > 0 {
		l.maxFiles = maxFiles
	}
}

// rotateLocked renames daemon.jsonl -> .1 -> .2 ... and reopens. Caller holds
// l.mu.
func (l *Logger) rotateLocked() {
	if l.file == nil || l.path == "" {
		return
	}
	l.file.Close()
	// Drop the oldest, shift the rest up.
	os.Remove(fmt.Sprintf("%s.%d", l.path, l.maxFiles))
	for i := l.maxFiles - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	os.Rename(l.path, l.path+".1")
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		// Keep logging to stderr rather than losing the logger entirely.
		l.file, l.out, l.size = nil, os.Stderr, 0
		return
	}
	l.file, l.out, l.size = f, io.MultiWriter(f, os.Stderr), 0
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
	line := append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil && l.maxBytes > 0 && l.size+int64(len(line)) > l.maxBytes {
		l.rotateLocked()
	}
	n, _ := l.out.Write(line)
	l.size += int64(n)
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
