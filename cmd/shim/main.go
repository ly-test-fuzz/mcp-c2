// Command debugmcp-shim is the stdio MCP server Claude Code spawns. It reads its
// hub IPC endpoint + MAC token from the environment and proxies tool calls to the
// hub. Configure Claude Code with: command=debugmcp-shim,
// env={DBGMCP_HUB_SOCKET=<path>, DBGMCP_HUB_TOKEN=<token>}.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"debugmcp/internal/shim"
)

func main() {
	socket := os.Getenv("DBGMCP_HUB_SOCKET")
	token := os.Getenv("DBGMCP_HUB_TOKEN")
	op := os.Getenv("DBGMCP_OP_SESSION")
	if socket == "" || token == "" {
		fmt.Fprintln(os.Stderr, "debugmcp-shim: DBGMCP_HUB_SOCKET and DBGMCP_HUB_TOKEN env vars are required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := shim.New(ctx, shim.Config{Socket: socket, Token: token, OpSession: op})
	if err != nil {
		log.Fatalf("shim: %v", err)
	}
	if err := s.Run(ctx); err != nil {
		log.Fatalf("shim: %v", err)
	}
}
