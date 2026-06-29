// Package hubapi implements the local HTTP client that mcpc2-server uses
// to communicate with the mcpc2-hub daemon. It also defines the C2API
// interface that the MCP server depends on, so the same mcpserver package
// works with both a direct hub reference (embedded mode, for testing) and
// the HTTP proxy (production mode).
package hubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/debugmcp/mcp-c2/internal/outputbuf"
	"github.com/debugmcp/mcp-c2/internal/proto"
)

// ── Shared types ────────────────────────────────────────────────────────

// SessionInfo mirrors remote.SessionInfo so the MCP server doesn't import remote.
type SessionInfo struct {
	ClientID    string `json:"client_id"`
	SessionID   string `json:"session_id"`
	Shell       string `json:"shell"`
	Interactive bool   `json:"interactive"`
	Alive       bool   `json:"alive"`
	CreatedAt   string `json:"created_at"`
	ExitCode    *int   `json:"exit_code,omitempty"`
}

// ── C2API interface ─────────────────────────────────────────────────────
//
// Both the hub HTTP client (Client) and a future in-process adapter
// satisfy this interface, so mcpserver doesn't care where the data
// comes from.

type C2API interface {
	ListClients() ([]proto.ClientSummary, error)
	ListSessions(clientID string) ([]SessionInfo, error)
	OpenSession(clientID, shell string) (SessionInfo, error)
	CloseSession(clientID, sessionID string) error
	RunCommand(clientID, sessionID, command string) (int64, error)
	SendInput(clientID, sessionID, text string, appendNewline bool) (int64, error)
	ReadOutput(clientID, sessionID string, since int64, maxBytes, blockMS int) (outputbuf.ReadResult, error)
	InterruptSession(clientID, sessionID string) error
	ListFiles(clientID, path string) ([]map[string]any, error)
	DownloadFile(clientID, remotePath string) (*proto.FileTransferPayload, error)
	DownloadFiles(clientID string, paths []string) (*proto.FileTransferPayload, error)
	UploadFile(clientID, remotePath string, data []byte, overwrite bool) (*proto.FileTransferPayload, error)
	Health() (map[string]any, error)
}

// ── HTTP client ─────────────────────────────────────────────────────────

// Client talks to the mcpc2-hub local HTTP API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Client pointed at the hub's HTTP API.
// hubAddr is "127.0.0.1:9000" (host:port only).
func NewClient(hubAddr string) *Client {
	return &Client{
		baseURL: "http://" + hubAddr,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Ping checks whether the hub is reachable.
func (c *Client) Ping(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/health", nil)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("hub at %s is not reachable: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned status %d", resp.StatusCode)
	}
	return nil
}

// ── C2API implementation ────────────────────────────────────────────────

func (c *Client) Health() (map[string]any, error) {
	var out map[string]any
	return out, c.get("/api/v1/health", &out)
}

func (c *Client) ListClients() ([]proto.ClientSummary, error) {
	var out []proto.ClientSummary
	return out, c.get("/api/v1/clients", &out)
}

func (c *Client) ListSessions(clientID string) ([]SessionInfo, error) {
	var out []SessionInfo
	return out, c.get("/api/v1/sessions?client_id="+clientID, &out)
}

func (c *Client) OpenSession(clientID, shell string) (SessionInfo, error) {
	var out SessionInfo
	return out, c.post("/api/v1/sessions", map[string]string{
		"client_id": clientID,
		"shell":     shell,
	}, &out)
}

func (c *Client) CloseSession(clientID, sessionID string) error {
	return c.del("/api/v1/sessions/" + sessionID + "?client_id=" + clientID)
}

func (c *Client) RunCommand(clientID, sessionID, command string) (int64, error) {
	var out struct {
		OutputCursor int64 `json:"output_cursor"`
	}
	err := c.post("/api/v1/sessions/"+sessionID+"/cmd?client_id="+clientID, map[string]any{
		"command":        command,
		"append_newline": true,
	}, &out)
	return out.OutputCursor, err
}

func (c *Client) SendInput(clientID, sessionID, text string, appendNewline bool) (int64, error) {
	var out struct {
		OutputCursor int64 `json:"output_cursor"`
	}
	err := c.post("/api/v1/sessions/"+sessionID+"/input?client_id="+clientID, map[string]any{
		"text":           text,
		"append_newline": appendNewline,
	}, &out)
	return out.OutputCursor, err
}

func (c *Client) ReadOutput(clientID, sessionID string, since int64, maxBytes, blockMS int) (outputbuf.ReadResult, error) {
	url := fmt.Sprintf("/api/v1/sessions/%s/output?client_id=%s&since=%d&max_bytes=%d&block_ms=%d",
		sessionID, clientID, since, maxBytes, blockMS)
	var out outputbuf.ReadResult
	return out, c.get(url, &out)
}

func (c *Client) InterruptSession(clientID, sessionID string) error {
	return c.post("/api/v1/sessions/"+sessionID+"/interrupt?client_id="+clientID, nil, nil)
}

func (c *Client) ListFiles(clientID, path string) ([]map[string]any, error) {
	var out struct {
		Files []map[string]any `json:"files"`
		Path  string            `json:"path"`
	}
	err := c.get("/api/v1/files/list?client_id="+clientID+"&path="+path, &out)
	return out.Files, err
}

func (c *Client) DownloadFile(clientID, remotePath string) (*proto.FileTransferPayload, error) {
	var out proto.FileTransferPayload
	return &out, c.post("/api/v1/files/download?client_id="+clientID, map[string]string{
		"remote_path": remotePath,
	}, &out)
}

func (c *Client) DownloadFiles(clientID string, paths []string) (*proto.FileTransferPayload, error) {
	var out proto.FileTransferPayload
	return &out, c.post("/api/v1/files/download-batch?client_id="+clientID, map[string]any{
		"paths": paths,
	}, &out)
}

func (c *Client) UploadFile(clientID, remotePath string, data []byte, overwrite bool) (*proto.FileTransferPayload, error) {
	var out proto.FileTransferPayload
	return &out, c.post("/api/v1/files/upload?client_id="+clientID, map[string]any{
		"remote_path": remotePath,
		"data":        data,
		"overwrite":   overwrite,
	}, &out)
}

// ── HTTP helpers ────────────────────────────────────────────────────────

func (c *Client) get(path string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) post(path string, body any, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) del(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("hub api error (%d): %s", resp.StatusCode, string(body))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
