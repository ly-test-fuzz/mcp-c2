// Command debugmcp-shim is the stdio MCP server Claude Code spawns. It reads its
// hub IPC endpoint + MAC token from the environment and proxies tool calls to the
// hub. Configure Claude Code with: command=debugmcp-shim,
// env={DBGMCP_HUB_ADDR=unix:/path|tcp:host:port, DBGMCP_HUB_TOKEN=<token>}.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"debugmcp/internal/shim"
	"debugmcp/internal/version"
)

func main() {
	version.HandleEarly("debugmcp-shim")
	addr := os.Getenv("DBGMCP_HUB_ADDR")
	if addr == "" {
		// backward compat: DBGMCP_HUB_SOCKET was the unix-only form.
		if sock := os.Getenv("DBGMCP_HUB_SOCKET"); sock != "" {
			addr = "unix:" + sock
		}
	}
	token := os.Getenv("DBGMCP_HUB_TOKEN")
	op := os.Getenv("DBGMCP_OP_SESSION")
	if addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "debugmcp-shim: DBGMCP_HUB_ADDR (unix:/path or tcp:host:port) and DBGMCP_HUB_TOKEN env vars are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := shim.New(ctx, shim.Config{Addr: addr, Token: token, OpSession: op})
	if err != nil {
		log.Fatalf("shim: %v", err)
	}
	if err := s.Run(ctx); err != nil {
		log.Fatalf("shim: %v", err)
	}
}
