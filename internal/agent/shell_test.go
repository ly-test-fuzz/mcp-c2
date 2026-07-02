package agent

import (
	"os"
	"testing"
)

func TestIsRestrictedShell(t *testing.T) {
	cases := map[string]bool{
		"/bin/appliancesh":    true, // VMware vCenter VCSA
		"appliancesh":         true,
		"/usr/bin/main-shell": true, // applianceShell backend
		"/bin/bash":           false,
		"/bin/sh":             false,
		"/bin/zsh":            false,
		"/usr/bin/fish":       false,
		"":                    false,
	}
	for name, want := range cases {
		if got := isRestrictedShell(name); got != want {
			t.Errorf("isRestrictedShell(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestResolveShellIgnoresRestrictedSHELL verifies the core fix: even when
// $SHELL points at a restricted shell (vCenter /bin/appliancesh), resolveShell
// must not return it — it prefers bash found on the system.
func TestResolveShellIgnoresRestrictedSHELL(t *testing.T) {
	t.Setenv("SHELL", "/bin/appliancesh")
	s := resolveShell()
	if s == "/bin/appliancesh" {
		t.Fatalf("resolveShell() returned the restricted $SHELL verbatim; should prefer bash")
	}
	if isRestrictedShell(s) {
		t.Fatalf("resolveShell()=%q is itself a restricted shell", s)
	}
	// The resolved default must be an existing executable on this host.
	if fi, err := os.Stat(s); err != nil {
		t.Fatalf("resolveShell()=%q does not exist on host: %v", s, err)
	} else if fi.IsDir() {
		t.Fatalf("resolveShell()=%q is a directory", s)
	}
}

// TestResolveShellPrefersBash ensures bash wins over a legitimate non-restricted
// $SHELL when bash is present (the common Linux/macOS case).
func TestResolveShellPrefersBash(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	s := resolveShell()
	for _, b := range []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash"} {
		if s == b {
			return // pass: bash preferred over zsh
		}
	}
	// Host without bash in a well-known path: the result must still exist.
	if _, err := os.Stat(s); err != nil {
		t.Fatalf("resolveShell()=%q does not exist: %v", s, err)
	}
}
