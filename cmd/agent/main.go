// Command debugmcp-agent is the C2 client run on a target machine. It dials the
// hub at the enrolled address, authenticates with the PSK, daemonizes (detaches
// from the launching shell), and serves exec/shell/fs operations.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"debugmcp/internal/agent"
	"debugmcp/internal/version"
)

func main() {
	version.RegisterFlag(flag.CommandLine, "debugmcp-agent")
	hubAddr := flag.String("hub", "127.0.0.1:7777", "hub host:port")
	pskHex := flag.String("psk", "", "PSK hex (enrollment secret), or use -psk-file")
	pskFile := flag.String("psk-file", "", "file containing PSK hex")
	id := flag.String("id", "", "agent id (auto-generated if empty)")
	noDaemon := flag.Bool("no-daemon", false, "do not detach into the background")
	flag.Parse()

	psk := mustPSK(*pskHex, *pskFile)
	a := agent.New(agent.Options{
		HubAddr: *hubAddr, PSK: psk, ID: *id, NoDaemon: *noDaemon,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

func mustPSK(pskHex, pskFile string) []byte {
	var hexStr string
	switch {
	case pskHex != "":
		hexStr = pskHex
	case pskFile != "":
		b, err := os.ReadFile(pskFile)
		if err != nil {
			log.Fatalf("read psk-file: %v", err)
		}
		hexStr = strings.TrimSpace(string(b))
	default:
		log.Fatal("missing PSK: pass -psk <hex> or -psk-file <path>")
	}
	dec, err := hex.DecodeString(hexStr)
	if err != nil {
		log.Fatalf("psk hex: %v", err)
	}
	if len(dec) < 32 {
		log.Fatalf("psk must be >= 32 bytes (got %d)", len(dec))
	}
	return dec
}
