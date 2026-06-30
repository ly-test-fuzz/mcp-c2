// Command debugmcp-probe queries a running hub over its IPC link (the same
// MAC-token-authenticated socket shims use). Ops/verification utility: confirms the
// hub is up and shows connected agents + occupancy.
//
// Usage:
//
//	DBGMCP_HUB_SOCKET=<path> DBGMCP_HUB_TOKEN=<hex> debugmcp-probe [method]
//
// method defaults to "list_targets"; any hub method works (e.g. status, list_sessions).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"debugmcp/internal/ipc"
)

func main() {
	socket := os.Getenv("DBGMCP_HUB_SOCKET")
	token := os.Getenv("DBGMCP_HUB_TOKEN")
	if socket == "" || token == "" {
		fmt.Fprintln(os.Stderr, "debugmcp-probe: DBGMCP_HUB_SOCKET and DBGMCP_HUB_TOKEN env vars required")
		os.Exit(2)
	}
	method := "list_targets"
	if len(os.Args) > 1 {
		method = os.Args[1]
	}
	// Optional 2nd arg: JSON params for the method (e.g. exec).
	var params any
	if len(os.Args) > 2 {
		params = json.RawMessage(os.Args[2])
	}

	c, err := ipc.Dial(context.Background(), "unix", socket, token)
	if err != nil {
		log.Fatalf("dial hub: %v", err)
	}
	defer c.Close()

	var out any
	if err := c.Call(context.Background(), method, params, &out); err != nil {
		log.Fatalf("%s: %v", method, err)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
