package agent

import "debugmcp/internal/pty"

// openPTY starts a shell under a new PTY, returning it as the ptyHandle the
// session manager uses (indirection enables future test fakes).
func openPTY(shell string, cols, rows uint16) (ptyHandle, error) {
	return pty.Start(shell, cols, rows)
}
