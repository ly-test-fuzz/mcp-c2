//go:build !windows

package daemon

import "syscall"

// detachedSysProcAttr makes the child a session leader (new session, no
// controlling terminal) — the POSIX "setsid" step.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
