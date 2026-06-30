//go:build !windows

package pty

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// Start spawns shell in a new PTY at the given size. If shell is empty, $SHELL or
// /bin/sh is used.
func Start(shell string, cols, rows uint16, argv ...string) (*PTY, error) {
	if shell == "" {
		if s := os.Getenv("SHELL"); s != "" {
			shell = s
		} else {
			shell = "/bin/sh"
		}
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	c := exec.Command(shell, argv...)
	c.Env = os.Environ()
	ptmx, err := pty.StartWithSize(c, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &PTY{ptmx: ptmx, cmd: c}, nil
}

// SetSize resizes the PTY window.
func (p *PTY) SetSize(cols, rows uint16) error {
	return pty.Setsize(p.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}
