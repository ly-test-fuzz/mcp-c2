//go:build !windows

package session

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionEcho(t *testing.T) {
	m := NewManager()
	s, interactive, err := m.Open(context.Background(), "test", "/bin/sh")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skip("PTY allocation is blocked by the current sandbox")
		}
		t.Fatal(err)
	}
	if !interactive {
		t.Fatal("expected interactive")
	}
	defer m.Close("test")
	if err := m.Write("test", "echo READY", true); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case out := <-s.Output():
			if string(out.Data) == "" {
				continue
			}
			if contains(out.Data, []byte("READY")) {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for echo")
		}
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
