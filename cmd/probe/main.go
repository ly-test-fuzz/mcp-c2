// Command debugmcp-probe queries a running hub over its IPC link (the same
// MAC-token-authenticated socket shims use). Ops/verification utility: confirms the
// hub is up and shows connected agents + occupancy.
//
// Usage:
//
//	DBGMCP_HUB_ADDR=unix:/path|tcp:host:port DBGMCP_HUB_TOKEN=<hex> debugmcp-probe [method]
//
// method defaults to "list_targets"; any hub method works (e.g. status, list_sessions).
// DBGMCP_HUB_SOCKET is accepted as a backward-compat alias for "unix:$DBGMCP_HUB_SOCKET".
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
	addr := os.Getenv("DBGMCP_HUB_ADDR")
	if addr == "" {
		if sock := os.Getenv("DBGMCP_HUB_SOCKET"); sock != "" {
			addr = "unix:" + sock
		}
	}
	token := os.Getenv("DBGMCP_HUB_TOKEN")
	if addr == "" || token == "" {
		fmt.Fprintln(os.Stderr, "debugmcp-probe: DBGMCP_HUB_ADDR (unix:/path or tcp:host:port) and DBGMCP_HUB_TOKEN env vars required")
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

	network, address, err := ipc.ParseListenSpec(addr)
	if err != nil {
		log.Fatalf("parse addr: %v", err)
	}
	c, err := ipc.Dial(context.Background(), network, address, token)
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
