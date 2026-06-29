package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/debugmcp/mcp-c2/internal/hubapi"
	"github.com/debugmcp/mcp-c2/internal/outputbuf"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Version = "0.2.0"

// SystemPrompt is injected via the server's Instructions capability.
// The AI host surfaces this as system-level guidance before any tool call.
const SystemPrompt = `You are operating through MCP-C2, a secure remote command-and-file access channel.

## Your role
You have authenticated access to one or more remote machines (clients). Each client runs a lightweight agent that connects back to this server. You can open shell sessions, run commands interactively, read output, and transfer files — all through the tools below.

## Core workflow
1. list_clients  — discover which remote machines are online and their capabilities (OS, PTY/conpty, hostname)
2. start_session — open a shell on a chosen client (bash/sh/powershell/cmd). Returns interactive=true if PTY is available, false if fallback pipe mode.
3. run_command   — send a command to the session. Returns immediately with an output_cursor. The command runs asynchronously.
4. read_output   — poll for new output since the cursor. Use block_ms=0 for non-blocking reads. When alive becomes false, the command (or session) has ended and exit_code will be set.
5. close_session — clean up when done.

## Important rules
- ALWAYS call list_clients before choosing a client_id. A client that was online moments ago may have disconnected.
- For a single session, you can use block_ms=2000-5000 on read_output to long-poll. For monitoring multiple sessions concurrently, use block_ms=0 to avoid serial blocking.
- When since_status is "expired", some output was lost (ring buffer overflow). The data returned starts from the earliest available cursor. Note missed_bytes to understand the gap. Consider re-running the command if the gap is critical.
- When since_status is "future", you requested a cursor beyond the current output. Retry with the current earliest_cursor from the response.
- When interactive=false, the session is running in non-PTY pipe mode — tab completion, colors, and interactive prompts (like "Are you sure?") will not work. Commands still execute normally.
- When a session has been idle for a long time or the client disconnects, the session is lost. Check is_alive if unsure.
- close_session when you are done. Orphaned sessions consume resources on both client and server.
- Do NOT run commands that would start long-running interactive TUI programs (vim, htop, top) without a clear exit strategy. They will produce ANSI escape sequences that are hard to parse. Use non-interactive alternatives (cat, ps, grep).
- file operations (upload_file, download_file, list_files) use the client's filesystem. Downloaded files are returned inline in the tool result. Large files (tens of MB) may be slow over the C2 channel — consider compressing first.
- All actions are audited. Operate as if every command is logged.

## Understanding the output
- client_id uniquely identifies a connected machine (usually hostname-os). Use this exact string in all per-client tool calls.
- session_id uniquely identifies a shell session on a client. One client can have multiple sessions.
- output_cursor is a monotonically increasing byte offset. Pass it as the 'since' parameter in read_output to get only new output.
- alive=false means the command or shell has exited. exit_code will be set (0 = success, non-zero = error).
- since_status values: "ok" (normal), "expired" (some output lost, read from earliest_cursor), "future" (bad cursor, use earliest_cursor)
- truncated_by values: "ring_buffer" (old data discarded by buffer limit), "max_bytes" (you requested a byte limit)

## Typical session example
1. list_clients → pick a client_id
2. start_session(client_id, shell="bash") → get session_id, note interactive status
3. run_command(client_id, session_id, "ls -la /etc") → get output_cursor
4. read_output(client_id, session_id, since=cursor) → get output, note new_cursor
5. Repeat step 4 until alive=false or exit_code is set
6. close_session(client_id, session_id) to clean up
`

// New constructs the stdio-facing MCP server for AI hosts.
// It takes a C2API implementation — in production this is a hubapi.Client
// that talks to the mcpc2-hub daemon over local HTTP.
func New(api hubapi.C2API) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-c2",
		Title:   "MCP-C2 Remote Debug Assistant",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: SystemPrompt,
	})

	// --- Discovery tools ---

	addTool(s, "list_clients",
		`List all currently connected remote clients (target machines).
Call this FIRST before any other tool — you need a client_id to operate on.
Returns: client_id, hostname, OS, architecture, connection uptime, PTY/conpty capabilities.
Use the returned client_id exactly as-is in all subsequent tool calls.`,
		noArgsSchema(),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			clients, err := api.ListClients()
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{"clients": clients}, nil
		})

	addTool(s, "list_sessions",
		`List active shell sessions on a specific client.
Returns: session_id, shell type, interactive (true=PTY, false=pipe), alive status, created_at.
If a session you expected is missing, it may have been closed or the client may have disconnected.`,
		schema(map[string]any{"client_id": str("Client ID from list_clients — use the exact string returned")}, "client_id"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var in struct{ ClientID string `json:"client_id"` }
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			sessions, err := api.ListSessions(in.ClientID)
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{"sessions": sessions}, nil
		})

	// --- Session lifecycle ---

	addTool(s, "start_session",
		`Open a new shell session on a remote client.
Choose the shell based on the client OS: use /bin/bash or /bin/sh for Linux, powershell or cmd for Windows.
Returns a session_id to use in run_command/read_output/send_input/close_session.
If interactive=false (pipe mode), tab completion and colors are unavailable but commands still execute normally.
If this fails with "client offline", the client may have disconnected — check list_clients.`,
		schema(map[string]any{
			"client_id": str("Client ID from list_clients"),
			"shell":     str("Shell: /bin/bash, /bin/sh, powershell, or cmd. Auto-selects bash/cmd if empty"),
		}, "client_id", "shell"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var raw map[string]string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			info, err := api.OpenSession(raw["client_id"], raw["shell"])
			if err != nil {
				return nil, wrapClientError(err)
			}
			progress(ctx, req, 1, 1, "session opened")
			return info, nil
		})

	addTool(s, "close_session",
		`Close a shell session and terminate its process tree on the remote client.
Always call this when you are done with a session to free resources.
Closed sessions cannot be reopened — use start_session to create a new one.`,
		schema(map[string]any{
			"client_id":  str("Client ID"),
			"session_id": str("Session ID to close"),
		}, "client_id", "session_id"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var raw map[string]string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, wrapClientError(api.CloseSession(raw["client_id"], raw["session_id"]))
		})

	// --- Command execution ---

	addTool(s, "run_command",
		`Send a command to a running session. Returns IMMEDIATELY with an output_cursor — the command runs asynchronously.
After calling this, use read_output(since=output_cursor) in a loop to fetch the command's output.
The command is written to the shell's stdin followed by a newline (like pressing Enter).
For commands that need interactive input, use send_input after run_command.

IMPORTANT: Do NOT wait for the command to finish. This tool returns instantly. Poll read_output until alive=false.`,
		schema(map[string]any{
			"client_id":  str("Client ID"),
			"session_id": str("Session ID from start_session"),
			"command":    str("Shell command to execute. Sent to stdin followed by newline."),
		}, "client_id", "session_id", "command"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var raw map[string]string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			cursor, err := api.RunCommand(raw["client_id"], raw["session_id"], raw["command"])
			if err != nil {
				return nil, wrapClientError(err)
			}
			progress(ctx, req, 1, 1, "command submitted")
			return map[string]any{"ok": true, "output_cursor": cursor, "started_at": time.Now().UTC().Format(time.RFC3339)}, nil
		})

	addTool(s, "read_output",
		`Read output from a session since a given cursor position. This is how you see command results.

Key behavior:
- Returns new output since the cursor you specify.
- block_ms=0 (default): non-blocking — returns whatever is available immediately. Use this when monitoring multiple sessions.
- block_ms=2000-5000: long-poll — waits up to block_ms milliseconds for new output before returning. Use this for single-session monitoring.
- alive=false means the command or shell has exited. STOP polling when this happens. exit_code tells you success (0) or failure (non-zero).
- When since_status="expired", some output was permanently lost (ring buffer overflow). The response includes missed_bytes and starts from earliest_cursor.
- When since_status="future", your cursor is ahead of the output stream. Use earliest_cursor from the response to recover.

After reading, use the returned new_cursor as the 'since' value in your next read_output call.`,
		schema(map[string]any{
			"client_id":  str("Client ID"),
			"session_id": str("Session ID"),
			"since":      integer("Cursor position to read from (0 for start, or use output_cursor/new_cursor from previous calls)"),
			"block_ms":   integer("How long to wait for new output in milliseconds. 0=non-blocking, max 5000. Default 0."),
			"max_bytes":  integer("Maximum bytes to return. Limits response size."),
			"strip_ansi": map[string]any{"type": "boolean", "description": "Strip ANSI escape sequences from output. Default true."},
		}, "client_id", "session_id", "since"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var in struct {
				ClientID, SessionID string
				Since               int64
				BlockMS             int
				MaxBytes            int
			}
			var m map[string]any
			if err := json.Unmarshal(args, &m); err != nil {
				return nil, err
			}
			in.ClientID, _ = m["client_id"].(string)
			in.SessionID, _ = m["session_id"].(string)
			in.Since = toInt64(m["since"])
			in.BlockMS = int(toInt64(m["block_ms"]))
			in.MaxBytes = int(toInt64(m["max_bytes"]))
			if in.BlockMS < 0 {
				in.BlockMS = 0
			}
			if in.BlockMS > 5000 {
				in.BlockMS = 5000
			}
			// Long-polling is handled by the hub; we just call ReadOutput with block_ms.
			res, err := api.ReadOutput(in.ClientID, in.SessionID, in.Since, in.MaxBytes, in.BlockMS)
			if err != nil {
				return nil, wrapClientError(err)
			}
			progress(ctx, req, float64(res.NewCursor), float64(res.NewCursor+1), fmt.Sprintf("cursor %d", res.NewCursor))
			return resultMap(res), nil
		})

	addTool(s, "send_input",
		`Send text to a running session's stdin. Use this for interactive commands that need follow-up input.
For example: after run_command("python -i"), use send_input("print(1+1)") to send Python code.
The append_newline parameter controls whether a newline is appended (default true — like pressing Enter).`,
		schema(map[string]any{
			"client_id":      str("Client ID"),
			"session_id":     str("Session ID"),
			"text":           str("Text to send to stdin"),
			"append_newline": map[string]any{"type": "boolean", "description": "Append a newline after the text. Default true."},
		}, "client_id", "session_id", "text"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var m map[string]any
			if err := json.Unmarshal(args, &m); err != nil {
				return nil, err
			}
			clientID, _ := m["client_id"].(string)
			sessionID, _ := m["session_id"].(string)
			text, _ := m["text"].(string)
			appendNewline, _ := m["append_newline"].(bool)
			cursor, err := api.SendInput(clientID, sessionID, text, appendNewline)
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{"ok": true, "output_cursor": cursor}, nil
		})

	addTool(s, "interrupt_session",
		`Send an interrupt signal (Ctrl-C / SIGINT) to a running command in a session.
Use this to stop a long-running command without closing the session.
The session remains open and you can run more commands afterward.`,
		schema(map[string]any{
			"client_id":  str("Client ID"),
			"session_id": str("Session ID"),
		}, "client_id", "session_id"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var raw map[string]string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, wrapClientError(api.InterruptSession(raw["client_id"], raw["session_id"]))
		})

	addTool(s, "is_alive",
		`Check whether a session is still alive.
Returns: alive (bool) and exit_code if the session has ended.
Use this as a quick check — for detailed output, use read_output instead.
If alive=false and you didn't see output confirming completion, the session may have crashed or been closed externally.`,
		schema(map[string]any{
			"client_id":  str("Client ID"),
			"session_id": str("Session ID"),
		}, "client_id", "session_id"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var raw map[string]string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			res, err := api.ReadOutput(raw["client_id"], raw["session_id"], 0, 0, 0)
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{"alive": res.Alive, "exit_code": res.ExitCode}, nil
		})

	// --- File operations ---

	addTool(s, "upload_file",
		`Upload data to a file on a remote client.
The file content must be base64-encoded. Use this to deploy scripts, configs, or binaries to the target machine.
Paths are relative to the client's working directory unless absolute.
Set overwrite=true to replace an existing file.`,
		schema(map[string]any{
			"client_id":      str("Client ID"),
			"remote_path":    str("Destination path on the client"),
			"content_base64": str("File content encoded as base64"),
			"overwrite":      map[string]any{"type": "boolean", "description": "Allow overwriting an existing file. Default true."},
		}, "client_id", "remote_path", "content_base64"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var m map[string]any
			if err := json.Unmarshal(args, &m); err != nil {
				return nil, err
			}
			cid, _ := m["client_id"].(string)
			rp, _ := m["remote_path"].(string)
			b64, _ := m["content_base64"].(string)
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("invalid base64: %w", err)
			}
			_, err = api.UploadFile(cid, rp, data, true)
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{"ok": true, "path": rp, "bytes": len(data)}, nil
		})

	addTool(s, "download_file",
		`Download a file from a remote client.
The file content is returned inline in the response (both as text and base64).
For text files, use the inline_content field. For binary files, decode inline_content_base64.
Large files (tens of MB) may be slow. Consider compressing on the client side first.`,
		schema(map[string]any{
			"client_id":   str("Client ID"),
			"remote_path": str("Path to the file on the client to download"),
		}, "client_id", "remote_path"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var m map[string]string
			if err := json.Unmarshal(args, &m); err != nil {
				return nil, err
			}
			ftp, err := api.DownloadFile(m["client_id"], m["remote_path"])
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{
				"download_id":           ftp.TransferID,
				"inline_content":        string(ftp.Data),
				"inline_content_base64": base64.StdEncoding.EncodeToString(ftp.Data),
				"size":                  len(ftp.Data),
				"path":                  ftp.TempPath,
			}, nil
		})

	addTool(s, "download_files",
		`Download multiple files and/or directories from a remote client as a tar.gz archive.
Specify one or more absolute paths in the 'paths' array. The remote end tars them up and returns the archive inline.
Use this to fetch a whole directory tree or a set of files in one call.
Returns inline_content (text) and inline_content_base64 with the tar.gz data.`,
		schema(map[string]any{
			"client_id": str("Client ID"),
			"paths": map[string]any{
				"type": "array",
				"items": map[string]any{"type": "string"},
				"description": "Absolute file/directory paths to download",
			},
		}, "client_id", "paths"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var m struct {
				ClientID string   `json:"client_id"`
				Paths    []string `json:"paths"`
			}
			if err := json.Unmarshal(args, &m); err != nil {
				return nil, err
			}
			if len(m.Paths) == 0 {
				return nil, fmt.Errorf("at least one path is required")
			}
			ftp, err := api.DownloadFiles(m.ClientID, m.Paths)
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{
				"download_id":           ftp.TransferID,
				"inline_content":        string(ftp.Data),
				"inline_content_base64": base64.StdEncoding.EncodeToString(ftp.Data),
				"size":                  len(ftp.Data),
				"path":                  ftp.TempPath,
			}, nil
		})

	addTool(s, "list_files",
		`List files and directories on a remote client at a given path.
Returns: name, size, and is_dir for each entry.
Use this to explore the client's filesystem before downloading or uploading files.`,
		schema(map[string]any{
			"client_id": str("Client ID"),
			"path":      str("Directory path to list. Use '.' or '/' as needed."),
		}, "client_id", "path"),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			var m map[string]string
			if err := json.Unmarshal(args, &m); err != nil {
				return nil, err
			}
			files, err := api.ListFiles(m["client_id"], m["path"])
			if err != nil {
				return nil, wrapClientError(err)
			}
			return map[string]any{"files": files, "path": m["path"]}, nil
		})

	// --- Health ---

	addTool(s, "health",
		`Return the MCP-C2 server health status and version.
Returns: status "ok" and version string if the server and hub are operational.`,
		noArgsSchema(),
		func(ctx context.Context, req *mcp.CallToolRequest, args json.RawMessage) (any, error) {
			h, err := api.Health()
			if err != nil {
				return map[string]any{"status": "degraded", "version": Version, "error": err.Error()}, nil
			}
			return h, nil
		})

	// --- Resources ---

	s.AddResource(&mcp.Resource{
		URI:         "mcp-c2://status",
		Name:        "server_status",
		Description: "MCP-C2 server status including version and connected client count. Read this to verify the server is operational.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		status := map[string]any{"version": Version}
		if h, err := api.Health(); err == nil {
			status["client_count"] = h["client_count"]
			status["hub"] = "ok"
		} else {
			status["hub"] = "unreachable"
			status["error"] = err.Error()
		}
		b, _ := json.MarshalIndent(status, "", "  ")
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(b),
		}}}, nil
	})

	return s
}

// RunStdio creates a server backed by the given API and runs it on stdio.
func RunStdio(ctx context.Context, api hubapi.C2API) error {
	return New(api).Run(ctx, &mcp.StdioTransport{})
}

// --- helpers ---

type handler func(context.Context, *mcp.CallToolRequest, json.RawMessage) (any, error)

func addTool(s *mcp.Server, name, desc string, inputSchema any, h handler) {
	s.AddTool(&mcp.Tool{Name: name, Description: desc, InputSchema: inputSchema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			out, err := h(ctx, req, req.Params.Arguments)
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
				StructuredContent: out,
			}, nil
		})
}

func noArgsSchema() map[string]any { return schema(map[string]any{}) }
func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}

func progress(ctx context.Context, req *mcp.CallToolRequest, p, total float64, msg string) {
	if tok := req.Params.GetProgressToken(); tok != nil {
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: tok, Progress: p, Total: total, Message: msg,
		})
	}
}

func resultMap(res outputbuf.ReadResult) map[string]any {
	out := map[string]any{
		"output":          string(res.Output),
		"output_base64":   base64.StdEncoding.EncodeToString(res.Output),
		"requested_since": res.RequestedSince,
		"earliest_cursor": res.EarliestCursor,
		"new_cursor":      res.NewCursor,
		"missed_bytes":    res.MissedBytes,
		"since_status":    res.SinceStatus,
		"alive":           res.Alive,
		"redacted_count":  res.RedactedCount,
	}
	if res.TruncatedBy != nil {
		out["truncated_by"] = *res.TruncatedBy
	}
	if res.ExitCode != nil {
		out["exit_code"] = *res.ExitCode
	}
	return out
}

// wrapClientError adds context to common errors so the AI knows how to recover.
func wrapClientError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "offline") || strings.Contains(msg, "not connected") {
		return fmt.Errorf("%w\n  → This client is no longer connected. Run list_clients to find available clients.", err)
	}
	if strings.Contains(msg, "not found") || strings.Contains(msg, "session") {
		return fmt.Errorf("%w\n  → The session may have been closed or never existed. Run list_sessions to check.", err)
	}
	if strings.Contains(msg, "timeout") {
		return fmt.Errorf("%w\n  → The client did not respond in time. It may be overloaded or the network is slow. Retry or check is_alive.", err)
	}
	if strings.Contains(msg, "not reachable") || strings.Contains(msg, "connection refused") {
		return fmt.Errorf("%w\n  → The MCP-C2 hub daemon is not running. Start 'mcpc2-hub' first.", err)
	}
	return err
}
