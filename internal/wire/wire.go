// Package wire defines the versioned, length-prefixed, binary-safe protocol
// spoken between the hub and a C2 agent (the transport plane). It is the single
// shared contract between the Go hub and the Go agent; the shim speaks MCP on
// the other side and is bridged to this protocol by the hub.
//
// Framing: every message on the wire is [4-byte big-endian length][JSON Envelope].
// The Envelope.Body is a json.RawMessage carrying one of the typed message structs
// below. []byte fields (shell/stdout data, file contents) are JSON-base64 encoded
// automatically, so arbitrary binary output (e.g. `cat /bin/ls`) cannot corrupt a
// frame and a sentinel/marker byte-sequence can never cause a false positive.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Version is the wire-protocol version carried in every Envelope.
const Version uint8 = 1

// MaxFrame bounds a single framed message (DoS / memory guard).
const MaxFrame = 64 * 1024 * 1024 // 64 MiB

// MsgType identifies the Body payload type.
type MsgType uint8

const (
	MsgHello MsgType = iota + 1
	MsgWelcome
	MsgHeartbeat
	MsgHeartbeatAck
	MsgExecRequest
	MsgExecResult
	MsgShellOpen
	MsgShellOpenResult
	MsgShellSend
	MsgShellRead
	MsgShellReadResult
	MsgShellClose
	MsgShellCloseResult
	MsgSignal
	MsgStreamChunk // agent -> hub: async stdout/stderr data for a session
	MsgExit        // agent -> hub: authoritative process exit for a one-shot exec
	MsgFSRead
	MsgFSReadResult
	MsgFSWrite
	MsgFSWriteResult
	MsgFSList
	MsgFSListResult
	MsgFSStat
	MsgFSStatResult
	MsgGoodbye
	MsgError
)

func (m MsgType) String() string {
	switch m {
	case MsgHello:
		return "Hello"
	case MsgWelcome:
		return "Welcome"
	case MsgHeartbeat:
		return "Heartbeat"
	case MsgHeartbeatAck:
		return "HeartbeatAck"
	case MsgExecRequest:
		return "ExecRequest"
	case MsgExecResult:
		return "ExecResult"
	case MsgShellOpen:
		return "ShellOpen"
	case MsgShellOpenResult:
		return "ShellOpenResult"
	case MsgShellSend:
		return "ShellSend"
	case MsgShellRead:
		return "ShellRead"
	case MsgShellReadResult:
		return "ShellReadResult"
	case MsgShellClose:
		return "ShellClose"
	case MsgShellCloseResult:
		return "ShellCloseResult"
	case MsgSignal:
		return "Signal"
	case MsgStreamChunk:
		return "StreamChunk"
	case MsgExit:
		return "Exit"
	case MsgFSRead:
		return "FSRead"
	case MsgFSReadResult:
		return "FSReadResult"
	case MsgFSWrite:
		return "FSWrite"
	case MsgFSWriteResult:
		return "FSWriteResult"
	case MsgFSList:
		return "FSList"
	case MsgFSListResult:
		return "FSListResult"
	case MsgFSStat:
		return "FSStat"
	case MsgFSStatResult:
		return "FSStatResult"
	case MsgGoodbye:
		return "Goodbye"
	case MsgError:
		return "Error"
	default:
		return fmt.Sprintf("MsgType(%d)", uint8(m))
	}
}

// Completion labels describe how trustworthy a "done" signal is, surfaced to the
// AI so it can decide whether to verify before acting (Principle #3).
const (
	CompletionAuthoritative = "authoritative" // exit event observed (waitpid)
	CompletionSentinel      = "sentinel"      // end-of-output marker seen
	CompletionIdleTimeout   = "idle_timeout"  // stream quiet for idle window
	CompletionHardTimeout   = "hard_timeout"  // absolute timeout reached
	CompletionHeuristic     = "heuristic"     // echo unavailable; idle-only guess
)

// Signal logical names (mapped to OS primitives by the agent).
const (
	SigInterrupt = "interrupt"  // Ctrl-C / SIGINT / CTRL_C_EVENT
	SigTerminate = "terminate"  // SIGTERM / CTRL_BREAK_EVENT
	SigForceKill = "force_kill" // SIGKILL / TerminateProcess
	SigQuit      = "quit"       // SIGQUIT (POSIX-only; maps to terminate on Windows)
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// Envelope is the top-level framed message exchanged over one hub<->agent conn.
type Envelope struct {
	Ver  uint8           `json:"v"`           // protocol version
	Type MsgType         `json:"t"`           // payload type
	Seq  uint64          `json:"s,omitempty"` // request correlation id (req/resp pairing)
	Sid  string          `json:"i,omitempty"` // session id (session-scoped ops)
	Body json.RawMessage `json:"b,omitempty"` // marshaled typed message body
}

// Capabilities advertises what an agent supports.
type Capabilities struct {
	ConcurrencyCap int    `json:"concurrency_cap"`
	MaxFileSize    int64  `json:"max_file_size"`
	AgentVersion   string `json:"agent_version"`
}

// Hello is sent by the agent right after the Noise handshake.
type Hello struct {
	AgentID  string       `json:"agent_id,omitempty"` // claimed id; hub assigns if empty
	Hostname string       `json:"hostname"`
	Platform string       `json:"platform"` // linux/darwin/windows
	Arch     string       `json:"arch"`
	Shell    string       `json:"shell"` // detected login shell
	Caps     Capabilities `json:"caps"`
}

// Welcome is the hub's reply to Hello.
type Welcome struct {
	AgentID  string `json:"agent_id"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

type LoadInfo struct {
	SessionsActive int     `json:"sessions_active"`
	CPU            float64 `json:"cpu,omitempty"`
}

// HeartbeatAck carries live load/occupancy back to the hub (Principle #4).
type HeartbeatAck struct {
	SessionsActive int      `json:"sessions_active"`
	Load           LoadInfo `json:"load"`
}

// SessionInfo is one active session's visibility record.
type SessionInfo struct {
	Sid       string `json:"sid"`
	OpSession string `json:"op_session"` // attribution, not isolation (single-operator)
	State     string `json:"state"`      // running|idle|blocked
	IdleMs    int64  `json:"idle_ms"`
	CreatedMs int64  `json:"created_ms"` // unix ms
}

// BusyInfo is returned when a slot cannot be allocated (occupancy surfaced).
type BusyInfo struct {
	Busy           bool          `json:"busy"`
	Used           int           `json:"used"`
	Cap            int           `json:"cap"`
	RetryAfterMs   int64         `json:"retry_after_ms,omitempty"` // lower-bound hint (min remaining timeout)
	ActiveSessions []SessionInfo `json:"active_sessions,omitempty"`
}

type ExecRequest struct {
	Command   string `json:"command"`
	TimeoutMs int64  `json:"timeout_ms,omitempty"`
	Shell     string `json:"shell,omitempty"` // override login shell
}

type ExecResult struct {
	Stdout     []byte `json:"stdout,omitempty"`
	Stderr     []byte `json:"stderr,omitempty"`
	ExitCode   int32  `json:"exit_code"`
	Signal     string `json:"signal,omitempty"`
	Completion string `json:"completion"`
	DurationMs int64  `json:"duration_ms"`
}

type ShellOpen struct {
	Shell string `json:"shell,omitempty"` // override login shell
	Rows  uint16 `json:"rows,omitempty"`
	Cols  uint16 `json:"cols,omitempty"`
}

type ShellOpenResult struct {
	Sid   string    `json:"sid,omitempty"`
	Shell string    `json:"shell,omitempty"`
	Busy  *BusyInfo `json:"busy,omitempty"`
}

type ShellSend struct {
	Input []byte `json:"input"`
}

type ShellRead struct {
	TimeoutMs int64 `json:"timeout_ms"`
}

// StreamChunk is an asynchronous data frame (binary-safe; []byte -> base64).
type StreamChunk struct {
	Stream string `json:"stream"` // stdout|stderr
	Data   []byte `json:"data"`
}

type ShellReadResult struct {
	Chunks     []StreamChunk `json:"chunks,omitempty"`
	Done       bool          `json:"done"`
	Completion string        `json:"completion,omitempty"`
}

type ShellCloseResult struct {
	ExitCode int32  `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
}

type Signal struct {
	Sig string `json:"sig"`
}

// Exit is the authoritative exit frame for a one-shot exec (no sentinel needed).
type Exit struct {
	Sid      string `json:"sid,omitempty"`
	ExitCode int32  `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
}

type FSRead struct {
	Path string `json:"path"`
}

type FSReadResult struct {
	Data []byte `json:"data,omitempty"`
	Err  string `json:"err,omitempty"`
}

type FSWrite struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
	Mode uint32 `json:"mode,omitempty"` // os.FileMode; 0 => 0644
}

type FSOpResult struct {
	Err string `json:"err,omitempty"`
}

type DirEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	IsDir bool   `json:"is_dir"`
}

type FSList struct {
	Path string `json:"path"`
}

type FSListResult struct {
	Entries []DirEntry `json:"entries,omitempty"`
	Err     string     `json:"err,omitempty"`
}

type FSStat struct {
	Path string `json:"path"`
}

type FileInfo struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Mode      uint32 `json:"mode"`
	IsDir     bool   `json:"is_dir"`
	ModTimeMs int64  `json:"mod_time_ms"`
}

type FSStatResult struct {
	Stat *FileInfo `json:"stat,omitempty"`
	Err  string    `json:"err,omitempty"`
}

type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Encode marshals a typed body into an Envelope ready to Write.
func Encode(t MsgType, seq uint64, sid string, body any) (*Envelope, error) {
	env := &Envelope{Ver: Version, Type: t, Seq: seq, Sid: sid}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("wire: marshal %s: %w", t, err)
		}
		env.Body = b
	}
	return env, nil
}

// Decode unmarshals an Envelope.Body into a value of type T.
func Decode[T any](env *Envelope) (*T, error) {
	var v T
	if len(env.Body) == 0 {
		return &v, nil
	}
	if err := json.Unmarshal(env.Body, &v); err != nil {
		return nil, fmt.Errorf("wire: unmarshal %s: %w", env.Type, err)
	}
	return &v, nil
}

// MarshalFrame returns the complete on-wire record for env: [4-byte BE length][JSON].
// The whole record is what an AEAD transport encrypts as a single unit, so one
// wire envelope maps to exactly one encrypted record.
func MarshalFrame(env *Envelope) ([]byte, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("wire: encode envelope: %w", err)
	}
	if len(body) > MaxFrame {
		return nil, fmt.Errorf("wire: frame too large (%d > %d)", len(body), MaxFrame)
	}
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(len(body)))
	copy(out[4:], body)
	return out, nil
}

// UnmarshalFrame parses a complete record (length prefix + JSON body) produced by
// MarshalFrame. data must be exactly one record.
func UnmarshalFrame(data []byte) (*Envelope, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("wire: record too short (%d)", len(data))
	}
	n := binary.BigEndian.Uint32(data[:4])
	if int(n) != len(data)-4 {
		return nil, fmt.Errorf("wire: record length mismatch (want %d, got %d)", n, len(data)-4)
	}
	if n > MaxFrame {
		return nil, fmt.Errorf("wire: frame too large (%d > %d)", n, MaxFrame)
	}
	var env Envelope
	if err := json.Unmarshal(data[4:], &env); err != nil {
		return nil, fmt.Errorf("wire: decode envelope: %w", err)
	}
	return &env, nil
}

// WriteFrame writes one length-prefixed Envelope to w.
func WriteFrame(w io.Writer, env *Envelope) error {
	rec, err := MarshalFrame(env)
	if err != nil {
		return err
	}
	if _, err := w.Write(rec); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one length-prefixed Envelope from r.
func ReadFrame(r io.Reader) (*Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, errors.New("wire: zero-length frame")
	}
	if n > MaxFrame {
		return nil, fmt.Errorf("wire: frame too large (%d > %d)", n, MaxFrame)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var env Envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return nil, fmt.Errorf("wire: decode envelope: %w", err)
	}
	return &env, nil
}

// ErrClosed is returned by a Conn after Close.
var ErrClosed = errors.New("wire: connection closed")

// Conn wraps a duplex byte stream and serializes concurrent writes (multiple
// sessions multiplex over one connection; reads must be single-threaded).
type Conn struct {
	rw   io.ReadWriteCloser
	wmu  sync.Mutex
	seq  uint64
	done chan struct{}
	once sync.Once
}

// NewConn wraps an existing read-write-closer.
func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{rw: rw, done: make(chan struct{})}
}

// Write writes one envelope; safe for concurrent callers.
func (c *Conn) Write(env *Envelope) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.isClosed() {
		return ErrClosed
	}
	return WriteFrame(c.rw, env)
}

// Read reads one envelope; caller must serialize (one reader goroutine).
func (c *Conn) Read() (*Envelope, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	return ReadFrame(c.rw)
}

// NextSeq returns a monotonically increasing correlation id.
func (c *Conn) NextSeq() uint64 {
	return atomic.AddUint64(&c.seq, 1)
}

// Close closes the underlying stream once.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		err = c.rw.Close()
	})
	return err
}

func (c *Conn) isClosed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}
