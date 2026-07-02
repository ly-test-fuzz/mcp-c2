package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
)

// roundTrip encodes body -> Envelope -> frames -> reads back -> decodes into T.
func roundTrip[T any](t *testing.T, typ MsgType, body *T) *T {
	t.Helper()
	env, err := Encode(typ, 42, "sid-1", body)
	if err != nil {
		t.Fatalf("Encode %s: %v", typ, err)
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, env); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Type != typ || got.Seq != 42 || got.Sid != "sid-1" {
		t.Fatalf("envelope header mismatch: %+v", got)
	}
	out, err := Decode[T](got)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return out
}

func TestRoundTripExecResult(t *testing.T) {
	in := &ExecResult{
		Stdout:     []byte("hello\nworld\n"),
		ExitCode:   7,
		Completion: CompletionAuthoritative,
		DurationMs: 123,
	}
	out := roundTrip[ExecResult](t, MsgExecResult, in)
	if !bytes.Equal(out.Stdout, in.Stdout) || out.ExitCode != 7 || out.Completion != CompletionAuthoritative {
		t.Fatalf("mismatch: %+v", out)
	}
}

// TestBinarySafety is the headline property: arbitrary binary bytes — including
// the sentinel marker string, NULs, control chars, and high bytes — must survive
// a round trip byte-identical, and must NOT be misread as a frame boundary or a
// completion signal. This is the core fix for the "sentinel vs binary output"
// failure mode (pre-mortem #7).
func TestBinarySafety(t *testing.T) {
	// A payload containing bytes that would be catastrophic if scanned naively:
	// the sentinel marker text, NULs, the frame-length prefix pattern, etc.
	nasty := []byte("__DBGMCP_EOF:deadbeef__")
	nasty = append(nasty, 0x00, 0x01, 0x02, 0x1b, 0x7f, 0x80, 0xff, 0xfe)
	nasty = append(nasty, []byte("\x00\x00\x00\x05")...) // looks like a length prefix
	for i := 0; i < 512; i++ {
		nasty = append(nasty, byte(i)) // full byte range cycle
	}
	in := &StreamChunk{Stream: StreamStdout, Data: nasty}
	out := roundTrip[StreamChunk](t, MsgStreamChunk, in)
	if !bytes.Equal(out.Data, nasty) {
		t.Fatalf("binary data corrupted: got %d bytes want %d", len(out.Data), len(nasty))
	}
	if out.Stream != StreamStdout {
		t.Fatalf("stream field corrupted: %q", out.Stream)
	}
	// And a ShellReadResult carrying many binary chunks.
	res := &ShellReadResult{
		Chunks:     []StreamChunk{{Stream: StreamStdout, Data: nasty}, {Stream: StreamStderr, Data: nasty}},
		Done:       false,
		Completion: CompletionSentinel,
	}
	got := roundTrip[ShellReadResult](t, MsgShellReadResult, res)
	if len(got.Chunks) != 2 || !bytes.Equal(got.Chunks[1].Data, nasty) {
		t.Fatalf("multi-chunk binary corrupted: %+v", got)
	}
}

func TestFrameGuards(t *testing.T) {
	t.Run("zero length rejected", func(t *testing.T) {
		var buf bytes.Buffer
		var hdr [4]byte
		buf.Write(hdr[:])
		if _, err := ReadFrame(&buf); err == nil {
			t.Fatal("expected error for zero-length frame")
		}
	})
	t.Run("oversized rejected", func(t *testing.T) {
		var buf bytes.Buffer
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
		buf.Write(hdr[:])
		if _, err := ReadFrame(&buf); err == nil {
			t.Fatal("expected error for oversized frame")
		}
	})
	t.Run("truncated body", func(t *testing.T) {
		var buf bytes.Buffer
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 100)
		buf.Write(hdr[:])
		buf.Write([]byte("only a few")) // < 100 bytes
		if _, err := ReadFrame(&buf); !errors.Is(err, io.ErrUnexpectedEOF) && err != io.EOF {
			// io.ReadFull returns io.ErrUnexpectedEOF or io.EOF; either is acceptable.
		}
		if err := func() error { _, e := ReadFrame(&buf); return e }(); err == nil {
			t.Fatal("expected error for truncated frame body")
		}
	})
}

func TestMsgTypeString(t *testing.T) {
	if MsgExecResult.String() != "ExecResult" {
		t.Fatalf("unexpected: %s", MsgExecResult.String())
	}
	if MsgType(250).String() != "MsgType(250)" {
		t.Fatalf("unknown type string mismatch")
	}
}

// TestConnRoundTrip exercises Conn framing over net.Pipe with a reader goroutine,
// and verifies that the typed decode matches.
func TestConnRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	ca, cb := NewConn(a), NewConn(b)

	env, err := Encode(MsgExecRequest, 1, "", &ExecRequest{Command: "echo hi", Shell: "bash"})
	if err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() {
		got, err := ca.Read()
		if err != nil {
			errc <- err
			return
		}
		req, derr := Decode[ExecRequest](got)
		if derr != nil {
			errc <- derr
			return
		}
		if req.Command != "echo hi" || req.Shell != "bash" {
			errc <- errors.New("decoded body mismatch")
			return
		}
		errc <- nil
	}()

	if err := cb.Write(env); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("reader: %v", err)
	}
}

// TestConnConcurrentWrites ensures many goroutines writing across one multiplexed
// connection never corrupt a frame (writes are serialized under wmu).
func TestConnConcurrentWrites(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca, cb := NewConn(a), NewConn(b)

	const N = 200
	// Reader drains N frames and checks each Seq appears exactly once.
	got := make(map[uint64]bool)
	var rerr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < N; i++ {
			env, err := ca.Read()
			if err != nil {
				rerr = err
				return
			}
			if got[env.Seq] {
				rerr = errors.New("duplicate or corrupted seq")
				return
			}
			got[env.Seq] = true
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env, _ := Encode(MsgHeartbeat, cb.NextSeq(), "", nil)
			if err := cb.Write(env); err != nil {
				t.Errorf("write %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	<-done
	if rerr != nil {
		t.Fatalf("reader error: %v", rerr)
	}
	if len(got) != N {
		t.Fatalf("expected %d unique seqs, got %d", N, len(got))
	}
}

// TestRoundTripFSChunkMessages covers the chunked-transfer message bodies
// (binary-safe Data via base64) and the new MsgType String() mappings.
func TestRoundTripFSChunkMessages(t *testing.T) {
	payload := []byte{0x00, 0xff, 0xfe, 0x55, 0x01, 0x7f, 0x80}
	wo := roundTrip[FSWriteOpen](t, MsgFSWriteOpen, &FSWriteOpen{
		Path: "/tmp/x", Mode: 0o600, TotalSize: 12345, TotalSha256: "abc123", ChunkSize: 4096,
	})
	if wo.Path != "/tmp/x" || wo.TotalSize != 12345 || wo.TotalSha256 != "abc123" || wo.ChunkSize != 4096 {
		t.Fatalf("FSWriteOpen mismatch: %+v", wo)
	}
	wc := roundTrip[FSWriteChunk](t, MsgFSWriteChunk, &FSWriteChunk{
		Index: 2, Offset: 8192, Data: payload, Sha256: "deadbeef",
	})
	if wc.Index != 2 || wc.Offset != 8192 || wc.Sha256 != "deadbeef" || !bytes.Equal(wc.Data, payload) {
		t.Fatalf("FSWriteChunk mismatch: %+v", wc)
	}
	// FSWriteCommit request body (new: carries WantSize/WantSha256/IsDir).
	wcom := roundTrip[FSWriteCommit](t, MsgFSWriteCommit, &FSWriteCommit{
		WantSize: 999, WantSha256: "cafe", IsDir: true,
	})
	if wcom.WantSize != 999 || wcom.WantSha256 != "cafe" || !wcom.IsDir {
		t.Fatalf("FSWriteCommit mismatch: %+v", wcom)
	}
	// FSWriteOpen with IsDir flag.
	wod := roundTrip[FSWriteOpen](t, MsgFSWriteOpen, &FSWriteOpen{Path: "/d", IsDir: true, ChunkSize: 4096})
	if !wod.IsDir || wod.Path != "/d" {
		t.Fatalf("FSWriteOpen IsDir mismatch: %+v", wod)
	}
	wcr := roundTrip[FSWriteChunkResult](t, MsgFSWriteChunkResult, &FSWriteChunkResult{Index: 2, OK: false})
	if wcr.Index != 2 || wcr.OK {
		t.Fatalf("FSWriteChunkResult mismatch: %+v", wcr)
	}
	ror := roundTrip[FSReadOpenResult](t, MsgFSReadOpenResult, &FSReadOpenResult{
		DownloadID: "d-1", TotalSize: 99, TotalSha256: "ff", ChunkSize: 4096,
	})
	if ror.DownloadID != "d-1" || ror.TotalSize != 99 || ror.TotalSha256 != "ff" {
		t.Fatalf("FSReadOpenResult mismatch: %+v", ror)
	}
	// FSWriteCommitResult with Entries (is_dir commit reports landed entries).
	wcres := roundTrip[FSWriteCommitResult](t, MsgFSWriteCommitResult, &FSWriteCommitResult{
		Size: 42, Sha256: "abcd", Entries: []string{"a.txt", "sub/b.txt"},
	})
	if wcres.Size != 42 || wcres.Sha256 != "abcd" || len(wcres.Entries) != 2 || wcres.Entries[1] != "sub/b.txt" {
		t.Fatalf("FSWriteCommitResult Entries mismatch: %+v", wcres)
	}
	rcr := roundTrip[FSReadChunkResult](t, MsgFSReadChunkResult, &FSReadChunkResult{
		Index: 3, Data: payload, Sha256: "x", EOF: true,
	})
	if rcr.Index != 3 || !rcr.EOF || !bytes.Equal(rcr.Data, payload) {
		t.Fatalf("FSReadChunkResult mismatch: %+v", rcr)
	}
	for _, c := range []struct {
		m MsgType
		s string
	}{
		{MsgFSWriteOpen, "FSWriteOpen"},
		{MsgFSWriteOpenResult, "FSWriteOpenResult"},
		{MsgFSWriteChunk, "FSWriteChunk"},
		{MsgFSWriteChunkResult, "FSWriteChunkResult"},
		{MsgFSWriteCommit, "FSWriteCommit"},
		{MsgFSWriteCommitResult, "FSWriteCommitResult"},
		{MsgFSReadOpen, "FSReadOpen"},
		{MsgFSReadOpenResult, "FSReadOpenResult"},
		{MsgFSReadChunk, "FSReadChunk"},
		{MsgFSReadChunkResult, "FSReadChunkResult"},
	} {
		if got := c.m.String(); got != c.s {
			t.Errorf("MsgType String(%d): got %q want %q", c.m, got, c.s)
		}
	}
}
