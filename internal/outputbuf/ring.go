package outputbuf

import "sync"

type SinceStatus string

type TruncatedBy string

const (
	SinceOK             SinceStatus = "ok"
	SinceExpired        SinceStatus = "expired"
	SinceFuture         SinceStatus = "future"
	SinceInvalidSession SinceStatus = "invalid_session"

	TruncatedRingBuffer TruncatedBy = "ring_buffer"
	TruncatedMaxBytes   TruncatedBy = "max_bytes"
)

type ReadResult struct {
	Output         []byte       `json:"output"`
	RequestedSince int64        `json:"requested_since"`
	EarliestCursor int64        `json:"earliest_cursor"`
	NewCursor      int64        `json:"new_cursor"`
	MissedBytes    int64        `json:"missed_bytes"`
	SinceStatus    SinceStatus  `json:"since_status"`
	TruncatedBy    *TruncatedBy `json:"truncated_by,omitempty"`
	Alive          bool         `json:"alive"`
	ExitCode       *int         `json:"exit_code,omitempty"`
	RedactedCount  int          `json:"redacted_count,omitempty"`
}

type Ring struct {
	mu       sync.RWMutex
	buf      []byte
	capBytes int
	earliest int64
	latest   int64
}

func New(capBytes int) *Ring {
	if capBytes <= 0 {
		capBytes = 1024 * 1024
	}
	return &Ring{capBytes: capBytes}
}

func (r *Ring) Write(p []byte) (from, to int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	from = r.latest
	r.latest += int64(len(p))
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.capBytes {
		drop := len(r.buf) - r.capBytes
		r.buf = append([]byte(nil), r.buf[drop:]...)
		r.earliest += int64(drop)
	}
	return from, r.latest
}

func (r *Ring) Cursors() (earliest, latest int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.earliest, r.latest
}

func (r *Ring) Read(since int64, maxBytes int) ReadResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := ReadResult{RequestedSince: since, EarliestCursor: r.earliest, NewCursor: r.latest, SinceStatus: SinceOK}
	start := since
	if since < r.earliest {
		res.SinceStatus = SinceExpired
		res.MissedBytes = r.earliest - since
		start = r.earliest
		t := TruncatedRingBuffer
		res.TruncatedBy = &t
	}
	if since > r.latest {
		res.SinceStatus = SinceFuture
		res.NewCursor = since
		return res
	}
	off := int(start - r.earliest)
	if off < 0 {
		off = 0
	}
	if off > len(r.buf) {
		off = len(r.buf)
	}
	data := append([]byte(nil), r.buf[off:]...)
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[:maxBytes]
		res.NewCursor = start + int64(maxBytes)
		t := TruncatedMaxBytes
		res.TruncatedBy = &t
	} else {
		res.NewCursor = r.latest
	}
	res.Output = data
	return res
}
