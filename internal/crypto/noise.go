// Package crypto secures the hub<->agent transport plane with Noise_NNpsk0:
// a shared pre-shared key (enrolled out-of-band) provides mutual authentication,
// ephemeral Diffie-Hellman gives forward secrecy, and ChaChaPoly1305 gives AEAD.
//
// The PSK is mutually authenticated via an explicit post-handshake AEAD confirm:
// Noise_NN's handshake messages are plaintext ephemeral keys, so the PSK only
// starts protecting the transport after Split(). A wrong PSK derives a different
// transport key, so the confirm fails to decrypt on at least one side — neither
// side can exchange data without the enrolled secret.
package crypto

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/flynn/noise"

	"debugmcp/internal/wire"
)

const (
	maxRecordSize = 64 * 1024 * 1024
	confirmI      = "DBGMCP-OK-I" // initiator -> responder
	confirmR      = "DBGMCP-OK-R" // responder -> initiator
)

// Config for a Noise_NNpsk0 transport.
type Config struct {
	PSK       []byte // enrolled pre-shared key (length >= 32 recommended)
	Initiator bool   // agent = initiator, hub = responder
}

// Handshake performs Noise_NNpsk0 over c and returns an encrypted Conn.
func Handshake(c net.Conn, cfg Config) (*Conn, error) {
	if len(cfg.PSK) < 32 {
		return nil, fmt.Errorf("crypto: PSK must be >= 32 bytes (got %d)", len(cfg.PSK))
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2b),
		Random:                rand.Reader,
		Pattern:               noise.HandshakeNN,
		Initiator:             cfg.Initiator,
		PresharedKey:          cfg.PSK,
		PresharedKeyPlacement: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("crypto: handshake state: %w", err)
	}

	// NN: initiator -> ephemeral; responder -> ephemeral. The handshake completes
	// on the second message, at which point WriteMessage/ReadMessage return the
	// transport cipher states. Per Noise (and flynn/noise's Test_NNpsk0_Roundtrip),
	// cs1 is the initiator->responder direction and cs2 is responder->initiator, so
	// the two roles assign send/recv in OPPOSITE order.
	var cs1, cs2 *noise.CipherState
	if cfg.Initiator {
		msg, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, err
		}
		if err := writeLenMsg(c, msg); err != nil {
			return nil, err
		}
		resp, err := readLenMsg(c)
		if err != nil {
			return nil, err
		}
		_, cs1, cs2, err = hs.ReadMessage(nil, resp)
		if err != nil {
			return nil, err
		}
	} else {
		resp, err := readLenMsg(c)
		if err != nil {
			return nil, err
		}
		if _, _, _, err := hs.ReadMessage(nil, resp); err != nil {
			return nil, err
		}
		msg, c1, c2, werr := hs.WriteMessage(nil, nil)
		if werr != nil {
			return nil, werr
		}
		if err := writeLenMsg(c, msg); err != nil {
			return nil, err
		}
		cs1, cs2 = c1, c2 // assign into outer cs1/cs2
	}
	if cs1 == nil || cs2 == nil {
		return nil, fmt.Errorf("crypto: handshake did not yield transport keys")
	}
	var send, recv *noise.CipherState
	if cfg.Initiator {
		send, recv = cs1, cs2 // send = I->R, recv = R->I
	} else {
		send, recv = cs2, cs1 // responder: send = R->I, recv = I->R
	}
	conn := &Conn{c: c, send: send, recv: recv}

	// Mutual PSK confirm (this is where auth is actually enforced).
	if cfg.Initiator {
		if err := conn.sendRaw([]byte(confirmI)); err != nil {
			return nil, err
		}
		if err := conn.recvRaw(confirmR); err != nil {
			return nil, fmt.Errorf("crypto: responder auth failed: %w", err)
		}
	} else {
		if err := conn.recvRaw(confirmI); err != nil {
			return nil, fmt.Errorf("crypto: initiator auth failed: %w", err)
		}
		if err := conn.sendRaw([]byte(confirmR)); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

// Conn is an AEAD-secured, message-oriented connection exchanging wire envelopes.
type Conn struct {
	c    net.Conn
	send *noise.CipherState
	recv *noise.CipherState
	wmu  sync.Mutex // serializes Sends (multiple sessions multiplex over one conn)
}

// Send encrypts and writes one wire envelope as a single AEAD record. Safe for
// concurrent callers.
func (c *Conn) Send(env *wire.Envelope) error {
	rec, err := wire.MarshalFrame(env)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ct, err := c.send.Encrypt(nil, nil, rec)
	if err != nil {
		return err
	}
	return writeLenMsg(c.c, ct)
}

// Recv reads and decrypts one wire envelope.
func (c *Conn) Recv() (*wire.Envelope, error) {
	ct, err := readLenMsg(c.c)
	if err != nil {
		return nil, err
	}
	pt, err := c.recv.Decrypt(nil, nil, ct)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return wire.UnmarshalFrame(pt)
}

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.c.Close() }

// RemoteAddr returns the peer's network address.
func (c *Conn) RemoteAddr() net.Addr { return c.c.RemoteAddr() }

func (c *Conn) sendRaw(plain []byte) error {
	ct, err := c.send.Encrypt(nil, nil, plain)
	if err != nil {
		return err
	}
	return writeLenMsg(c.c, ct)
}

func (c *Conn) recvRaw(want string) error {
	ct, err := readLenMsg(c.c)
	if err != nil {
		return err
	}
	pt, err := c.recv.Decrypt(nil, nil, ct)
	if err != nil {
		return err
	}
	if string(pt) != want {
		return fmt.Errorf("crypto: confirm mismatch")
	}
	return nil
}

func writeLenMsg(w io.Writer, msg []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(msg)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

func readLenMsg(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxRecordSize {
		return nil, fmt.Errorf("crypto: bad record length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
