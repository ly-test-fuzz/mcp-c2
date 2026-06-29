package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/debugmcp/mcp-c2/internal/hubapi"
	"github.com/debugmcp/mcp-c2/internal/mcpserver"
)

func main() {
	hubAddr := flag.String("hub", "127.0.0.1:9000", "mcpc2-hub HTTP API address")
	flag.Parse()

	log.SetFlags(0) // MCP stdio uses stdout; keep stderr clean for logging

	// ── Connect to hub ────────────────────────────────────────────
	api := hubapi.NewClient(*hubAddr)

	// Verify hub is reachable before starting MCP server.
	// This gives the AI a clear error instead of a cryptic timeout later.
	if err := api.Ping(2 * time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "mcpc2-server: hub not reachable at %s: %v\n", *hubAddr, err)
		fmt.Fprintf(os.Stderr, "mcpc2-server: start 'mcpc2-hub' first, then run this MCP server.\n")
		os.Exit(1)
	}
	log.Printf("mcpc2-server: connected to hub at %s", *hubAddr)

	// ── Run MCP stdio server ──────────────────────────────────────
	if err := mcpserver.RunStdio(context.Background(), api); err != nil {
		log.Fatal(err)
	}
}
