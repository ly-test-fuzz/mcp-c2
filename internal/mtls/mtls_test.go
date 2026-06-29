package mtls

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllowedListEmptyAllowsAll(t *testing.T) {
	al, err := LoadAllowedList("")
	if err != nil {
		t.Fatal(err)
	}
	if !al.Empty() {
		t.Fatal("expected empty list")
	}
	if !al.ContainsFingerprint("abcdef") {
		t.Fatal("empty allowlist should allow any fingerprint")
	}
}

func TestAllowedListNormalizesFingerprint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allowed.txt")
	if err := os.WriteFile(path, []byte("# comment\nAA:BB:cc\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	al, err := LoadAllowedList(path)
	if err != nil {
		t.Fatal(err)
	}
	if al.Empty() {
		t.Fatal("expected entries")
	}
	if !al.ContainsFingerprint("aabbcc") {
		t.Fatal("expected normalized fingerprint match")
	}
	if al.ContainsFingerprint("deadbeef") {
		t.Fatal("unexpected fingerprint match")
	}
}
