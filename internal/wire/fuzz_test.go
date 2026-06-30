package wire

import (
	"bytes"
	"os"
	"testing"
)

// FuzzReadFrame enforces the core robustness property (pre-mortem #7, spike #6):
// the frame reader must never panic on arbitrary bytes — including a real binary
// file like /bin/ls (the `cat /bin/ls` scenario) and crafted adversarial prefixes.
func FuzzReadFrame(f *testing.F) {
	// Seed 1: a valid frame (empty-body Heartbeat).
	valid, _ := Encode(MsgHeartbeat, 1, "", nil)
	var vb bytes.Buffer
	_ = WriteFrame(&vb, valid)
	f.Add(vb.Bytes())
	// Seed 2-4: edge cases.
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 5, 1, 2, 3})
	// Seed 5: a real binary file (the "cat /bin/ls" corpus).
	if bin, err := os.ReadFile("/bin/ls"); err == nil {
		f.Add(bin)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		env, err := ReadFrame(r)
		if err != nil {
			return // expected for the vast majority of random input
		}
		// If a frame decoded, the typed body decode must also be panic-free.
		_, _ = Decode[ExecResult](env)
		_, _ = Decode[ShellReadResult](env)
	})
}

// FuzzDecodeBody ensures json body decoding never panics on arbitrary bytes.
func FuzzDecodeBody(f *testing.F) {
	f.Add([]byte(`{"stdout":"aGk=","exit_code":0,"completion":"authoritative"}`))
	f.Add([]byte(`{`))
	f.Add([]byte("\x00\x00\xff"))
	if bin, err := os.ReadFile("/bin/ls"); err == nil {
		f.Add(bin)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		env := &Envelope{Ver: Version, Type: MsgExecResult, Body: data}
		_, _ = Decode[ExecResult](env)
	})
}
