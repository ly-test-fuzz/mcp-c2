package shim

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"debugmcp/internal/agent"
	"debugmcp/internal/hub"
	"debugmcp/internal/ipc"
)

// TestShim_ExposesToolsAndExecs is the MCP spike: a real MCP client discovers the
// shim's tools (>= 10) and an end-to-end exec (client -> shim -> ipc -> hub -> agent)
// returns the expected bash-syntax output.
func TestShim_ExposesToolsAndExecs(t *testing.T) {
	psk := bytes.Repeat([]byte{0x33}, 32)
	h := hub.New(psk, nil)

	agentLn, err := h.ListenAgents("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := agentLn.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.ServeAgentsOn(ctx, agentLn) }()

	a := agent.New(agent.Options{HubAddr: addr, PSK: psk, NoDaemon: true, Cap: 8})
	go func() { _ = a.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	var target string
	for time.Now().Before(deadline) {
		if ts := h.ListTargets(); len(ts) > 0 {
			target = ts[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if target == "" {
		t.Fatal("agent did not register")
	}

	ipcPath := filepath.Join(t.TempDir(), "hub.sock")
	ipcLn, err := net.Listen("unix", ipcPath)
	if err != nil {
		t.Fatal(err)
	}
	token := "test-token-secret"
	go func() { _ = ipc.Serve(ctx, ipcLn, token, h) }()
	time.Sleep(50 * time.Millisecond) // let the IPC listener accept

	sh, err := New(ctx, Config{Socket: ipcPath, Token: token, OpSession: "test-win"})
	if err != nil {
		t.Fatal(err)
	}

	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := sh.Server().Connect(ctx, t1, nil); err != nil {
		t.Fatal(err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	cs, err := cli.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	lr, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(lr.Tools) < 10 {
		names := make([]string, 0, len(lr.Tools))
		for _, tt := range lr.Tools {
			names = append(names, tt.Name)
		}
		t.Fatalf("expected >=10 tools exposed, got %d: %v", len(lr.Tools), names)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec",
		Arguments: map[string]any{"target": target, "command": "echo hi-$(echo 99)"},
	})
	if err != nil {
		t.Fatalf("call exec: %v", err)
	}
	if !containsText(res, "hi-99") {
		t.Fatalf("exec result missing token; content=%v isError=%v", res.Content, res.IsError)
	}
}

func containsText(res *mcp.CallToolResult, want string) bool {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, want) {
			return true
		}
	}
	return false
}
