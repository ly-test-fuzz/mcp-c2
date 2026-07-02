package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogAppendOnlyAndFsync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return t0 }

	if err := l.Log(Record{Op: "exec", Args: "echo hi", Session: "win-A", Target: "t1", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Log(Record{Op: "shell_open", Target: "t1", Result: "busy"}); err != nil {
		t.Fatal(err)
	}
	// Reopen: must append, never truncate.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l2.now = func() time.Time { return t0.Add(time.Second) }
	if err := l2.Log(Record{Op: "upload", Target: "t1", Result: "ok", Bytes: 42}); err != nil {
		t.Fatal(err)
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 records, got %d", len(lines))
	}
	var first Record
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Op != "exec" || first.Args != "echo hi" || first.Session != "win-A" {
		t.Fatalf("first record mismatch: %+v", first)
	}
	if first.Ts != t0.UnixMilli() {
		t.Fatalf("ts not stamped: %d", first.Ts)
	}
	var third Record
	json.Unmarshal([]byte(lines[2]), &third)
	if third.Bytes != 42 {
		t.Fatalf("bytes mismatch: %+v", third)
	}
}

func TestRedactor(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(filepath.Join(dir, "a.jsonl"))
	l.SetRedactor(func(s string) string { return strings.ReplaceAll(s, "secret", "***") })
	if err := l.Log(Record{Op: "download", Args: "token=secret path=/x"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "a.jsonl"))
	if strings.Contains(string(data), "secret") || !strings.Contains(string(data), "***") {
		t.Fatalf("redaction failed: %s", string(data))
	}
}

func TestFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	l, _ := Open(path)
	l.Close()
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %o", fi.Mode().Perm())
	}
}
