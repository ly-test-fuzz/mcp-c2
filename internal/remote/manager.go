package remote

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/debugmcp/mcp-c2/internal/outputbuf"
	"github.com/debugmcp/mcp-c2/internal/proto"
	"github.com/debugmcp/mcp-c2/internal/transport"
)

type SessionInfo struct {
	ClientID    string `json:"client_id"`
	SessionID   string `json:"session_id"`
	Shell       string `json:"shell"`
	Interactive bool   `json:"interactive"`
	Alive       bool   `json:"alive"`
	CreatedAt   string `json:"created_at"`
	ExitCode    *int   `json:"exit_code,omitempty"`
}

type remoteSession struct {
	info SessionInfo
	buf  *outputbuf.Ring
}

type Manager struct {
	hub      *transport.Hub
	mu       sync.RWMutex
	sessions map[string]*remoteSession
	pending  map[string]chan proto.Frame
}

func NewManager(hub *transport.Hub) *Manager {
	return &Manager{hub: hub, sessions: map[string]*remoteSession{}, pending: map[string]chan proto.Frame{}}
}

func key(clientID, sessionID string) string { return clientID + ":" + sessionID }

func (m *Manager) List(clientID string) []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []SessionInfo{}
	for _, s := range m.sessions {
		if s.info.ClientID == clientID {
			out = append(out, s.info)
		}
	}
	return out
}

func (m *Manager) Open(clientID, shell string) (SessionInfo, error) {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return SessionInfo{}, fmt.Errorf("client %s offline", clientID)
	}
	sid := proto.NewID()
	resp, err := m.sendAndWait(cs, proto.FrameSessionOpen, proto.SessionOpenPayload{SessionID: sid, Shell: shell}, proto.FrameSessionOpen, 10*time.Second)
	if err != nil {
		return SessionInfo{}, err
	}
	var r proto.SessionOpenResult
	if err := json.Unmarshal(resp.Payload, &r); err != nil {
		return SessionInfo{}, err
	}
	info := SessionInfo{ClientID: clientID, SessionID: r.SessionID, Shell: shell, Interactive: r.Interactive, Alive: true, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	m.mu.Lock()
	m.sessions[key(clientID, r.SessionID)] = &remoteSession{info: info, buf: outputbuf.New(1024 * 1024)}
	m.mu.Unlock()
	return info, nil
}

func (m *Manager) RunCommand(clientID, sessionID, command string) (int64, error) {
	return m.SendInput(clientID, sessionID, command, true)
}

func (m *Manager) SendInput(clientID, sessionID, text string, appendNewline bool) (int64, error) {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return 0, fmt.Errorf("client %s offline", clientID)
	}
	m.mu.RLock()
	rs := m.sessions[key(clientID, sessionID)]
	m.mu.RUnlock()
	if rs == nil {
		return 0, fmt.Errorf("session %s not found", sessionID)
	}
	_, latest := rs.buf.Cursors()
	_, err := m.sendAndWait(cs, proto.FrameCmdInput, proto.CommandInputPayload{SessionID: sessionID, Text: text, AppendNewline: appendNewline}, proto.FrameAck, 10*time.Second)
	return latest, err
}

func (m *Manager) ReadOutput(clientID, sessionID string, since int64, maxBytes int) (outputbuf.ReadResult, error) {
	m.mu.RLock()
	rs := m.sessions[key(clientID, sessionID)]
	m.mu.RUnlock()
	if rs == nil {
		return outputbuf.ReadResult{RequestedSince: since, SinceStatus: outputbuf.SinceInvalidSession}, fmt.Errorf("session %s not found", sessionID)
	}
	res := rs.buf.Read(since, maxBytes)
	res.Alive = rs.info.Alive
	res.ExitCode = rs.info.ExitCode
	return res, nil
}

func (m *Manager) Interrupt(clientID, sessionID string) error {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return fmt.Errorf("client %s offline", clientID)
	}
	_, err := m.sendAndWait(cs, proto.FrameInterrupt, proto.SessionClosePayload{SessionID: sessionID}, proto.FrameAck, 10*time.Second)
	return err
}

func (m *Manager) Close(clientID, sessionID string) error {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return fmt.Errorf("client %s offline", clientID)
	}
	_, err := m.sendAndWait(cs, proto.FrameSessionClose, proto.SessionClosePayload{SessionID: sessionID}, proto.FrameAck, 10*time.Second)
	m.mu.Lock()
	if rs := m.sessions[key(clientID, sessionID)]; rs != nil {
		rs.info.Alive = false
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) ListFiles(clientID, path string) ([]map[string]any, error) {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return nil, fmt.Errorf("client %s offline", clientID)
	}
	resp, err := m.sendAndWait(cs, proto.FrameFileUpload, map[string]string{"path": path, "command": "list"}, proto.FrameFileAck, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var files []map[string]any
	_ = json.Unmarshal(resp.Payload, &files)
	return files, nil
}

func (m *Manager) Download(clientID, remotePath string) (*proto.FileTransferPayload, error) {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return nil, fmt.Errorf("client %s offline", clientID)
	}
	resp, err := m.sendAndWait(cs, proto.FrameFileDownload, map[string]string{"command": "download", "path": remotePath}, proto.FrameFileDownload, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var ftp proto.FileTransferPayload
	_ = json.Unmarshal(resp.Payload, &ftp)
	return &ftp, nil
}

func (m *Manager) RecursiveDownload(clientID string, paths []string) (*proto.FileTransferPayload, error) {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return nil, fmt.Errorf("client %s offline", clientID)
	}
	resp, err := m.sendAndWait(cs, proto.FrameDirDownload, proto.DirDownloadPayload{
		TransferID: proto.NewID(),
		Paths:      paths,
	}, proto.FrameFileAck, 120*time.Second)
	if err != nil {
		return nil, err
	}
	var ftp proto.FileTransferPayload
	_ = json.Unmarshal(resp.Payload, &ftp)
	return &ftp, nil
}

func (m *Manager) Upload(clientID, remotePath string, data []byte, overwrite bool) (*proto.FileTransferPayload, error) {
	cs, ok := m.hub.Get(clientID)
	if !ok {
		return nil, fmt.Errorf("client %s offline", clientID)
	}
	cmd := map[string]any{"command": "upload", "path": remotePath, "data": data, "overwrite": overwrite}
	resp, err := m.sendAndWait(cs, proto.FrameFileUpload, cmd, proto.FrameFileAck, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var ftp proto.FileTransferPayload
	_ = json.Unmarshal(resp.Payload, &ftp)
	return &ftp, nil
}

func (m *Manager) HandleFrame(cs *transport.ClientSession, f *proto.Frame) {
	switch f.Type {
	case proto.FrameOutputChunk:
		var p proto.OutputChunkPayload
		if err := json.Unmarshal(f.Payload, &p); err != nil {
			return
		}
		m.mu.Lock()
		rs := m.sessions[key(cs.ID, p.SessionID)]
		if rs != nil {
			if len(p.Data) > 0 {
				rs.buf.Write(p.Data)
			}
			rs.info.Alive = p.Alive
			rs.info.ExitCode = p.ExitCode
		}
		m.mu.Unlock()
	case proto.FrameSessionOpen, proto.FrameAck, proto.FrameAlive, proto.FrameError,
		proto.FrameFileAck, proto.FrameFileDownload, proto.FrameFileUpload:
		m.mu.Lock()
		ch := m.pending[f.ID]
		m.mu.Unlock()
		if ch != nil {
			select {
			case ch <- *f:
			default:
			}
		}
	}
}

func (m *Manager) sendAndWait(cs *transport.ClientSession, typ proto.FrameType, payload any, expect proto.FrameType, timeout time.Duration) (proto.Frame, error) {
	frame, err := proto.NewFrame(typ, payload)
	if err != nil {
		return proto.Frame{}, err
	}
	ch := make(chan proto.Frame, 1)
	m.mu.Lock()
	m.pending[frame.ID] = ch
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.pending, frame.ID); m.mu.Unlock() }()
	if err := cs.Conn.SendJSON(frame); err != nil {
		return proto.Frame{}, err
	}
	select {
	case resp := <-ch:
		if resp.Type == proto.FrameError {
			var ep proto.ErrorPayload
			_ = json.Unmarshal(resp.Payload, &ep)
			return resp, fmt.Errorf("%s: %s", ep.Code, ep.Message)
		}
		if resp.Type != expect {
			return resp, fmt.Errorf("unexpected response %s", resp.Type)
		}
		return resp, nil
	case <-time.After(timeout):
		return proto.Frame{}, fmt.Errorf("timeout waiting for %s", expect)
	}
}
