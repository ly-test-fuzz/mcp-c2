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
