// Command debugmcp-cli is the human-facing status tool. It auto-discovers the
// running hub from the default state dir (~/.debugmcp: hub.sock + ipc.token) and
// prints a readable snapshot: is the hub up, which agents (clients) are connected,
// and which sessions are active.
//
// Usage:
//
//	debugmcp-cli [status|targets|sessions]   # default: status
//	debugmcp-cli --socket <path> --token <hex>  # override hub endpoint
//
// Exit codes: 0 ok, 1 hub not reachable, 2 usage/config error.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"debugmcp/internal/hub"
	"debugmcp/internal/ipc"
)

func main() {
	var (
		socket = flag.String("socket", "", "hub IPC socket path (default: <state>/hub.sock)")
		token  = flag.String("token", "", "hub IPC token hex (default: <state>/ipc.token)")
		state  = flag.String("state", defaultStateDir(), "hub state directory")
		raw    = flag.Bool("json", false, "emit raw JSON instead of formatted tables")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "debugmcp-cli — human-facing hub status tool\n\n")
		fmt.Fprintf(os.Stderr, "Usage: %s [status|targets|sessions] [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Subcommands (default status):\n")
		fmt.Fprintf(os.Stderr, "  status    hub up/down + connected agents + active sessions summary\n")
		fmt.Fprintf(os.Stderr, "  targets   list connected agents (clients)\n")
		fmt.Fprintf(os.Stderr, "  sessions  list active sessions\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	cmd := "status"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}
	switch cmd {
	case "status", "targets", "sessions":
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", cmd)
		flag.Usage()
		os.Exit(2)
	}

	sock, tok, err := resolve(*state, *socket, *token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := ipc.Dial(ctx, "unix", sock, tok)
	if err != nil {
		// Hub not reachable: friendly message instead of a stack trace.
		fmt.Fprintf(os.Stderr, "hub not reachable at %s\n", sock)
		fmt.Fprintf(os.Stderr, "  %v\n", err)
		fmt.Fprintf(os.Stderr, "start it with: debugmcp-hub (state dir: %s)\n", *state)
		os.Exit(1)
	}
	defer c.Close()

	method := map[string]string{"status": "status", "targets": "list_targets", "sessions": "list_sessions"}[cmd]
	var rawOut json.RawMessage
	if err := c.Call(ctx, method, nil, &rawOut); err != nil {
		fmt.Fprintf(os.Stderr, "hub call %s: %v\n", method, err)
		os.Exit(1)
	}

	if *raw {
		var v any
		_ = json.Unmarshal(rawOut, &v)
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return
	}

	switch cmd {
	case "status":
		var s hub.StatusInfo
		if err := json.Unmarshal(rawOut, &s); err != nil {
			fmt.Fprintf(os.Stderr, "decode status: %v\n", err)
			os.Exit(1)
		}
		printStatus(s, sock)
	case "targets":
		var ts []hub.TargetInfo
		if err := json.Unmarshal(rawOut, &ts); err != nil {
			fmt.Fprintf(os.Stderr, "decode targets: %v\n", err)
			os.Exit(1)
		}
		printTargets(ts)
	case "sessions":
		var ss []sessionInfo
		_ = json.Unmarshal(rawOut, &ss)
		printSessions(ss)
	}
}

// sessionInfo mirrors wire.SessionInfo without importing wire here.
type sessionInfo struct {
	Sid       string `json:"sid"`
	OpSession string `json:"op_session"`
	State     string `json:"state"`
	IdleMs    int64  `json:"idle_ms"`
	CreatedMs int64  `json:"created_ms"`
}

func resolve(stateDir, sock, tok string) (string, string, error) {
	if sock == "" {
		sock = filepath.Join(stateDir, "hub.sock")
	}
	if tok == "" {
		path := filepath.Join(stateDir, "ipc.token")
		b, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", "", fmt.Errorf("no IPC token at %s (is the hub initialized? run debugmcp-hub first)", path)
			}
			return "", "", fmt.Errorf("read token %s: %w", path, err)
		}
		tok = string(b)
	} else {
		// Validate hex token form early for a clearer error.
		if _, err := hex.DecodeString(tok); err != nil {
			return "", "", fmt.Errorf("invalid --token hex: %w", err)
		}
	}
	return sock, tok, nil
}

func printStatus(s hub.StatusInfo, sock string) {
	fmt.Printf("hub: up  (socket %s)\n", sock)
	fmt.Printf("connected agents : %d\n", s.Targets)
	fmt.Printf("active sessions  : %d\n", s.TotalSessions)
	if len(s.TargetsList) == 0 {
		fmt.Println("\n(no agents connected)")
		return
	}
	fmt.Println()
	printTargets(s.TargetsList)
}

func printTargets(ts []hub.TargetInfo) {
	if len(ts) == 0 {
		fmt.Println("(no agents connected)")
		return
	}
	fmt.Printf("%-20s %-16s %-10s %-10s %-8s %s\n", "AGENT ID", "HOSTNAME", "PLATFORM", "ARCH", "SESS", "STATUS")
	fmt.Println(repeat("-", 78))
	for _, t := range ts {
		status := t.Status
		if t.Busy {
			status += " (busy)"
		}
		fmt.Printf("%-20s %-16s %-10s %-10s %-8d %s\n",
			trunc(t.ID, 20), t.Hostname, t.Platform, t.Arch, t.SessionsActive, status)
		for _, ses := range t.Sessions {
			fmt.Printf("    └─ session %-12s op=%-12s idle=%s\n", ses.Sid, trunc(ses.OpSession, 12), fmtDur(ses.IdleMs))
		}
	}
}

func printSessions(ss []sessionInfo) {
	if len(ss) == 0 {
		fmt.Println("(no active sessions)")
		return
	}
	fmt.Printf("%-20s %-16s %-10s %s\n", "SESSION ID", "OP SESSION", "STATE", "IDLE")
	fmt.Println(repeat("-", 60))
	for _, s := range ss {
		fmt.Printf("%-20s %-16s %-10s %s\n", trunc(s.Sid, 20), trunc(s.OpSession, 16), s.State, fmtDur(s.IdleMs))
	}
}

func fmtDur(ms int64) string {
	if ms < 0 {
		return "?"
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return d.Round(time.Second).String()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func repeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".debugmcp"
	}
	return filepath.Join(home, ".debugmcp")
}
