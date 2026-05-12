package openvpn

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/hkdf"
)

const (
	tlsCryptHMACSize    = 32
	tlsCryptCipherIVLen = 16
	tlsCryptHeaderLen   = 1 + 8 + 4 + 4 + tlsCryptHMACSize // 49
)

var (
	ErrTLSCryptShort   = errors.New("openvpn: tls-crypt frame too short")
	ErrTLSCryptHMAC    = errors.New("openvpn: tls-crypt HMAC mismatch")
	ErrTLSCryptReplay  = errors.New("openvpn: tls-crypt replay detected")
	ErrTLSCryptIDSpace = errors.New("openvpn: tls-crypt replay id exhausted")
)

// ControlCipher applies the tls-crypt wire silhouette to control packets:
// inserts a 4B replay id + 4B net_time + 32B HMAC after the session id,
// and AES-256-CTR-encrypts the body (everything past the session id).
//
// The wire layout — opcode|sid|replay|time|HMAC|ciphertext — matches what
// DPI sees from OpenVPN tls-crypt. The keys and replay state are ours;
// peers using this codec interoperate with each other, not with the
// reference implementation.
type ControlCipher struct {
	ka       []byte // 32B HMAC-SHA256 key
	ke       []byte // 32B AES-256 key
	sendID   atomic.Uint32
	replay   *ReplayWindow
	timeFn   func() uint32
}

// NewControlCipher derives Ka and Ke from a shared secret via HKDF-SHA256
// and returns a fresh cipher with its own replay window.
func NewControlCipher(secret []byte) (*ControlCipher, error) {
	if len(secret) == 0 {
		return nil, errors.New("openvpn: control cipher secret empty")
	}
	ka := make([]byte, 32)
	r := hkdf.New(sha256.New, secret, nil, []byte("openvpn-shape-control-hmac-v1"))
	if _, err := io.ReadFull(r, ka); err != nil {
		return nil, err
	}
	ke := make([]byte, 32)
	r = hkdf.New(sha256.New, secret, nil, []byte("openvpn-shape-control-cipher-v1"))
	if _, err := io.ReadFull(r, ke); err != nil {
		return nil, err
	}
	return &ControlCipher{
		ka:     ka,
		ke:     ke,
		replay: NewReplayWindow(),
		timeFn: func() uint32 { return uint32(time.Now().Unix()) },
	}, nil
}

// Wrap takes the plaintext bytes of an encoded ControlPacket (output of
// ControlPacket.Encode) and returns the tls-crypt-shaped wire bytes.
func (c *ControlCipher) Wrap(plain []byte) ([]byte, error) {
	if len(plain) < 9 {
		return nil, ErrTLSCryptShort
	}
	id := c.sendID.Add(1)
	if id == 0 {
		return nil, ErrTLSCryptIDSpace
	}

	body := plain[9:]
	out := make([]byte, tlsCryptHeaderLen+len(body))
	copy(out[:9], plain[:9]) // opcode|kid + sid
	binary.BigEndian.PutUint32(out[9:13], id)
	binary.BigEndian.PutUint32(out[13:17], c.timeFn())

	mac := hmac.New(sha256.New, c.ka)
	mac.Write(out[:17])
	mac.Write(body)
	tag := mac.Sum(nil)
	copy(out[17:49], tag)

	block, err := aes.NewCipher(c.ke)
	if err != nil {
		return nil, err
	}
	stream := cipher.NewCTR(block, tag[:tlsCryptCipherIVLen])
	stream.XORKeyStream(out[49:], body)
	return out, nil
}

// Unwrap reverses Wrap. Returns the reconstructed encoded ControlPacket
// (suitable for DecodeControlPacket). Rejects tampered packets and
// replays.
func (c *ControlCipher) Unwrap(wire []byte) ([]byte, error) {
	if len(wire) < tlsCryptHeaderLen {
		return nil, ErrTLSCryptShort
	}
	id := binary.BigEndian.Uint32(wire[9:13])
	tag := wire[17:49]
	ct := wire[49:]

	block, err := aes.NewCipher(c.ke)
	if err != nil {
		return nil, err
	}
	body := make([]byte, len(ct))
	stream := cipher.NewCTR(block, tag[:tlsCryptCipherIVLen])
	stream.XORKeyStream(body, ct)

	mac := hmac.New(sha256.New, c.ka)
	mac.Write(wire[:17])
	mac.Write(body)
	if !hmac.Equal(tag, mac.Sum(nil)) {
		return nil, ErrTLSCryptHMAC
	}
	if !c.replay.Accept(id) {
		return nil, ErrTLSCryptReplay
	}

	plain := make([]byte, 9+len(body))
	copy(plain[:9], wire[:9])
	copy(plain[9:], body)
	return plain, nil
}
