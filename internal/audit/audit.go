// Package audit is an append-only JSONL audit log with per-record fsync (MVP).
// Every shell/fs/signal op is recorded for forensics/pentest reporting. Tamper-
// evidence (hash-chain/HMAC) is a Phase 3 hardening item; the append+fsync here
// already guarantees crash-safe continuity (no gap across a hub restart).
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Record is one audit entry.
type Record struct {
	Ts      int64  `json:"ts"`                // unix ms
	CorrID  string `json:"corr_id,omitempty"` // request correlation id
	Session string `json:"session,omitempty"` // op_session (Claude window attribution)
	Target  string `json:"target,omitempty"`  // agent id
	Op      string `json:"op"`                // exec|shell_open|file_read|signal|...
	Args    string `json:"args,omitempty"`    // redacted argument summary
	Result  string `json:"result,omitempty"`  // ok|error|exit=N|busy
	Bytes   int64  `json:"bytes,omitempty"`
}

// Logger writes Records to an append-only file.
type Logger struct {
	mu     sync.Mutex
	f      *os.File
	enc    *json.Encoder
	redact func(string) string
	now    func() time.Time
}

// Open opens (creating if needed) an append-only 0600 audit log at path.
func Open(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return &Logger{f: f, enc: enc, redact: identity, now: time.Now}, nil
}

// SetRedactor installs a function used to scrub secrets from Args before write.
func (l *Logger) SetRedactor(fn func(string) string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fn != nil {
		l.redact = fn
	}
}

// Log appends one record and fsyncs.
func (l *Logger) Log(r Record) error {
	if r.Ts == 0 {
		r.Ts = l.now().UnixMilli()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r.Args = l.redact(r.Args)
	if err := l.enc.Encode(r); err != nil {
		return fmt.Errorf("audit: encode: %w", err)
	}
	return l.f.Sync()
}

// Close flushes and closes the log.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func identity(s string) string { return s }
