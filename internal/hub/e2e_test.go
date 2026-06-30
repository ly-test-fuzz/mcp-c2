package hub

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"debugmcp/internal/agent"
)

func newE2E(t *testing.T) (*Hub, string, context.CancelFunc) {
	t.Helper()
	psk := bytes.Repeat([]byte{0x11}, 32)
	h := New(psk, nil)
	ln, err := h.ListenAgents("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = h.ServeAgentsOn(ctx, ln) }()
	return h, addr, cancel
}

func waitForTarget(t *testing.T, h *Hub) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ts := h.ListTargets(); len(ts) > 0 {
			return ts[0].ID
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent did not register within timeout")
	return ""
}

func startAgent(t *testing.T, ctx context.Context, addr string, psk []byte, cap int) {
	t.Helper()
	a := agent.New(agent.Options{HubAddr: addr, PSK: psk, NoDaemon: true, Cap: cap})
	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx) }()
	// Fail fast if Run returns immediately with an error.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("agent.Run: %v", err)
		}
	default:
	}
}

func TestE2E_ExecBashSyntax(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	psk := h.PSK()
	startAgent(t, context.Background(), addr, psk, 8)
	target := waitForTarget(t, h)

	// command substitution + pipe + redirect: proves login-shell wrapping works.
	cmd := `echo "hi-$(echo 42)" | tr -d '[:space:]'`
	res, err := h.Exec(ExecParams{OpSession: "win-A", Target: target, Command: cmd})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "hi-42") {
		t.Fatalf("exec result: code=%d out=%q err=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if res.Completion != "authoritative" {
		t.Fatalf("expected authoritative completion, got %q", res.Completion)
	}
}

func TestE2E_FileReadWrite(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 8)
	target := waitForTarget(t, h)

	path := t.TempDir() + "/e2e-file.bin"
	payload := []byte{0x00, 0x01, 0xff, 0xfe, 0x55}
	if _, err := h.FSWrite(FSWriteParams{Target: target, Path: path, Data: payload, Mode: 0o600}); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := h.FSRead(FSReadParams{Target: target, Path: path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(res.Data, payload) {
		t.Fatalf("file round-trip mismatch: got %v want %v", res.Data, payload)
	}
}

func TestE2E_InteractiveShell(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 8)
	target := waitForTarget(t, h)

	open, err := h.ShellOpen(ShellOpenParams{OpSession: "win-A", Target: target})
	if err != nil {
		t.Fatalf("shell_open: %v", err)
	}
	if open.Sid == "" {
		t.Fatalf("expected session id, got %+v", open)
	}
	if err := h.ShellSend(ShellSendParams{Target: target, Sid: open.Sid, Input: []byte("echo shell-hello-XYZ\r\n")}); err != nil {
		t.Fatalf("shell_send: %v", err)
	}
	read, err := h.ShellRead(ShellReadParams{Target: target, Sid: open.Sid, TimeoutMs: 1500})
	if err != nil {
		t.Fatalf("shell_read: %v", err)
	}
	var all []byte
	for _, c := range read.Chunks {
		all = append(all, c.Data...)
	}
	if !strings.Contains(string(all), "shell-hello-XYZ") {
		t.Fatalf("interactive shell did not echo token; got %q", all)
	}
	if _, err := h.ShellClose(ShellCloseParams{Target: target, Sid: open.Sid}); err != nil {
		t.Fatalf("shell_close: %v", err)
	}
}

func TestE2E_OccupancyBusy(t *testing.T) {
	h, addr, cancel := newE2E(t)
	defer cancel()
	startAgent(t, context.Background(), addr, h.PSK(), 2) // cap=2
	target := waitForTarget(t, h)

	var sids []string
	for i := 0; i < 2; i++ {
		o, err := h.ShellOpen(ShellOpenParams{OpSession: "win-A", Target: target})
		if err != nil || o.Sid == "" {
			t.Fatalf("open %d: %v %+v", i, err, o)
		}
		sids = append(sids, o.Sid)
	}
	// Third open must be busy (cap=2).
	o, err := h.ShellOpen(ShellOpenParams{OpSession: "win-B", Target: target})
	if err != nil {
		t.Fatalf("open3: %v", err)
	}
	if o.Busy == nil || !o.Busy.Busy || o.Busy.Used != 2 || o.Busy.Cap != 2 {
		t.Fatalf("expected busy{used:2,cap:2}, got %+v", o)
	}
	// Occupancy visible in list_targets.
	ts := h.ListTargets()
	if !ts[0].Busy || ts[0].SessionsActive != 2 || ts[0].ConcurrencyCap != 2 {
		t.Fatalf("occupancy not surfaced: %+v", ts[0])
	}
	// Closing frees the slot; a new open should then succeed.
	if _, err := h.ShellClose(ShellCloseParams{Target: target, Sid: sids[0]}); err != nil {
		t.Fatal(err)
	}
	o2, err := h.ShellOpen(ShellOpenParams{OpSession: "win-C", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if o2.Sid == "" {
		t.Fatalf("expected a fresh session after close, got %+v", o2)
	}
	_, _ = h.ShellClose(ShellCloseParams{Target: target, Sid: o2.Sid})
	_, _ = h.ShellClose(ShellCloseParams{Target: target, Sid: sids[1]})
}
