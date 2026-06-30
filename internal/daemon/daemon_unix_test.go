//go:build !windows

package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestDaemonizeNoopWhenSentinel(t *testing.T) {
	t.Setenv(envDaemonized, "1")
	_, err := daemonize()
	if err != ErrAlreadyDaemonized {
		t.Fatalf("expected ErrAlreadyDaemonized, got %v", err)
	}
}

func TestDetachedAttrHasSetsid(t *testing.T) {
	a := detachedSysProcAttr()
	if !a.Setsid {
		t.Fatal("expected Setsid=true on POSIX")
	}
}

// TestDetachedChildGetsNewSession spawns a child with the detach attr and asserts
// it lands in a DIFFERENT session id than the caller (proves setsid works).
// Uses `ps` rather than syscall.Getsid (not exposed in the linux syscall pkg).
func TestDetachedChildGetsNewSession(t *testing.T) {
	parentSID := psSID(t, os.Getpid())

	cmd := exec.Command("sh", "-c", "ps -o sid= -p $$ | tr -d ' '")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Run(); err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	childSID := atoiOrSkip(t, strings.TrimSpace(out.String()))
	if childSID == parentSID {
		t.Fatalf("child sid %d == parent sid %d; expected detachment", childSID, parentSID)
	}
}

func psSID(t *testing.T, pid int) int {
	out, err := exec.Command("ps", "-o", "sid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	return atoiOrSkip(t, strings.TrimSpace(string(out)))
}

func atoiOrSkip(t *testing.T, s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Skipf("could not parse %q: %v", s, err)
	}
	return n
}
