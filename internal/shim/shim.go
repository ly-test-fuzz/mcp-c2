// Package shim is the stdio MCP server Claude Code spawns per session. It is the
// ONLY component that speaks MCP; it proxies every tool call to the hub over the
// local MAC-token-authenticated IPC link. Each shim instance carries one op_session
// id (Claude-window attribution) and an optional default target from select_target.
package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"debugmcp/internal/hub"
	"debugmcp/internal/ipc"
	"debugmcp/internal/wire"
)

// Config locates the hub IPC endpoint.
type Config struct {
	Addr      string // IPC listen spec: "unix:/path" or "tcp:host:port"
	Token     string // MAC token (sent as HMAC proof, never plaintext)
	OpSession string // attribution id; auto if empty
}

// Shim is one MCP stdio server bridging Claude <-> hub.
type Shim struct {
	cfg Config
	cli *ipc.Client

	mu     sync.Mutex
	target string // selected default target (op-session state)
}

// New connects to the hub and returns a Shim ready to serve.
func New(ctx context.Context, cfg Config) (*Shim, error) {
	if cfg.Addr == "" || cfg.Token == "" {
		return nil, fmt.Errorf("shim: addr and token are required")
	}
	if cfg.OpSession == "" {
		cfg.OpSession = defaultOpSession()
	}
	network, address, err := ipc.ParseListenSpec(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("shim: %w", err)
	}
	cli, err := ipc.Dial(ctx, network, address, cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("shim: connect hub: %w", err)
	}
	return &Shim{cfg: cfg, cli: cli}, nil
}

// Server builds and returns the configured MCP server (tools registered). Exposed
// so tests can drive it over an in-memory transport without stdio.
func (s *Shim) Server() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "debugmcp", Title: "debugMcp C2", Version: "0.1.0"}, nil)
	s.registerTools(srv)
	return srv
}

// Run serves MCP over stdio until ctx is done.
func (s *Shim) Run(ctx context.Context) error {
	return s.Server().Run(ctx, &mcp.StdioTransport{})
}

func (s *Shim) resolveTarget(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	s.mu.Lock()
	t := s.target
	s.mu.Unlock()
	if t == "" {
		return "", fmt.Errorf("no target selected; call select_target first")
	}
	return t, nil
}

func (s *Shim) registerTools(srv *mcp.Server) {
	add := func(name, desc, schema string, h func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)) {
		srv.AddTool(&mcp.Tool{
			Name:        name,
			Description: desc,
			InputSchema: json.RawMessage(schema),
		}, h)
	}

	add("list_targets", "List connected C2 targets with live occupancy (sessions_active, cap, busy).", `{"type":"object"}`,
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var out []hub.TargetInfo
			if err := s.cli.Call(ctx, "list_targets", nil, &out); err != nil {
				return errResult(err), nil
			}
			return jsonResult(out), nil
		})

	add("select_target", "Set the default target for this Claude session (attribution, not isolation).", `{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target string `json:"target"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			s.mu.Lock()
			s.target = p.Target
			s.mu.Unlock()
			return textResult("selected: "+p.Target, false), nil
		})

	add("exec", "Run a one-shot command via the target login shell (bash -lc / cmd /c / pwsh -c). Supports pipes, redirects, $(). Authoritative exit code.", `{"type":"object","properties":{"command":{"type":"string"},"target":{"type":"string"},"timeout_ms":{"type":"integer"},"shell":{"type":"string"}},"required":["command"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target    string `json:"target"`
				Command   string `json:"command"`
				TimeoutMs int64  `json:"timeout_ms"`
				Shell     string `json:"shell"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var out wire.ExecResult
			if err := s.cli.Call(ctx, "exec", hub.ExecParams{OpSession: s.cfg.OpSession, Target: t, Command: p.Command, TimeoutMs: p.TimeoutMs, Shell: p.Shell}, &out); err != nil {
				return errResult(err), nil
			}
			return execResult(out), nil
		})

	add("shell_open", "Open an independent interactive PTY shell. Returns {sid} or {busy} when the per-target cap is hit.", `{"type":"object","properties":{"target":{"type":"string"},"shell":{"type":"string"},"rows":{"type":"integer"},"cols":{"type":"integer"}}}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target string `json:"target"`
				Shell  string `json:"shell"`
				Rows   uint16 `json:"rows"`
				Cols   uint16 `json:"cols"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var out wire.ShellOpenResult
			if err := s.cli.Call(ctx, "shell_open", hub.ShellOpenParams{OpSession: s.cfg.OpSession, Target: t, Shell: p.Shell, Cols: p.Cols, Rows: p.Rows}, &out); err != nil {
				return errResult(err), nil
			}
			return jsonResult(out), nil
		})

	add("shell_send", "Send raw input to an interactive shell session (state persists: cd, env, functions).", `{"type":"object","properties":{"sid":{"type":"string"},"input":{"type":"string"},"target":{"type":"string"}},"required":["sid","input"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target string `json:"target"`
				Sid    string `json:"sid"`
				Input  string `json:"input"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var ack map[string]any
			if err := s.cli.Call(ctx, "shell_send", hub.ShellSendParams{OpSession: s.cfg.OpSession, Target: t, Sid: p.Sid, Input: []byte(p.Input)}, &ack); err != nil {
				return errResult(err), nil
			}
			return jsonResult(ack), nil
		})

	add("shell_read", "Read pending output from an interactive shell. 'completion' tells you how trustworthy 'done' is (authoritative on shell exit; heuristic otherwise).", `{"type":"object","properties":{"sid":{"type":"string"},"target":{"type":"string"},"timeout_ms":{"type":"integer"}},"required":["sid"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target    string `json:"target"`
				Sid       string `json:"sid"`
				TimeoutMs int64  `json:"timeout_ms"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var out wire.ShellReadResult
			if err := s.cli.Call(ctx, "shell_read", hub.ShellReadParams{OpSession: s.cfg.OpSession, Target: t, Sid: p.Sid, TimeoutMs: p.TimeoutMs}, &out); err != nil {
				return errResult(err), nil
			}
			return jsonResult(out), nil
		})

	add("shell_close", "Close an interactive shell session; frees the slot.", `{"type":"object","properties":{"sid":{"type":"string"},"target":{"type":"string"}},"required":["sid"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target string `json:"target"`
				Sid    string `json:"sid"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var out wire.ShellCloseResult
			if err := s.cli.Call(ctx, "shell_close", hub.ShellCloseParams{OpSession: s.cfg.OpSession, Target: t, Sid: p.Sid}, &out); err != nil {
				return errResult(err), nil
			}
			return jsonResult(out), nil
		})

	add("signal", "Send a signal to an interactive shell: interrupt (Ctrl-C) | terminate | force_kill | quit.", `{"type":"object","properties":{"sid":{"type":"string"},"target":{"type":"string"},"sig":{"type":"string","enum":["interrupt","terminate","force_kill","quit"]}},"required":["sid","sig"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target string `json:"target"`
				Sid    string `json:"sid"`
				Sig    string `json:"sig"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var ack map[string]any
			if err := s.cli.Call(ctx, "signal", hub.SignalParams{OpSession: s.cfg.OpSession, Target: t, Sid: p.Sid, Sig: p.Sig}, &ack); err != nil {
				return errResult(err), nil
			}
			return jsonResult(ack), nil
		})

	add("upload", "Transfer a file or directory from the operator (hub) local disk to the target. scp-style: pass two paths plus is_dir; the file BYTES never enter this context. is_dir=true streams the directory as a tar (recursive, like scp -r); is_dir=false sends a single file verbatim (a real .tar file is treated as a normal file, not unpacked). Use exec(ls/stat) for listing/stat.", `{"type":"object","properties":{"local_path":{"type":"string"},"remote_path":{"type":"string"},"is_dir":{"type":"boolean"},"target":{"type":"string"}},"required":["local_path","remote_path","is_dir"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target     string `json:"target"`
				LocalPath  string `json:"local_path"`
				RemotePath string `json:"remote_path"`
				IsDir      bool   `json:"is_dir"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var out hub.TransferResult
			if err := s.cli.Call(ctx, "upload", hub.UploadParams{OpSession: s.cfg.OpSession, Target: t, LocalPath: p.LocalPath, RemotePath: p.RemotePath, IsDir: p.IsDir}, &out); err != nil {
				return errResult(err), nil
			}
			return transferResult("uploaded", p.LocalPath, p.RemotePath, out), nil
		})

	add("download", "Transfer a file or directory from the target to the operator (hub) local disk. scp-style: pass two paths plus is_dir; the file BYTES never enter this context. is_dir=true streams the directory as a tar (recursive, like scp -r); is_dir=false fetches a single file verbatim.", `{"type":"object","properties":{"remote_path":{"type":"string"},"local_path":{"type":"string"},"is_dir":{"type":"boolean"},"target":{"type":"string"}},"required":["remote_path","local_path","is_dir"]}`,
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var p struct {
				Target     string `json:"target"`
				RemotePath string `json:"remote_path"`
				LocalPath  string `json:"local_path"`
				IsDir      bool   `json:"is_dir"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &p)
			t, err := s.resolveTarget(p.Target)
			if err != nil {
				return errResult(err), nil
			}
			var out hub.TransferResult
			if err := s.cli.Call(ctx, "download", hub.DownloadParams{OpSession: s.cfg.OpSession, Target: t, RemotePath: p.RemotePath, LocalPath: p.LocalPath, IsDir: p.IsDir}, &out); err != nil {
				return errResult(err), nil
			}
			return transferResult("downloaded", p.RemotePath, p.LocalPath, out), nil
		})

	add("list_sessions", "List all active shell sessions across targets (with op_session attribution).", `{"type":"object"}`,
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var out []wire.SessionInfo
			if err := s.cli.Call(ctx, "list_sessions", nil, &out); err != nil {
				return errResult(err), nil
			}
			return jsonResult(out), nil
		})

	add("status", "Hub-wide occupancy snapshot.", `{"type":"object"}`,
		func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var out hub.StatusInfo
			if err := s.cli.Call(ctx, "status", nil, &out); err != nil {
				return errResult(err), nil
			}
			return jsonResult(out), nil
		})
}

// --- result helpers ---

func textResult(s string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
		IsError: isErr,
	}
}

func jsonResult(v any) *mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}
}

func errResult(err error) *mcp.CallToolResult {
	return textResult("error: "+err.Error(), true)
}

func execResult(r wire.ExecResult) *mcp.CallToolResult {
	summary := fmt.Sprintf("exit=%d completion=%s\n--- stdout ---\n%s\n--- stderr ---\n%s",
		r.ExitCode, r.Completion, r.Stdout, r.Stderr)
	b, _ := json.Marshal(r)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
			&mcp.TextContent{Text: string(b)},
		},
		IsError: r.ExitCode != 0,
	}
}

// transferResult 把一次 upload/download 的结果渲染成人类可读摘要 + 原始 JSON。
// verb = "uploaded" / "downloaded"; src/dst 是两个路径。
func transferResult(verb, src, dst string, r hub.TransferResult) *mcp.CallToolResult {
	summary := fmt.Sprintf("%s %s -> %s (size=%d sha256=%s", verb, src, dst, r.Size, r.Sha256)
	if r.NEntries > 0 {
		summary += fmt.Sprintf(" entries=%d", r.NEntries)
	}
	summary += fmt.Sprintf(" %dms)", r.DurationMs)
	if r.Err != "" {
		summary += "\nERROR: " + r.Err
		if len(r.Entries) > 0 {
			summary += "\npartial entries already written: " + strings.Join(r.Entries, ", ")
		}
	}
	b, _ := json.Marshal(r)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
			&mcp.TextContent{Text: string(b)},
		},
		IsError: r.Err != "",
	}
}

func defaultOpSession() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "shim"
	}
	return host + "-pid" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano()&0xffff, 16)
}
