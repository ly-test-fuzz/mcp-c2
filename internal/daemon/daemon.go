// Package daemon detaches the C2 agent from its parent process and terminal so it
// runs in the background after the launching shell exits (a hard requirement from
// the design intent). On POSIX this is achieved by re-exec'ing the binary in a new
// session (setsid) with stdio redirected to /dev/null and the parent exiting(0),
// reparenting the child to init. Full Windows detach+ConPTY is Phase 3.
package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

const envDaemonized = "DBGMCP_DAEMONIZED"

// ErrAlreadyDaemonized is returned (and then swallowed by Daemonize) when the
// process is already a daemon child.
var ErrAlreadyDaemonized = errors.New("daemon: already daemonized")

// Daemonize re-execs the current program detached in a new session, then the
// parent calls os.Exit(0). The child inherits os.Args[1:]. Idempotent.
func Daemonize() error {
	cmd, err := daemonize()
	if errors.Is(err, ErrAlreadyDaemonized) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = cmd
	os.Exit(0)
	return nil
}

// daemonize starts the detached re-exec WITHOUT exiting the caller. Split out so
// it can be unit-tested without os.Exit killing the test process.
func daemonize() (*exec.Cmd, error) {
	if os.Getenv(envDaemonized) == "1" {
		return nil, ErrAlreadyDaemonized
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("daemon: resolve executable: %w", err)
	}
	dn, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("daemon: open %s: %w", os.DevNull, err)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), envDaemonized+"=1")
	cmd.Stdin = dn
	cmd.Stdout = dn
	cmd.Stderr = dn
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("daemon: start: %w", err)
	}
	return cmd, nil
}
