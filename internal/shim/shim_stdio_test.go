package shim

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"debugmcp/internal/agent"
	"debugmcp/internal/hub"
	"debugmcp/internal/ipc"
)

// TestShim_RealBinaryStdio is the definitive Claude-Code-integration test: it
// spawns the REAL shim binary and drives it over actual stdio via the go-sdk
// CommandTransport (exactly how Claude Code launches an MCP server), then runs
// list_targets + exec end-to-end through hub -> agent. This closes the "real
// stdio" gap that the in-memory transport test does not cover.
func TestShim_RealBinaryStdio(t *testing.T) {
	// Skip if the toolchain isn't on PATH (e.g. stripped CI).
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}

	// 1. build the real shim binary
	shimBin := filepath.Join(t.TempDir(), "dbgmcp-shim")
	if out, err := exec.Command("go", "build", "-o", shimBin, "debugmcp/cmd/shim").CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v: %s", err, out)
	}

	// 2. hub + agent in-process
	psk := bytes.Repeat([]byte{0x44}, 32)
	h := hub.New(psk, nil)
	agentLn, err := h.ListenAgents("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.ServeAgentsOn(ctx, agentLn) }()
	a := agent.New(agent.Options{HubAddr: agentLn.Addr().String(), PSK: psk, NoDaemon: true, Cap: 8})
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

	// 3. ipc listener
	ipcPath := filepath.Join(t.TempDir(), "hub.sock")
	ipcLn, err := net.Listen("unix", ipcPath)
	if err != nil {
		t.Fatal(err)
	}
	token := "stdio-test-token"
	go func() { _ = ipc.Serve(ctx, ipcLn, token, h) }()
	time.Sleep(50 * time.Millisecond)

	// 4. spawn the REAL shim binary over stdio via CommandTransport (= Claude Code)
	cmd := exec.Command(shimBin)
	cmd.Env = append(os.Environ(),
		"DBGMCP_HUB_SOCKET="+ipcPath,
		"DBGMCP_HUB_TOKEN="+token,
	)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code-test", Version: "v0.0.1"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("client connect over stdio: %v", err)
	}
	defer cs.Close()

	// 5. discover tools and run exec end-to-end
	lr, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(lr.Tools) < 10 {
		t.Fatalf("expected >=10 tools over real stdio, got %d", len(lr.Tools))
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "exec",
		Arguments: map[string]any{"target": target, "command": "echo real-stdio-$(echo 7)"},
	})
	if err != nil {
		t.Fatalf("call exec: %v", err)
	}
	if !containsText(res, "real-stdio-7") {
		t.Fatalf("exec result missing token; content=%v isError=%v", res.Content, res.IsError)
	}
}
