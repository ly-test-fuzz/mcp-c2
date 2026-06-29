package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/debugmcp/mcp-c2/internal/proto"
	"github.com/gorilla/websocket"
)

const (
	HeartbeatInterval = 15 * time.Second
	HeartbeatTimeout  = 30 * time.Second
	WriteTimeout      = 10 * time.Second
)

type Conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func NewConn(ws *websocket.Conn) *Conn {
	return &Conn{ws: ws}
}

func (c *Conn) SendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(WriteTimeout))
	return c.ws.WriteMessage(websocket.BinaryMessage, b)
}

func (c *Conn) SendFrame(frameType proto.FrameType, payload any) error {
	f, err := proto.NewFrame(frameType, payload)
	if err != nil {
		return err
	}
	return c.SendJSON(f)
}

func (c *Conn) SendFrameWithID(id string, frameType proto.FrameType, payload any) error {
	f, err := proto.NewFrame(frameType, payload)
	if err != nil {
		return err
	}
	f.ID = id
	return c.SendJSON(f)
}

func (c *Conn) Close() error { return c.ws.Close() }

// ClientSession represents a connected C2 client on the server side.
type ClientSession struct {
	ID       string
	Conn     *Conn
	Summary  proto.ClientSummary
	LastSeen time.Time

	mu     sync.RWMutex
	closed bool
}

func (s *ClientSession) Touch() {
	s.mu.Lock()
	s.LastSeen = time.Now()
	s.Summary.LastSeenUnix = s.LastSeen.Unix()
	s.mu.Unlock()
}

func (s *ClientSession) MarkClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Hub manages connected C2 clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]*ClientSession

	onHello func(cs *ClientSession, hp *proto.HelloPayload)
	onFrame func(cs *ClientSession, f *proto.Frame)
	onGone  func(cs *ClientSession)
	allowed func(fp string) bool
}

func NewHub(allowed func(fp string) bool) *Hub {
	return &Hub{clients: map[string]*ClientSession{}, allowed: allowed}
}

func (h *Hub) SetHandlers(onHello func(*ClientSession, *proto.HelloPayload), onFrame func(*ClientSession, *proto.Frame), onGone func(*ClientSession)) {
	h.onHello = onHello
	h.onFrame = onFrame
	h.onGone = onGone
}

func (h *Hub) Register(cs *ClientSession) {
	h.mu.Lock()
	h.clients[cs.ID] = cs
	h.mu.Unlock()
}

func (h *Hub) Unregister(id string) *ClientSession {
	h.mu.Lock()
	cs := h.clients[id]
	delete(h.clients, id)
	h.mu.Unlock()
	return cs
}

func (h *Hub) Get(id string) (*ClientSession, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	cs, ok := h.clients[id]
	return cs, ok
}

func (h *Hub) List() []proto.ClientSummary {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]proto.ClientSummary, 0, len(h.clients))
	for _, cs := range h.clients {
		cs.mu.RLock()
		s := cs.Summary
		s.LastSeenUnix = cs.LastSeen.Unix()
		cs.mu.RUnlock()
		out = append(out, s)
	}
	return out
}

func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := NewConn(ws)

	var fp string
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		fp = FingerprintSHA256(r.TLS.PeerCertificates[0].Raw)
	}
	if h.allowed != nil && !h.allowed(fp) {
		_ = conn.SendFrame(proto.FrameError, proto.ErrorPayload{Code: "denied", Message: "client certificate not allowed"})
		_ = conn.Close()
		return
	}

	cs := &ClientSession{Conn: conn, LastSeen: time.Now()}
	go h.readLoop(ws, conn, cs, fp)
}

func (h *Hub) readLoop(ws *websocket.Conn, conn *Conn, cs *ClientSession, fp string) {
	defer func() {
		cs.MarkClosed()
		if cs.ID != "" {
			if old := h.Unregister(cs.ID); old != nil && h.onGone != nil {
				h.onGone(old)
			}
		}
		_ = conn.Close()
	}()

	_ = ws.SetReadDeadline(time.Now().Add(HeartbeatTimeout))
	ws.SetPongHandler(func(string) error {
		_ = ws.SetReadDeadline(time.Now().Add(HeartbeatTimeout))
		cs.Touch()
		return nil
	})

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var f proto.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			_ = conn.SendFrame(proto.FrameError, proto.ErrorPayload{Code: "bad_frame", Message: err.Error()})
			continue
		}
		cs.Touch()
		switch f.Type {
		case proto.FrameHello:
			var hp proto.HelloPayload
			if err := json.Unmarshal(f.Payload, &hp); err != nil {
				_ = conn.SendFrame(proto.FrameError, proto.ErrorPayload{Code: "bad_hello", Message: err.Error()})
				continue
			}
			if hp.ClientID == "" {
				hp.ClientID = proto.NewID()
			}
			cs.mu.Lock()
			cs.ID = hp.ClientID
			cs.Summary = proto.ClientSummary{ClientID: hp.ClientID, Hostname: hp.Hostname, OS: hp.OS, Arch: hp.Arch, CertFingerprint: fp, Caps: hp.Caps, Meta: hp.Meta, LastSeenUnix: time.Now().Unix()}
			cs.mu.Unlock()
			h.Register(cs)
			if h.onHello != nil {
				h.onHello(cs, &hp)
			}
			_ = conn.SendFrame(proto.FrameAuth, proto.AuthPayload{OK: true, ClientID: hp.ClientID, Fingerprint: fp})
		case proto.FrameHeartbeat:
			_ = conn.SendFrame(proto.FrameAck, proto.AckPayload{ForFrameID: f.ID})
		default:
			if cs.ID != "" && h.onFrame != nil {
				h.onFrame(cs, &f)
			}
		}
		_ = ws.SetReadDeadline(time.Now().Add(HeartbeatTimeout))
	}
}

func FingerprintSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
