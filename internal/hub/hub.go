// Package hub is the long-lived operator-side daemon: it accepts authenticated C2
// agents over the transport plane (Noise PSK), holds the agent registry and the
// session/occupancy table, routes shim requests to the right agent as Seq-correlated
// wire RPCs, and audits every operation. The hub never speaks MCP — the shim does.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"debugmcp/internal/audit"
	"debugmcp/internal/crypto"
	"debugmcp/internal/wire"
)

// ErrUnknownTarget / ErrTimeout are surfaced to callers.
var (
	ErrUnknownTarget = errors.New("hub: unknown target")
	ErrTimeout       = errors.New("hub: rpc timeout")
)

// TargetInfo is a target's public view, including live occupancy.
type TargetInfo struct {
	ID             string             `json:"id"`
	Hostname       string             `json:"hostname"`
	Platform       string             `json:"platform"`
	Arch           string             `json:"arch"`
	Shell          string             `json:"shell"`
	Status         string             `json:"status"` // online|offline
	SessionsActive int                `json:"sessions_active"`
	ConcurrencyCap int                `json:"concurrency_cap"`
	Busy           bool               `json:"busy"`
	Sessions       []wire.SessionInfo `json:"sessions"`
}

// StatusInfo is the hub-wide occupancy snapshot.
type StatusInfo struct {
	Targets       int          `json:"targets"`
	TotalSessions int          `json:"total_sessions"`
	TargetsList   []TargetInfo `json:"items"`
}

// Hub holds the agent registry, session table, and audit log.
type Hub struct {
	psk   []byte
	audit *audit.Logger

	mu       sync.Mutex
	agents   map[string]*agentConn  // id -> active connection
	sessions map[string]*sessionRec // sid -> record (occupancy + attribution)
}

type sessionRec struct {
	sid, targetID, opSession string
	createdMs, lastIOMs      int64
}

type agentConn struct {
	id      string
	hello   *wire.Hello
	conn    *crypto.Conn
	mu      sync.Mutex
	pending map[uint64]chan *wire.Envelope
	nextSeq uint64
}

// New constructs a Hub. psk authenticates agents; audit may be nil.
func New(psk []byte, aud *audit.Logger) *Hub {
	if len(psk) < 32 {
		panic("hub: PSK must be >= 32 bytes")
	}
	return &Hub{
		psk:      psk,
		audit:    aud,
		agents:   make(map[string]*agentConn),
		sessions: make(map[string]*sessionRec),
	}
}

// PSK returns the configured PSK (used by cmd to print it at enrollment).
func (h *Hub) PSK() []byte { return h.psk }

// ListenAgents creates the TCP listener for C2 agents.
func (h *Hub) ListenAgents(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// ServeAgentsOn runs the accept loop on an existing listener until ctx is done.
func (h *Hub) ServeAgentsOn(ctx context.Context, ln net.Listener) error {
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
		go h.handleAgent(nc)
	}
}

// ServeAgents is a convenience: listen + serve. Blocking.
func (h *Hub) ServeAgents(ctx context.Context, addr string) error {
	ln, err := h.ListenAgents(addr)
	if err != nil {
		return fmt.Errorf("hub: listen %s: %w", addr, err)
	}
	return h.ServeAgentsOn(ctx, ln)
}

func (h *Hub) handleAgent(nc net.Conn) {
	defer nc.Close()
	conn, err := crypto.Handshake(nc, crypto.Config{PSK: h.psk, Initiator: false})
	if err != nil {
		return // unauthenticated callback dropped (audit could log)
	}
	// First message must be Hello.
	env, err := conn.Recv()
	if err != nil || env.Type != wire.MsgHello {
		_ = conn.Close()
		return
	}
	hello, err := wire.Decode[wire.Hello](env)
	if err != nil {
		_ = conn.Close()
		return
	}
	ac := &agentConn{id: hello.AgentID, hello: hello, conn: conn, pending: make(map[uint64]chan *wire.Envelope)}
	h.register(ac)
	// Welcome
	wenv, _ := wire.Encode(wire.MsgWelcome, env.Seq, "", &wire.Welcome{AgentID: ac.id, Accepted: true})
	_ = conn.Send(wenv)

	// Reader loop: route responses by Seq to pending callers.
	for {
		e, err := conn.Recv()
		if err != nil {
			h.unregister(ac)
			ac.failPending(fmt.Errorf("agent disconnected: %w", err))
			return
		}
		ac.deliver(e)
	}
}

func (h *Hub) register(ac *agentConn) {
	h.mu.Lock()
	h.agents[ac.id] = ac
	h.mu.Unlock()
}

func (h *Hub) unregister(ac *agentConn) {
	h.mu.Lock()
	if h.agents[ac.id] == ac {
		delete(h.agents, ac.id)
	}
	// drop sessions owned by this agent
	for sid, rec := range h.sessions {
		if rec.targetID == ac.id {
			delete(h.sessions, sid)
		}
	}
	h.mu.Unlock()
}

func (ac *agentConn) deliver(env *wire.Envelope) {
	ac.mu.Lock()
	ch, ok := ac.pending[env.Seq]
	if ok {
		delete(ac.pending, env.Seq)
	}
	ac.mu.Unlock()
	if ok {
		select {
		case ch <- env:
		default:
		}
	}
}

func (ac *agentConn) failPending(err error) {
	ac.mu.Lock()
	for seq, ch := range ac.pending {
		delete(ac.pending, seq)
		close(ch)
	}
	ac.mu.Unlock()
}

func (ac *agentConn) removePending(seq uint64) {
	ac.mu.Lock()
	delete(ac.pending, seq)
	ac.mu.Unlock()
}

// call sends a wire request to the agent and waits for the Seq-matched reply.
func (ac *agentConn) call(t wire.MsgType, sid string, body any, timeout time.Duration) (*wire.Envelope, error) {
	ac.mu.Lock()
	ac.nextSeq++
	seq := ac.nextSeq
	ch := make(chan *wire.Envelope, 1)
	ac.pending[seq] = ch
	ac.mu.Unlock()

	env, err := wire.Encode(t, seq, sid, body)
	if err != nil {
		ac.removePending(seq)
		return nil, err
	}
	if err := ac.conn.Send(env); err != nil {
		ac.removePending(seq)
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp == nil {
			return nil, errors.New("hub: agent closed connection")
		}
		return resp, nil
	case <-time.After(timeout):
		ac.removePending(seq)
		return nil, ErrTimeout
	}
}

func (h *Hub) agentOf(id string) (*agentConn, error) {
	h.mu.Lock()
	ac := h.agents[id]
	h.mu.Unlock()
	if ac == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownTarget, id)
	}
	return ac, nil
}

// --- public API (called by the shim via IPC) ---

func (h *Hub) ListTargets() []TargetInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]TargetInfo, 0, len(h.agents))
	for _, ac := range h.agents {
		ti := TargetInfo{
			ID: ac.id, Hostname: ac.hello.Hostname, Platform: ac.hello.Platform,
			Arch: ac.hello.Arch, Shell: ac.hello.Shell, Status: "online",
			ConcurrencyCap: ac.hello.Caps.ConcurrencyCap,
		}
		for _, rec := range h.sessions {
			if rec.targetID == ac.id {
				ti.SessionsActive++
				ti.Sessions = append(ti.Sessions, wire.SessionInfo{
					Sid: rec.sid, OpSession: rec.opSession, State: "running",
					IdleMs: sinceMs(rec.lastIOMs), CreatedMs: rec.createdMs,
				})
			}
		}
		ti.Busy = ti.SessionsActive >= ti.ConcurrencyCap
		out = append(out, ti)
	}
	return out
}

func (h *Hub) ListSessions() []wire.SessionInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]wire.SessionInfo, 0, len(h.sessions))
	for _, rec := range h.sessions {
		out = append(out, wire.SessionInfo{
			Sid: rec.sid, OpSession: rec.opSession, State: "running",
			IdleMs: sinceMs(rec.lastIOMs), CreatedMs: rec.createdMs,
		})
	}
	return out
}

func (h *Hub) Status() StatusInfo {
	tg := h.ListTargets()
	total := 0
	for _, t := range tg {
		total += t.SessionsActive
	}
	return StatusInfo{Targets: len(tg), TotalSessions: total, TargetsList: tg}
}

type ExecParams struct {
	OpSession string
	Target    string
	Command   string
	TimeoutMs int64
	Shell     string
}

func (h *Hub) Exec(p ExecParams) (*wire.ExecResult, error) {
	ac, err := h.agentOf(p.Target)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(p.TimeoutMs)*time.Millisecond + 5*time.Second
	if p.TimeoutMs == 0 {
		timeout = 5 * time.Minute
	}
	resp, err := ac.call(wire.MsgExecRequest, "", &wire.ExecRequest{
		Command: p.Command, TimeoutMs: p.TimeoutMs, Shell: p.Shell,
	}, timeout)
	if err != nil {
		return nil, err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return nil, errors.New(m.Message)
	}
	res, _ := wire.Decode[wire.ExecResult](resp)
	h.log(p.OpSession, p.Target, "exec", p.Command, fmt.Sprintf("exit=%d", res.ExitCode))
	return res, nil
}

type ShellOpenParams struct {
	OpSession  string
	Target     string
	Shell      string
	Cols, Rows uint16
}

func (h *Hub) ShellOpen(p ShellOpenParams) (*wire.ShellOpenResult, error) {
	ac, err := h.agentOf(p.Target)
	if err != nil {
		return nil, err
	}
	resp, err := ac.call(wire.MsgShellOpen, "", &wire.ShellOpen{Shell: p.Shell, Cols: p.Cols, Rows: p.Rows}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return nil, errors.New(m.Message)
	}
	res, _ := wire.Decode[wire.ShellOpenResult](resp)
	if res.Sid != "" {
		now := time.Now().UnixMilli()
		h.mu.Lock()
		h.sessions[res.Sid] = &sessionRec{sid: res.Sid, targetID: p.Target, opSession: p.OpSession, createdMs: now, lastIOMs: now}
		h.mu.Unlock()
	} else if res.Busy != nil && res.Busy.Busy {
		// Enrich with attribution from the hub's own table.
		h.mu.Lock()
		for _, rec := range h.sessions {
			if rec.targetID == p.Target {
				res.Busy.ActiveSessions = append(res.Busy.ActiveSessions, wire.SessionInfo{
					Sid: rec.sid, OpSession: rec.opSession, State: "running",
					IdleMs: sinceMs(rec.lastIOMs), CreatedMs: rec.createdMs,
				})
			}
		}
		h.mu.Unlock()
	}
	h.log(p.OpSession, p.Target, "shell_open", p.Shell, fmt.Sprintf("sid=%s", res.Sid))
	return res, nil
}

type ShellSendParams struct {
	OpSession string
	Target    string
	Sid       string
	Input     []byte
}

func (h *Hub) ShellSend(p ShellSendParams) error {
	ac, err := h.agentOf(p.Target)
	if err != nil {
		return err
	}
	resp, err := ac.call(wire.MsgShellSend, p.Sid, &wire.ShellSend{Input: p.Input}, 15*time.Second)
	if err != nil {
		return err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return errors.New(m.Message)
	}
	h.touchSession(p.Sid)
	return nil
}

type ShellReadParams struct {
	OpSession string
	Target    string
	Sid       string
	TimeoutMs int64
}

func (h *Hub) ShellRead(p ShellReadParams) (*wire.ShellReadResult, error) {
	ac, err := h.agentOf(p.Target)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(p.TimeoutMs)*time.Millisecond + 5*time.Second
	resp, err := ac.call(wire.MsgShellRead, p.Sid, &wire.ShellRead{TimeoutMs: p.TimeoutMs}, timeout)
	if err != nil {
		return nil, err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return nil, errors.New(m.Message)
	}
	res, _ := wire.Decode[wire.ShellReadResult](resp)
	h.touchSession(p.Sid)
	return res, nil
}

type ShellCloseParams struct {
	OpSession string
	Target    string
	Sid       string
}

func (h *Hub) ShellClose(p ShellCloseParams) (*wire.ShellCloseResult, error) {
	ac, err := h.agentOf(p.Target)
	if err != nil {
		return nil, err
	}
	resp, err := ac.call(wire.MsgShellClose, p.Sid, nil, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return nil, errors.New(m.Message)
	}
	res, _ := wire.Decode[wire.ShellCloseResult](resp)
	h.mu.Lock()
	delete(h.sessions, p.Sid)
	h.mu.Unlock()
	h.log(p.OpSession, p.Target, "shell_close", p.Sid, fmt.Sprintf("exit=%d", res.ExitCode))
	return res, nil
}

type SignalParams struct {
	OpSession string
	Target    string
	Sid       string
	Sig       string
}

func (h *Hub) Signal(p SignalParams) error {
	ac, err := h.agentOf(p.Target)
	if err != nil {
		return err
	}
	resp, err := ac.call(wire.MsgSignal, p.Sid, &wire.Signal{Sig: p.Sig}, 5*time.Second)
	if err != nil {
		return err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return errors.New(m.Message)
	}
	h.log(p.OpSession, p.Target, "signal", p.Sid+":"+p.Sig, "ok")
	return nil
}

type FSReadParams struct {
	OpSession string
	Target    string
	Path      string
}

func (h *Hub) FSRead(p FSReadParams) (*wire.FSReadResult, error) {
	res, err := h.fsCall(p.OpSession, p.Target, wire.MsgFSRead, wire.MsgFSReadResult, &wire.FSRead{Path: p.Path}, "file_read:"+p.Path)
	if err != nil {
		return nil, err
	}
	out, _ := wire.Decode[wire.FSReadResult](res)
	return out, nil
}

type FSWriteParams struct {
	OpSession string
	Target    string
	Path      string
	Data      []byte
	Mode      uint32
}

func (h *Hub) FSWrite(p FSWriteParams) (*wire.FSOpResult, error) {
	res, err := h.fsCall(p.OpSession, p.Target, wire.MsgFSWrite, wire.MsgFSWriteResult, &wire.FSWrite{Path: p.Path, Data: p.Data, Mode: p.Mode}, "file_write:"+p.Path)
	if err != nil {
		return nil, err
	}
	out, _ := wire.Decode[wire.FSOpResult](res)
	return out, nil
}

type FSListParams struct {
	OpSession string
	Target    string
	Path      string
}

func (h *Hub) FSList(p FSListParams) (*wire.FSListResult, error) {
	res, err := h.fsCall(p.OpSession, p.Target, wire.MsgFSList, wire.MsgFSListResult, &wire.FSList{Path: p.Path}, "file_list:"+p.Path)
	if err != nil {
		return nil, err
	}
	out, _ := wire.Decode[wire.FSListResult](res)
	return out, nil
}

type FSStatParams struct {
	OpSession string
	Target    string
	Path      string
}

func (h *Hub) FSStat(p FSStatParams) (*wire.FSStatResult, error) {
	res, err := h.fsCall(p.OpSession, p.Target, wire.MsgFSStat, wire.MsgFSStatResult, &wire.FSStat{Path: p.Path}, "file_stat:"+p.Path)
	if err != nil {
		return nil, err
	}
	out, _ := wire.Decode[wire.FSStatResult](res)
	return out, nil
}

func (h *Hub) fsCall(opSession, target string, reqT, resT wire.MsgType, body any, auditArgs string) (*wire.Envelope, error) {
	ac, err := h.agentOf(target)
	if err != nil {
		return nil, err
	}
	resp, err := ac.call(reqT, "", body, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return nil, errors.New(m.Message)
	}
	h.log(opSession, target, "fs", auditArgs, "ok")
	return resp, nil
}

// Handle dispatches an IPC JSON-RPC method. Returns a JSON-marshalable result.
func (h *Hub) Handle(method string, params json.RawMessage) (any, error) {
	switch method {
	case "list_targets":
		return h.ListTargets(), nil
	case "list_sessions":
		return h.ListSessions(), nil
	case "status":
		return h.Status(), nil
	case "exec":
		var p ExecParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.Exec(p)
	case "shell_open":
		var p ShellOpenParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.ShellOpen(p)
	case "shell_send":
		var p ShellSendParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		err := h.ShellSend(p)
		return ack{OK: err == nil}, err
	case "shell_read":
		var p ShellReadParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.ShellRead(p)
	case "shell_close":
		var p ShellCloseParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.ShellClose(p)
	case "signal":
		var p SignalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		err := h.Signal(p)
		return ack{OK: err == nil}, err
	case "file_read":
		var p FSReadParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.FSRead(p)
	case "file_write":
		var p FSWriteParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.FSWrite(p)
	case "file_list":
		var p FSListParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.FSList(p)
	case "file_stat":
		var p FSStatParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.FSStat(p)
	default:
		return nil, fmt.Errorf("hub: unknown method %q", method)
	}
}

type ack struct{ OK bool }

// --- helpers ---

func (h *Hub) touchSession(sid string) {
	h.mu.Lock()
	if r, ok := h.sessions[sid]; ok {
		r.lastIOMs = time.Now().UnixMilli()
	}
	h.mu.Unlock()
}

func (h *Hub) log(opSession, target, op, args, result string) {
	if h.audit == nil {
		return
	}
	_ = h.audit.Log(audit.Record{Session: opSession, Target: target, Op: op, Args: args, Result: result})
}

func sinceMs(ms int64) int64 {
	if ms == 0 {
		return 0
	}
	now := time.Now().UnixMilli()
	if now <= ms {
		return 0
	}
	return now - ms
}
