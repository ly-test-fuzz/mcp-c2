package proto

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

type FrameType string

const (
	FrameHello        FrameType = "HELLO"
	FrameAuth         FrameType = "AUTH"
	FrameSessionOpen  FrameType = "SESSION_OPEN"
	FrameSessionClose FrameType = "SESSION_CLOSE"
	FrameCmdInput     FrameType = "CMD_INPUT"
	FrameInterrupt    FrameType = "INTERRUPT"
	FrameOutputChunk  FrameType = "OUTPUT_CHUNK"
	FrameAlive        FrameType = "ALIVE"
	FrameFileUpload   FrameType = "FILE_UPLOAD"
	FrameFileDownload FrameType = "FILE_DOWNLOAD"
	FrameFileAck       FrameType = "FILE_ACK"
	FrameDirDownload   FrameType = "DIR_DOWNLOAD"
	FrameHeartbeat     FrameType = "HEARTBEAT"
	FrameAck          FrameType = "ACK"
	FrameError        FrameType = "ERROR"
)

type Frame struct {
	Type       FrameType       `json:"type"`
	ID         string          `json:"id"`
	ClientID   string          `json:"client_id,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	TransferID string          `json:"transfer_id,omitempty"`
	Timestamp  time.Time       `json:"timestamp"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

func NewFrame(t FrameType, payload any) (Frame, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Type: t, ID: NewID(), Timestamp: time.Now().UTC(), Payload: b}, nil
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

type HelloPayload struct {
	ClientID string            `json:"client_id,omitempty"`
	Hostname string            `json:"hostname"`
	OS       string            `json:"os"`
	Arch     string            `json:"arch"`
	Caps     map[string]bool   `json:"caps,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
}

type AuthPayload struct {
	OK          bool   `json:"ok"`
	ClientID    string `json:"client_id,omitempty"`
	Message     string `json:"message,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type HeartbeatPayload struct {
	UptimeSeconds int64 `json:"uptime_seconds"`
}

type AckPayload struct {
	ForFrameID string `json:"for_frame_id,omitempty"`
	Message    string `json:"message,omitempty"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type SessionOpenPayload struct {
	SessionID string `json:"session_id,omitempty"`
	Shell     string `json:"shell"`
}

type SessionOpenResult struct {
	SessionID   string `json:"session_id"`
	Interactive bool   `json:"interactive"`
	Message     string `json:"message,omitempty"`
}

type SessionClosePayload struct {
	SessionID string `json:"session_id"`
}

type CommandInputPayload struct {
	SessionID     string `json:"session_id"`
	Text          string `json:"text"`
	AppendNewline bool   `json:"append_newline"`
}

type OutputChunkPayload struct {
	SessionID string `json:"session_id"`
	Data      []byte `json:"data"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Alive     bool   `json:"alive"`
}

type AlivePayload struct {
	SessionID string `json:"session_id"`
	Alive     bool   `json:"alive"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

// DirDownloadPayload is sent from hub to client to request a recursive directory tar.
type DirDownloadPayload struct {
	TransferID string   `json:"transfer_id"`
	Paths      []string `json:"paths"`
}

type FileTransferPayload struct {
	TransferID  string `json:"transfer_id"`
	Direction   string `json:"direction"`
	ChunkIndex  int64  `json:"chunk_index"`
	Offset      int64  `json:"offset"`
	Length      int64  `json:"length"`
	ChunkSHA256 string `json:"chunk_sha256,omitempty"`
	FileSHA256  string `json:"file_sha256,omitempty"`
	ACK         bool   `json:"ack,omitempty"`
	NACK        bool   `json:"nack,omitempty"`
	ResumeFrom  int64  `json:"resume_from,omitempty"`
	Finalize    bool   `json:"finalize,omitempty"`
	TempPath    string `json:"temp_path,omitempty"`
	Data        []byte `json:"data,omitempty"`
}

type ClientSummary struct {
	ClientID        string            `json:"client_id"`
	Hostname        string            `json:"hostname"`
	OS              string            `json:"os"`
	Arch            string            `json:"arch"`
	UptimeSeconds   int64             `json:"uptime_seconds"`
	LastSeenUnix    int64             `json:"last_seen_unix"`
	CertFingerprint string            `json:"cert_fingerprint,omitempty"`
	Caps            map[string]bool   `json:"caps,omitempty"`
	Meta            map[string]string `json:"meta,omitempty"`
}
