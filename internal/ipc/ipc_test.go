package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type echoHandler struct{}

func (echoHandler) Handle(method string, params json.RawMessage) (any, error) {
	if method == "echo" {
		return map[string]any{"method": method, "params": string(params)}, nil
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

func startTestServer(t *testing.T, token string) (string, context.CancelFunc) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ipc.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go Serve(ctx, ln, token, echoHandler{})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return path, cancel
}

func TestIPCValidTokenRoundTrip(t *testing.T) {
	path, cancel := startTestServer(t, "secret-token-xyz")
	defer cancel()
	ctx := context.Background()
	c, err := Dial(ctx, "unix", path, "secret-token-xyz")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var out map[string]any
	if err := c.Call(ctx, "echo", map[string]any{"k": "v"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["method"] != "echo" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestIPCBadTokenRejected(t *testing.T) {
	path, cancel := startTestServer(t, "good-token")
	defer cancel()
	_, err := Dial(context.Background(), "unix", path, "WRONG-token")
	if err == nil {
		t.Fatal("expected auth rejection for wrong token")
	}
}

// startTestServerTCP starts an IPC server on a TCP loopback port. Validates the
// Phase 1 split-deployment path: shim <-> hub over TCP (not unix socket).
func startTestServerTCP(t *testing.T, token string) (addr string, cancel context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go Serve(ctx, ln, token, echoHandler{})
	return ln.Addr().String(), cancel
}

// TestIPC_TCP_HMACRoundTrip drives the full HMAC challenge-response over TCP: the
// token never crosses the wire, only HMAC-SHA256(token, nonce) does, and a normal
// echo call succeeds afterwards.
func TestIPC_TCP_HMACRoundTrip(t *testing.T) {
	addr, cancel := startTestServerTCP(t, "tcp-token-abc")
	defer cancel()
	c, err := Dial(context.Background(), "tcp", addr, "tcp-token-abc")
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer c.Close()
	var out map[string]any
	if err := c.Call(context.Background(), "echo", map[string]any{"k": "v"}, &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if out["method"] != "echo" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

// TestIPC_TCP_BadTokenRejected ensures a wrong token fails the HMAC proof over
// TCP (the client computes HMAC with the wrong key; server rejects).
func TestIPC_TCP_BadTokenRejected(t *testing.T) {
	addr, cancel := startTestServerTCP(t, "good-tcp-token")
	defer cancel()
	_, err := Dial(context.Background(), "tcp", addr, "WRONG")
	if err == nil {
		t.Fatal("expected auth rejection for wrong token over TCP")
	}
}

// TestParseListenSpec covers the IPC listen spec parser used by both hub and shim.
func TestParseListenSpec(t *testing.T) {
	cases := []struct {
		spec        string
		wantNetwork string
		wantAddr    string
		wantErr     bool
	}{
		{"unix:/tmp/hub.sock", "unix", "/tmp/hub.sock", false},
		{"unix:/a/b/c", "unix", "/a/b/c", false},
		{"tcp:127.0.0.1:7778", "tcp", "127.0.0.1:7778", false},
		{"tcp:host:port", "tcp", "host:port", false},
		{"bad:spec", "", "", true},
		{"/no/scheme", "", "", true},
	}
	for _, c := range cases {
		net, addr, err := ParseListenSpec(c.spec)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseListenSpec(%q): want error, got (%q,%q,nil)", c.spec, net, addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseListenSpec(%q): unexpected error: %v", c.spec, err)
			continue
		}
		if net != c.wantNetwork || addr != c.wantAddr {
			t.Errorf("ParseListenSpec(%q): got (%q,%q), want (%q,%q)", c.spec, net, addr, c.wantNetwork, c.wantAddr)
		}
	}
}
