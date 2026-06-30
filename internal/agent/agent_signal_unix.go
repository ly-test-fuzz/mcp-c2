//go:build !windows

package agent

import "syscall"

// killGroup sends a signal to the shell's foreground process group (negative pid).
func killGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, sig)
}
