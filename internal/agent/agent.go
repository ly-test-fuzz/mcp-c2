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
}

// New constructs an Agent (does not connect).
func New(o Options) *Agent {
	if o.Cap <= 0 {
		o.Cap = DefaultConcurrencyCap
	}
	if o.ID == "" {
		o.ID = autoID()
	}
	return &Agent{opts: o, shells: make(map[string]*shellSession)}
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
	shell := loginShell()
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

	case wire.MsgFSRead:
		req, _ := wire.Decode[wire.FSRead](env)
		a.fsRead(env, req)
	case wire.MsgFSWrite:
		req, _ := wire.Decode[wire.FSWrite](env)
		a.fsWrite(env, req)
	case wire.MsgFSList:
		req, _ := wire.Decode[wire.FSList](env)
		a.fsList(env, req)
	case wire.MsgFSStat:
		req, _ := wire.Decode[wire.FSStat](env)
		a.fsStat(env, req)

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
		shell = loginShell()
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

func (a *Agent) fsRead(env *wire.Envelope, req *wire.FSRead) {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadResult, &wire.FSReadResult{Err: err.Error()})
		return
	}
	if int64(len(data)) > DefaultMaxFileSize {
		a.reply(env.Seq, env.Sid, wire.MsgFSReadResult, &wire.FSReadResult{Err: fmt.Sprintf("file exceeds %d bytes", DefaultMaxFileSize)})
		return
	}
	a.reply(env.Seq, env.Sid, wire.MsgFSReadResult, &wire.FSReadResult{Data: data})
}

func (a *Agent) fsWrite(env *wire.Envelope, req *wire.FSWrite) {
	if int64(len(req.Data)) > DefaultMaxFileSize {
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteResult, &wire.FSOpResult{Err: fmt.Sprintf("payload exceeds %d bytes", DefaultMaxFileSize)})
		return
	}
	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.WriteFile(req.Path, req.Data, mode); err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSWriteResult, &wire.FSOpResult{Err: err.Error()})
		return
	}
	a.reply(env.Seq, env.Sid, wire.MsgFSWriteResult, &wire.FSOpResult{})
}

func (a *Agent) fsList(env *wire.Envelope, req *wire.FSList) {
	entries, err := os.ReadDir(req.Path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSListResult, &wire.FSListResult{Err: err.Error()})
		return
	}
	out := make([]wire.DirEntry, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		out = append(out, wire.DirEntry{Name: e.Name(), IsDir: e.IsDir(), Mode: uint32(info.Mode()), Size: info.Size()})
	}
	a.reply(env.Seq, env.Sid, wire.MsgFSListResult, &wire.FSListResult{Entries: out})
}

func (a *Agent) fsStat(env *wire.Envelope, req *wire.FSStat) {
	info, err := os.Stat(req.Path)
	if err != nil {
		a.reply(env.Seq, env.Sid, wire.MsgFSStatResult, &wire.FSStatResult{Err: err.Error()})
		return
	}
	a.reply(env.Seq, env.Sid, wire.MsgFSStatResult, &wire.FSStatResult{Stat: &wire.FileInfo{
		Name: filepath.Base(req.Path), Size: info.Size(), Mode: uint32(info.Mode()),
		IsDir: info.IsDir(), ModTimeMs: info.ModTime().UnixMilli(),
	}})
}

// --- helpers ---

func loginShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "/bin/sh"
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
