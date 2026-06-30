//go:build windows

package agent

import (
	"errors"
	"syscall"
)

// killGroup: Windows process-group signal delivery is Phase 3. The shell path is
// unreachable on Windows today (pty.Start returns ErrConPTYNotImplemented), so
// this is a compile-safe stub.
func killGroup(pid int, sig syscall.Signal) error {
	return errors.New("killGroup: Windows process-group signals are Phase 3")
}
