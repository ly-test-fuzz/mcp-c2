package crypto

import (
	"bytes"
	"net"
	"testing"

	"debugmcp/internal/wire"
)

// Both handshake halves must run concurrently (net.Pipe is synchronous). The hub
// goroutine owns its half of the round trip and closes its end on exit so the
// agent never blocks on a pipe nobody is reading.
func TestHandshakeRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()

	psk := bytes.Repeat([]byte{0x42}, 32)

	type hubResult struct {
		got *wire.Envelope
		err error
	}
	hubCh := make(chan hubResult, 1)
	go func() {
		defer b.Close()
		hc, err := Handshake(b, Config{PSK: psk, Initiator: false})
		if err != nil {
			hubCh <- hubResult{err: err}
			return
		}
		defer hc.Close()
		// agent -> hub: heartbeat
		got, err := hc.Recv()
		if err != nil {
			hubCh <- hubResult{err: err}
			return
		}
		// hub -> agent: exec result with binary stdout
		out, _ := wire.Encode(wire.MsgExecResult, 8, "sid-9", &wire.ExecResult{
			Stdout:     []byte{0x00, 0x01, 0xff, 0xfe},
			ExitCode:   3,
			Completion: wire.CompletionAuthoritative,
		})
		if err := hc.Send(out); err != nil {
			hubCh <- hubResult{err: err}
			return
		}
		hubCh <- hubResult{got: got}
	}()

	agent, err := Handshake(a, Config{PSK: psk, Initiator: true})
	if err != nil {
		t.Fatalf("agent handshake: %v", err)
	}
	defer agent.Close()

	env, _ := wire.Encode(wire.MsgHeartbeat, 7, "", nil)
	if err := agent.Send(env); err != nil {
		t.Fatalf("agent send: %v", err)
	}
	got2, err := agent.Recv()
	if err != nil {
		t.Fatalf("agent recv: %v", err)
	}

	res := <-hubCh
	if res.err != nil {
		t.Fatalf("hub side: %v", res.err)
	}
	if res.got.Type != wire.MsgHeartbeat || res.got.Seq != 7 {
		t.Fatalf("hub received mismatched envelope: %+v", res.got)
	}
	er, _ := wire.Decode[wire.ExecResult](got2)
	if !bytes.Equal(er.Stdout, []byte{0x00, 0x01, 0xff, 0xfe}) || er.ExitCode != 3 {
		t.Fatalf("exec result mismatch: %+v", er)
	}
}

func TestHandshakeWrongPSKFails(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		_, _ = Handshake(b, Config{PSK: bytes.Repeat([]byte{1}, 32), Initiator: false})
	}()

	_, err := Handshake(a, Config{PSK: bytes.Repeat([]byte{2}, 32), Initiator: true})
	if err == nil {
		t.Fatal("expected handshake to FAIL with mismatched PSK, but it succeeded")
	}
}

func TestHandshakeShortPSKRejected(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	_, err := Handshake(a, Config{PSK: []byte("short"), Initiator: true})
	if err == nil {
		t.Fatal("expected error for short PSK")
	}
}
