// Package pty provides a minimal, binary-transparent PTY abstraction used by the
// agent for interactive shell sessions. The binary-safety guarantee lives at the
// wire layer (wire.StreamChunk.Data is base64 over the framed envelope); the PTY
// itself is terminal-mediated (echo, line discipline) as expected for an
// interactive shell. One-shot exec does NOT use a PTY and is fully binary-clean.
package pty

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// PTY wraps a pseudo-terminal master and the child process running under it.
type PTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// Read reads raw bytes from the PTY master (stdout+stderr of the child, merged).
func (p *PTY) Read(b []byte) (int, error) { return p.ptmx.Read(b) }

// Write writes raw bytes to the child's stdin (the PTY slave).
func (p *PTY) Write(b []byte) (int, error) { return p.ptmx.Write(b) }

// Close closes the PTY master. Call Wait after Close to reap the child.
func (p *PTY) Close() error { return p.ptmx.Close() }

// PID returns the child process PID (0 if not started).
func (p *PTY) PID() int {
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// Wait blocks until the child exits and reports its exit code + signal. Safe to
// call after Close.
func (p *PTY) Wait() (exitCode int, signal string, err error) {
	err = p.cmd.Wait()
	if err == nil {
		return 0, "", nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return -1, ws.Signal().String(), nil
			}
			return ws.ExitStatus(), "", nil
		}
	}
	return -1, "", fmt.Errorf("pty: wait: %w", err)
}
