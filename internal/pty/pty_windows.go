//go:build windows

package pty

import "errors"

// ErrConPTYNotImplemented: Windows ConPTY support is Phase 3. The package compiles
// cross-platform; Start fails clearly until the ConPTY path lands.
var ErrConPTYNotImplemented = errors.New("pty: ConPTY support not yet implemented (Phase 3)")

// Start is the Windows stub.
func Start(shell string, cols, rows uint16, argv ...string) (*PTY, error) {
	return nil, ErrConPTYNotImplemented
}

// SetSize is the Windows stub.
func (p *PTY) SetSize(cols, rows uint16) error {
	return ErrConPTYNotImplemented
}
