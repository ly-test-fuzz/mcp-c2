// Package hub is the long-lived operator-side daemon: it accepts authenticated C2
// agents over the transport plane (Noise PSK), holds the agent registry and the
// session/occupancy table, routes shim requests to the right agent as Seq-correlated
// wire RPCs, and audits every operation. The hub never speaks MCP — the shim does.
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"debugmcp/internal/audit"
	"debugmcp/internal/crypto"
	"debugmcp/internal/fsutil"
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

// --- file transfer: upload/download ---
//
// upload/download 把文件/目录在 operator 本地磁盘(hub 进程的 fs)与 target(agent 的 fs)
// 之间搬运。字节流不再流经 shim/LLM —— shim 只传两个路径 + is_dir 标志, hub 做本地磁盘
// I/O 并驱动现有分块管道流向 agent。单文件走 raw 字节流; 目录(is_dir=true) 在传输源端
// 用 fsutil.TarDir 流式打包成 tar, 在接收端用 fsutil.UntarStream 流式解包。整条链路
// 二进制、零临时 tar 文件、内存 O(一个 chunk)。

type UploadParams struct {
	OpSession  string `json:"op_session"`
	Target     string `json:"target"`
	LocalPath  string `json:"local_path"`  // operator 本地路径(hub 进程可见)
	RemotePath string `json:"remote_path"` // target 上的目标路径
	IsDir      bool   `json:"is_dir"`      // 类似 scp -r: true=目录走 tar 流
}

type DownloadParams struct {
	OpSession  string `json:"op_session"`
	Target     string `json:"target"`
	RemotePath string `json:"remote_path"` // target 上的源路径
	LocalPath  string `json:"local_path"`  // operator 本地目标路径
	IsDir      bool   `json:"is_dir"`
}

// TransferResult 是 upload/download 的返回值。size/sha256 是传输字节流(单文件=文件,
// 目录=tar 流)的统计。NEntries 仅 is_dir 时有意义。
type TransferResult struct {
	Size       int64    `json:"size"`
	Sha256     string   `json:"sha256"`
	NEntries   int      `json:"n_entries,omitempty"`
	DurationMs int64    `json:"duration_ms"`
	Entries    []string `json:"entries,omitempty"` // is_dir 模式下已落地相对路径(commit 时由 agent 回报)
	Err        string   `json:"err,omitempty"`
}

// Upload 把 operator 本地的 local_path 传到 target 的 remote_path。
// 模式由 is_dir 决定: false=单文件, true=目录(tar 流)。
func (h *Hub) Upload(p UploadParams) (*TransferResult, error) {
	start := time.Now()
	res := &TransferResult{}

	var (
		src       io.Reader
		wantSha   string
		wantSize  int64
		mode      uint32
		nEntries  int
		tarRes    *fsutil.TarResult
		cleanup   func()
	)
	if p.IsDir {
		r, tr, err := fsutil.TarDir(p.LocalPath)
		if err != nil {
			res.Err = err.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			h.logTransfer(p.OpSession, p.Target, "upload", p.LocalPath, p.RemotePath, res)
			return res, err
		}
		src = r
		tarRes = tr
		// 目录模式: wantSha/wantSize 在流读尽后才确定, commit 时由 agent 独立累计并比对。
		mode = 0o755
		cleanup = nil
	} else {
		f, err := os.Open(p.LocalPath)
		if err != nil {
			res.Err = err.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			h.logTransfer(p.OpSession, p.Target, "upload", p.LocalPath, p.RemotePath, res)
			return res, err
		}
		fi, _ := f.Stat()
		// 预算单文件 sha(流式) + size。
		h2 := sha256.New()
		if _, err := io.Copy(h2, f); err != nil {
			f.Close()
			res.Err = err.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			h.logTransfer(p.OpSession, p.Target, "upload", p.LocalPath, p.RemotePath, res)
			return res, err
		}
		wantSha = hex.EncodeToString(h2.Sum(nil))
		wantSize = fi.Size()
		mode = uint32(fi.Mode().Perm())
		if mode == 0 {
			mode = 0o644
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			res.Err = err.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			h.logTransfer(p.OpSession, p.Target, "upload", p.LocalPath, p.RemotePath, res)
			return res, err
		}
		src = f
		cleanup = func() { f.Close() }
	}
	if cleanup != nil {
		defer cleanup()
	}

	commit, err := h.streamToAgent(p.Target, p.RemotePath, mode, p.IsDir, src, wantSha, wantSize)
	if err != nil {
		res.Err = err.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		h.logTransfer(p.OpSession, p.Target, "upload", p.LocalPath, p.RemotePath, res)
		return res, err
	}
	if tarRes != nil {
		_, sha, n, terr := tarRes.Result()
		if terr != nil {
			res.Err = terr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			h.logTransfer(p.OpSession, p.Target, "upload", p.LocalPath, p.RemotePath, res)
			return res, err
		}
		_ = sha
		nEntries = n
	}
	res.Size = commit.Size
	res.Sha256 = commit.Sha256
	res.NEntries = nEntries
	res.Entries = commit.Entries
	res.DurationMs = time.Since(start).Milliseconds()
	h.logTransfer(p.OpSession, p.Target, "upload", p.LocalPath, p.RemotePath, res)
	return res, nil
}

// Download 把 target 上的 remote_path 拉到 operator 本地的 local_path。
func (h *Hub) Download(p DownloadParams) (*TransferResult, error) {
	start := time.Now()
	res := &TransferResult{}

	var (
		dst      io.Writer
		cleanup  func()
		untarRes *fsutil.UntarResult
		untarDir string
	)
	if p.IsDir {
		// 目录下载: hub 侧接收 tar 流并 untar 到 local_path。
		// UntarStream 接收一个 io.Reader, 我们用一个 pipe 把 streamFromAgent 的 chunk
		// 流喂给它。
		pr, pw := io.Pipe()
		untarDone := make(chan struct{})
		var ures *fsutil.UntarResult
		go func() {
			defer close(untarDone)
			ures = fsutil.UntarStream(p.LocalPath, pr)
		}()
		dst = pw
		untarDir = p.LocalPath
		cleanup = func() {
			_ = pw.Close()
			<-untarDone
			untarRes = ures
		}
		_ = untarDir
	} else {
		f, err := os.Create(p.LocalPath)
		if err != nil {
			res.Err = err.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			h.logTransfer(p.OpSession, p.Target, "download", p.RemotePath, p.LocalPath, res)
			return res, err
		}
		dst = f
		cleanup = func() { f.Close() }
	}

	size, sha, nEntries, err := h.streamFromAgent(p.Target, p.RemotePath, p.IsDir, dst)
	cleanup()
	if err != nil {
		res.Err = err.Error()
		res.Size = size
		res.DurationMs = time.Since(start).Milliseconds()
		h.logTransfer(p.OpSession, p.Target, "download", p.RemotePath, p.LocalPath, res)
		return res, err
	}
	if untarRes != nil {
		// 用 untar 的统计覆盖: size/sha 是 tar 字节流, entries 是已落地路径。
		size = untarRes.Size
		sha = untarRes.Sha256
		nEntries = len(untarRes.Entries)
		res.Entries = untarRes.Entries
		if untarRes.Err != nil {
			res.Err = untarRes.Err.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			h.logTransfer(p.OpSession, p.Target, "download", p.RemotePath, p.LocalPath, res)
			return res, untarRes.Err
		}
	}
	res.Size = size
	res.Sha256 = sha
	res.NEntries = nEntries
	res.DurationMs = time.Since(start).Milliseconds()
	h.logTransfer(p.OpSession, p.Target, "download", p.RemotePath, p.LocalPath, res)
	return res, nil
}

// streamToAgent 把 src 的字节流分块推到 agent(target)的 remote_path。
// wantSha/wantSize 用于 commit 时的端到端校验(单文件可预算; 目录在 Upload 里事后从 tarRes 取)。
// isDir=true 时 agent 走 untar, commit 回报 Entries。
func (h *Hub) streamToAgent(target, remotePath string, mode uint32, isDir bool, src io.Reader, wantSha string, wantSize int64) (*wire.FSWriteCommitResult, error) {
	openResp, err := h.fsChunkCall(target, wire.MsgFSWriteOpen, wire.MsgFSWriteOpenResult, "",
		&wire.FSWriteOpen{Path: remotePath, Mode: mode, IsDir: isDir, ChunkSize: defaultChunkSize, TotalSha256: wantSha, TotalSize: wantSize})
	if err != nil {
		return nil, err
	}
	open, _ := wire.Decode[wire.FSWriteOpenResult](openResp)
	if open.Err != "" {
		return nil, errors.New(open.Err)
	}
	buf := make([]byte, defaultChunkSize)
	off := int64(0)
	for index := 0; ; index++ {
		n, rerr := io.ReadFull(src, buf)
		if n == 0 && (rerr == io.EOF || rerr == io.ErrUnexpectedEOF) {
			break
		}
		if n == 0 && rerr != nil {
			return nil, rerr
		}
		chunk := buf[:n]
		ok := false
		for retry := 0; retry < 3; retry++ {
			cresp, cerr := h.fsChunkCall(target, wire.MsgFSWriteChunk, wire.MsgFSWriteChunkResult, open.UploadID,
				&wire.FSWriteChunk{Index: int64(index), Offset: off, Data: chunk, Sha256: sha256Hex(chunk)})
			if cerr != nil {
				return nil, cerr
			}
			cres, _ := wire.Decode[wire.FSWriteChunkResult](cresp)
			if cres.Err != "" {
				return nil, errors.New(cres.Err)
			}
			if cres.OK {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("chunk %d hash-mismatched after 3 retries", index)
		}
		off += int64(n)
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
	}
	cresp, err := h.fsChunkCall(target, wire.MsgFSWriteCommit, wire.MsgFSWriteCommitResult, open.UploadID,
		&wire.FSWriteCommit{WantSize: wantSize, WantSha256: wantSha, IsDir: isDir})
	if err != nil {
		return nil, err
	}
	commit, _ := wire.Decode[wire.FSWriteCommitResult](cresp)
	if commit.Err != "" {
		return nil, errors.New(commit.Err)
	}
	return commit, nil
}

// streamFromAgent 从 agent(target)的 remote_path 分块拉取字节流写入 dst。
// 返回 (流字节数, 流 sha256, 目录模式 entry 数, err)。
func (h *Hub) streamFromAgent(target, remotePath string, isDir bool, dst io.Writer) (int64, string, int, error) {
	openResp, err := h.fsChunkCall(target, wire.MsgFSReadOpen, wire.MsgFSReadOpenResult, "",
		&wire.FSReadOpen{Path: remotePath, ChunkSize: defaultChunkSize, IsDir: isDir})
	if err != nil {
		return 0, "", 0, err
	}
	open, _ := wire.Decode[wire.FSReadOpenResult](openResp)
	if open.Err != "" {
		return 0, "", 0, errors.New(open.Err)
	}
	hsh := sha256.New()
	mw := io.MultiWriter(dst, hsh)
	var total int64
	for index := 0; ; index++ {
		var res *wire.FSReadChunkResult
		for retry := 0; ; retry++ {
			cresp, cerr := h.fsChunkCall(target, wire.MsgFSReadChunk, wire.MsgFSReadChunkResult, open.DownloadID,
				&wire.FSReadChunk{Index: int64(index), Offset: total})
			if cerr != nil {
				return 0, "", 0, cerr
			}
			res, _ = wire.Decode[wire.FSReadChunkResult](cresp)
			if res.Err != "" {
				return 0, "", 0, errors.New(res.Err)
			}
			if len(res.Data) == 0 || sha256Hex(res.Data) == res.Sha256 {
				break
			}
			if retry >= 2 {
				return 0, "", 0, fmt.Errorf("read chunk %d hash mismatch after 3 tries", index)
			}
		}
		if _, werr := mw.Write(res.Data); werr != nil {
			return 0, "", 0, werr
		}
		total += int64(len(res.Data))
		if res.EOF {
			break
		}
	}
	sha := hex.EncodeToString(hsh.Sum(nil))
	if open.TotalSha256 != "" && sha != open.TotalSha256 && !isDir {
		return total, sha, open.NEntries, fmt.Errorf("integrity check failed: full-file sha256 mismatch")
	}
	return total, sha, open.NEntries, nil
}

// logTransfer 记录一次 upload/download 的审计。
func (h *Hub) logTransfer(opSession, target, op, src, dst string, res *TransferResult) {
	if h.audit == nil {
		return
	}
	args := src + " -> " + dst
	result := "ok"
	if res.Err != "" {
		result = "error: " + res.Err
	}
	_ = h.audit.Log(audit.Record{
		Session: opSession, Target: target, Op: op, Args: args, Result: result, Bytes: res.Size,
	})
}

// --- chunked file transfer (shared by upload/download) ---
// defaultChunkSize 与 sha256Hex 在 streamToAgent/streamFromAgent 中使用。
const defaultChunkSize int64 = 4 << 20 // 4 MiB

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fsChunkCall is the per-chunk RPC passthrough for chunked transfer. It carries a
// session id (Sid = upload/download id) and enforces that the agent replies with
// the expected resT.
func (h *Hub) fsChunkCall(target string, reqT, resT wire.MsgType, sid string, body any) (*wire.Envelope, error) {
	ac, err := h.agentOf(target)
	if err != nil {
		return nil, err
	}
	resp, err := ac.call(reqT, sid, body, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.Type == wire.MsgError {
		m, _ := wire.Decode[wire.ErrorMsg](resp)
		return nil, errors.New(m.Message)
	}
	if resp.Type != resT {
		return nil, fmt.Errorf("hub: %s: unexpected response type %s", reqT, resp.Type)
	}
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
	case "upload":
		var p UploadParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.Upload(p)
	case "download":
		var p DownloadParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return h.Download(p)
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
