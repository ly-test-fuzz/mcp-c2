package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/debugmcp/mcp-c2/internal/proto"
	"github.com/gorilla/websocket"
)

// ClientDialer dials the C2 server and runs the heartbeat/read loop.
type ClientDialer struct {
	ServerURL    string
	TLSConfig    *tls.Config
	Hostname     string
	ClientIDHint string
	OnAuth       func(ok bool, clientID, message string)
	OnFrame      func(f *proto.Frame, send func(proto.FrameType, any) error, reply func(proto.FrameType, any) error)
	OnError      func(err error)
}

func (d *ClientDialer) Dial(ctx context.Context) error {
	u, err := url.Parse(d.ServerURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}
	host := u.Host
	path := u.Path
	if path == "" {
		path = "/c2"
	}
	wsURL := fmt.Sprintf("%s://%s%s", scheme, host, path)

	dialer := websocket.Dialer{
		TLSClientConfig:  d.TLSConfig,
		HandshakeTimeout: 10 * time.Second,
	}
	hdr := http.Header{}
	hdr.Set("User-Agent", "mcp-c2-client/0.1 ("+runtime.GOOS+"/"+runtime.GOARCH+")")

	ws, resp, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial: %w (status %d)", err, resp.StatusCode)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.Close()

	conn := NewConn(ws)
	if d.OnError == nil {
		d.OnError = func(error) {}
	}
	procStart := time.Now()

	// Send HELLO.
	hello, err := proto.NewFrame(proto.FrameHello, proto.HelloPayload{
		ClientID: d.ClientIDHint,
		Hostname: d.Hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Caps:     map[string]bool{"conpty": runtime.GOOS == "windows", "pty": runtime.GOOS != "windows"},
	})
	if err != nil {
		return err
	}
	if err := conn.SendJSON(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Heartbeat ticker.
	var writeMu sync.Mutex
	heartbeat := time.NewTicker(HeartbeatInterval)
	defer heartbeat.Stop()
	ping := time.NewTicker(HeartbeatInterval / 2)
	defer ping.Stop()

	_ = ws.SetReadDeadline(time.Now().Add(HeartbeatTimeout))
	ws.SetPongHandler(func(string) error { _ = ws.SetReadDeadline(time.Now().Add(HeartbeatTimeout)); return nil })

	errCh := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			var f proto.Frame
			if err := json.Unmarshal(data, &f); err != nil {
				continue
			}
			switch f.Type {
			case proto.FrameAuth:
				var ap proto.AuthPayload
				_ = json.Unmarshal(f.Payload, &ap)
				if d.OnAuth != nil {
					d.OnAuth(ap.OK, ap.ClientID, ap.Message)
				}
				if !ap.OK {
					errCh <- fmt.Errorf("auth rejected: %s", ap.Message)
					return
				}
			case proto.FrameAck:
				// heartbeat ack; ignore
			default:
				if d.OnFrame != nil {
					reply := func(t proto.FrameType, payload any) error { return conn.SendFrameWithID(f.ID, t, payload) }
					d.OnFrame(&f, conn.SendFrame, reply)
				}
			}
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				writeMu.Lock()
				_ = ws.SetWriteDeadline(time.Now().Add(WriteTimeout))
				_ = ws.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
			case <-heartbeat.C:
				f, err := proto.NewFrame(proto.FrameHeartbeat, proto.HeartbeatPayload{UptimeSeconds: int64(time.Since(procStart).Seconds())})
				if err == nil {
					writeMu.Lock()
					_ = conn.SendJSON(f)
					writeMu.Unlock()
				}
			}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, websocket.ErrCloseSent) || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			return nil
		}
		return err
	}
}
