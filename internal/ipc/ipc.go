// Package ipc is the shim<->hub control-plane link: a length-prefixed JSON-RPC
// over a Unix socket (or named pipe), authenticated by a local MAC token the hub
// writes to a 0600 file at start. This closes the Principle #1 gap: a non-owner
// local process cannot call hub tools.
package ipc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxMsg    = 384 << 20 // 384 MiB — historical limit (file_read/file_write once passed full payload over IPC). upload/download now carry only paths + result metadata; left generously.
	handshake = "DBGMCP-IPC-v1"
)

// Handler is implemented by the hub: dispatch a method to a JSON-marshalable result.
type Handler interface {
	Handle(method string, params json.RawMessage) (any, error)
}

// AuthMsg is the first frame from client to server.
type AuthMsg struct {
	Auth string `json:"auth"`
}

// AckMsg is the server's auth reply.
type AckMsg struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Req is a client JSON-RPC request.
type Req struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Resp is a server JSON-RPC response.
type Resp struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxMsg {
		return fmt.Errorf("ipc: message too large (%d)", len(b))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func readJSON(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxMsg {
		return fmt.Errorf("ipc: bad frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// Serve runs the JSON-RPC server on ln, requiring `token` for auth. Blocking.
func Serve(ctx context.Context, ln net.Listener, token string, h Handler) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var auth AuthMsg
			if err := readJSON(c, &auth); err != nil {
				return
			}
			if auth.Auth != token {
				_ = writeJSON(c, AckMsg{OK: false, Error: "bad token"})
				return
			}
			if err := writeJSON(c, AckMsg{OK: true}); err != nil {
				return
			}
			c.SetDeadline(time.Time{})
			serveConn(c, h)
		}(nc)
	}
}

func serveConn(c net.Conn, h Handler) {
	for {
		var req Req
		if err := readJSON(c, &req); err != nil {
			return
		}
		go func(r Req) {
			resp := Resp{ID: r.ID}
			if res, err := h.Handle(r.Method, r.Params); err != nil {
				resp.Error = err.Error()
			} else if res != nil {
				b, err := json.Marshal(res)
				if err != nil {
					resp.Error = fmt.Sprintf("ipc: marshal result: %v", err)
				} else {
					resp.Result = b
				}
			}
			_ = writeJSON(c, resp)
		}(req)
	}
}

// Client is a shim-side JSON-RPC client.
type Client struct {
	conn    net.Conn
	mu      sync.Mutex
	wmu     sync.Mutex
	nextID  uint64
	pending map[uint64]chan Resp
	done    chan struct{}
}

// Dial connects to the Unix socket at path and authenticates with token.
func Dial(ctx context.Context, network, address, token string) (*Client, error) {
	d := net.Dialer{}
	nc, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeJSON(nc, AuthMsg{Auth: token}); err != nil {
		nc.Close()
		return nil, err
	}
	var ack AckMsg
	if err := readJSON(nc, &ack); err != nil {
		nc.Close()
		return nil, err
	}
	if !ack.OK {
		nc.Close()
		return nil, fmt.Errorf("ipc: auth rejected: %s", ack.Error)
	}
	nc.SetDeadline(time.Time{})
	c := &Client{conn: nc, pending: make(map[uint64]chan Resp), done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	for {
		var resp Resp
		if err := readJSON(c.conn, &resp); err != nil {
			c.fail(err)
			return
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			select {
			case ch <- resp:
			default:
			}
		}
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.mu.Unlock()
	close(c.done)
}

// Call invokes method with params (JSON-marshalable) and unmarshals result into out.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	id := atomic.AddUint64(&c.nextID, 1)
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	ch := make(chan Resp, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	c.wmu.Lock()
	err := writeJSON(c.conn, Req{ID: id, Method: method, Params: raw})
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("ipc: decode result: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("ipc: connection closed")
	}
}

// Close closes the client.
func (c *Client) Close() error {
	return c.conn.Close()
}
