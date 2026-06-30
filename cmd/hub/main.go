// Command debugmcp-hub runs the operator-side hub: a Noise-authenticated TCP
// listener for C2 agents plus a MAC-token-authenticated Unix-socket IPC server for
// shims. PSK + IPC token are generated on first run and persisted (0600) under the
// state dir. The hub refuses to bind non-loopback without --allow-inbound, because
// a config error here equals remote code execution.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"debugmcp/internal/audit"
	"debugmcp/internal/hub"
	"debugmcp/internal/ipc"
)

func main() {
	var (
		listen       = flag.String("listen", "127.0.0.1:7777", "TCP addr for C2 agents")
		allowInbound = flag.Bool("allow-inbound", false, "allow binding a non-loopback address (required for 0.0.0.0; warning: RCE surface)")
		stateDir     = flag.String("state", defaultStateDir(), "state directory (psk, token, socket, audit)")
	)
	flag.Parse()

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	if !*allowInbound && !isLoopback(*listen) {
		log.Fatalf("refusing non-loopback bind %q without --allow-inbound (config error = RCE)", *listen)
	}

	psk := loadOrGenHex(filepath.Join(*stateDir, "psk.hex"), 32)
	token := loadOrGenHex(filepath.Join(*stateDir, "ipc.token"), 32)
	aud, err := audit.Open(filepath.Join(*stateDir, "audit.jsonl"))
	if err != nil {
		log.Fatalf("audit: %v", err)
	}

	h := hub.New(psk, aud)

	agentLn, err := h.ListenAgents(*listen)
	if err != nil {
		log.Fatalf("listen agents: %v", err)
	}
	ipcPath := filepath.Join(*stateDir, "hub.sock")
	_ = os.Remove(ipcPath)
	ipcLn, err := net.Listen("unix", ipcPath)
	if err != nil {
		log.Fatalf("listen ipc: %v", err)
	}
	_ = os.Chmod(ipcPath, 0o600)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tokenStr := hex.EncodeToString(token)
	go func() { _ = h.ServeAgentsOn(ctx, agentLn) }()
	go func() { _ = ipc.Serve(ctx, ipcLn, tokenStr, h) }()

	log.Printf("hub: agents=%s psk=%s", *listen, hex.EncodeToString(psk))
	log.Printf("hub: ipc=%s token=%s", ipcPath, tokenStr)
	log.Printf("hub: ready. Claude Code MCP stdio config -> command=debugmcp-shim, env={DBGMCP_HUB_SOCKET=%s DBGMCP_HUB_TOKEN=%s}", ipcPath, tokenStr)

	<-ctx.Done()
	log.Printf("hub: shutting down")
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".debugmcp"
	}
	return filepath.Join(home, ".debugmcp")
}

// loadOrGenHex reads n bytes (stored as hex) from path, or generates + persists them.
func loadOrGenHex(path string, n int) []byte {
	if b, err := os.ReadFile(path); err == nil {
		if dec, derr := hex.DecodeString(strings.TrimSpace(string(b))); derr == nil && len(dec) == n {
			return dec
		}
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)), 0o600); err != nil {
		log.Fatalf("write %s: %v", path, err)
	}
	return buf
}

func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == ""
}
