//go:build windows

package daemon

import "syscall"

// detachedSysProcAttr spawns the child detached with no window. Full Windows
// hardening (service install, ConPTY) is Phase 3; this is best-effort + builds.
const (
	detachedProcess = 0x00000008
)

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess,
		HideWindow:    true,
	}
}
