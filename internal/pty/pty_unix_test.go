//go:build !windows

package pty

import (
	"bytes"
	"testing"
	"time"
)

// TestPTYEchoAndExit: spawn a shell, send a command, observe echoed output, then
// exit and reap the child with a clean exit code. The master is closed only AFTER
// the child exits on its own (Wait before Close) so the shell is not killed by
// the SIGHUP that closing the master delivers to the slave's process group.
func TestPTYEchoAndExit(t *testing.T) {
	p, err := Start("/bin/sh", 80, 24)
	if err != nil {
		t.Skipf("cannot start PTY/shell: %v", err)
	}

	token := "ZZMARK-90210"
	if _, err := p.Write([]byte("echo " + token + "\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	ch := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				nb := make([]byte, n)
				copy(nb, buf[:n])
				ch <- nb
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()

	got := make([]byte, 0, 4096)
	deadline := time.After(5 * time.Second)
	sawToken := false
	for !sawToken {
		select {
		case nb, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before seeing token; got %q", got)
			}
			got = append(got, nb...)
			if bytes.Contains(got, []byte(token)) {
				sawToken = true
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q; got %q", token, got)
		}
	}

	if _, err := p.Write([]byte("exit 0\r\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	// Drain until the child exits (ptmx EOF -> reader closes ch), then Wait
	// BEFORE closing the master so the shell is not SIGHUP'd.
	exited := false
	for !exited {
		select {
		case _, ok := <-ch:
			if !ok {
				exited = true
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for child exit")
		}
	}

	exitCode, signal, err := p.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	_ = p.Close()
	if signal != "" || exitCode != 0 {
		t.Fatalf("expected clean exit 0, got code=%d signal=%q", exitCode, signal)
	}
}
