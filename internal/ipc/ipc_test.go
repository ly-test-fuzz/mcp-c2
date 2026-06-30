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
