package openvpn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"golang.org/x/crypto/hkdf"
)

// On-wire sizes for handshake payloads. Chosen so each control packet
// (after tls-crypt wrapping: +40B, plus reliability layer: ~13B) fits
// inside a 1500-byte UDP MTU.
const (
	HandshakeNonceSize         = 16
	HandshakeFinishTagSize     = 16
	HandshakeSessionKeySize    = 32
	HandshakeClientHelloSize   = 320
	HandshakeServerHelloSize   = 1200
	HandshakeClientFinishSize  = 80
	HandshakeServerFinishSize  = 80
)

var (
	ErrHandshakeWrongSize  = errors.New("openvpn: handshake message wrong size")
	ErrHandshakeBadFinish  = errors.New("openvpn: handshake finish HMAC mismatch")
	ErrHandshakeWrongState = errors.New("openvpn: handshake step called in wrong state")
)

type handshakeState int

const (
	hsInitial handshakeState = iota
	hsClientAwaitServerHello
	hsClientAwaitServerFinish
	hsServerAwaitClientHello
	hsServerAwaitClientFinish
	hsDone
)

// Handshake derives a 32-byte session key from a pre-shared key and fresh
// client/server nonces. tls-crypt (Phase 3) is responsible for proving
// PSK knowledge per-packet; this state machine only handles nonce
// exchange and session-key derivation.
//
// Wire trip count and payload sizes mimic a TLS 1.2 handshake to a
// passive DPI observer (which sees only encrypted payloads through
// tls-crypt, so the *content* is irrelevant — only the per-packet sizes
// and the four-packet rhythm matter).
type Handshake struct {
	psk         []byte
	isServer    bool
	state       handshakeState
	clientNonce [HandshakeNonceSize]byte
	serverNonce [HandshakeNonceSize]byte
	sessionKey  []byte
}

func NewClientHandshake(psk []byte) *Handshake {
	return &Handshake{psk: append([]byte(nil), psk...), state: hsInitial}
}

func NewServerHandshake(psk []byte) *Handshake {
	return &Handshake{psk: append([]byte(nil), psk...), isServer: true, state: hsServerAwaitClientHello}
}

// Initial returns the first handshake payload (ClientHello). Only valid
// for a client, only valid in the initial state.
func (h *Handshake) Initial() ([]byte, error) {
	if h.isServer || h.state != hsInitial {
		return nil, ErrHandshakeWrongState
	}
	if _, err := rand.Read(h.clientNonce[:]); err != nil {
		return nil, err
	}
	out := make([]byte, HandshakeClientHelloSize)
	copy(out, h.clientNonce[:])
	if _, err := rand.Read(out[HandshakeNonceSize:]); err != nil {
		return nil, err
	}
	h.state = hsClientAwaitServerHello
	return out, nil
}

// Receive processes one inbound handshake payload. Returns the response
// to send back (nil if no response is required) and whether the handshake
// has completed after this step. SessionKey is available once done.
func (h *Handshake) Receive(in []byte) (out []byte, done bool, err error) {
	switch h.state {
	case hsServerAwaitClientHello:
		if len(in) != HandshakeClientHelloSize {
			return nil, false, ErrHandshakeWrongSize
		}
		copy(h.clientNonce[:], in[:HandshakeNonceSize])
		if _, err := rand.Read(h.serverNonce[:]); err != nil {
			return nil, false, err
		}
		if err := h.deriveSession(); err != nil {
			return nil, false, err
		}
		resp := make([]byte, HandshakeServerHelloSize)
		copy(resp, h.serverNonce[:])
		if _, err := rand.Read(resp[HandshakeNonceSize:]); err != nil {
			return nil, false, err
		}
		h.state = hsServerAwaitClientFinish
		return resp, false, nil

	case hsClientAwaitServerHello:
		if len(in) != HandshakeServerHelloSize {
			return nil, false, ErrHandshakeWrongSize
		}
		copy(h.serverNonce[:], in[:HandshakeNonceSize])
		if err := h.deriveSession(); err != nil {
			return nil, false, err
		}
		resp := make([]byte, HandshakeClientFinishSize)
		tag := h.finishTag("client-finish-v1")
		copy(resp, tag[:HandshakeFinishTagSize])
		if _, err := rand.Read(resp[HandshakeFinishTagSize:]); err != nil {
			return nil, false, err
		}
		h.state = hsClientAwaitServerFinish
		return resp, false, nil

	case hsServerAwaitClientFinish:
		if len(in) != HandshakeClientFinishSize {
			return nil, false, ErrHandshakeWrongSize
		}
		expected := h.finishTag("client-finish-v1")
		if !hmac.Equal(expected[:HandshakeFinishTagSize], in[:HandshakeFinishTagSize]) {
			return nil, false, ErrHandshakeBadFinish
		}
		resp := make([]byte, HandshakeServerFinishSize)
		tag := h.finishTag("server-finish-v1")
		copy(resp, tag[:HandshakeFinishTagSize])
		if _, err := rand.Read(resp[HandshakeFinishTagSize:]); err != nil {
			return nil, false, err
		}
		h.state = hsDone
		return resp, true, nil

	case hsClientAwaitServerFinish:
		if len(in) != HandshakeServerFinishSize {
			return nil, false, ErrHandshakeWrongSize
		}
		expected := h.finishTag("server-finish-v1")
		if !hmac.Equal(expected[:HandshakeFinishTagSize], in[:HandshakeFinishTagSize]) {
			return nil, false, ErrHandshakeBadFinish
		}
		h.state = hsDone
		return nil, true, nil
	}
	return nil, false, ErrHandshakeWrongState
}

func (h *Handshake) deriveSession() error {
	var salt [2 * HandshakeNonceSize]byte
	copy(salt[:HandshakeNonceSize], h.clientNonce[:])
	copy(salt[HandshakeNonceSize:], h.serverNonce[:])
	h.sessionKey = make([]byte, HandshakeSessionKeySize)
	r := hkdf.New(sha256.New, h.psk, salt[:], []byte("openvpn-shape-session-v1"))
	_, err := io.ReadFull(r, h.sessionKey)
	return err
}

func (h *Handshake) finishTag(label string) []byte {
	m := hmac.New(sha256.New, h.sessionKey)
	m.Write([]byte(label))
	m.Write(h.clientNonce[:])
	m.Write(h.serverNonce[:])
	return m.Sum(nil)
}

func (h *Handshake) Done() bool { return h.state == hsDone }

// SessionKey returns a copy of the derived 32-byte session key, or nil
// if the handshake is not yet complete.
func (h *Handshake) SessionKey() []byte {
	if !h.Done() {
		return nil
	}
	return append([]byte(nil), h.sessionKey...)
}
