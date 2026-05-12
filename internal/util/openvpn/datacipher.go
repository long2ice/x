package openvpn

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"

	"golang.org/x/crypto/hkdf"
)

const (
	DataAEADTagSize    = 16
	DataPacketIDSize   = 4
	DataImplicitIVSize = 8
	DataKeySize        = 32
)

var (
	ErrDataShort   = errors.New("openvpn: data frame too short")
	ErrDataAEAD    = errors.New("openvpn: data AEAD authentication failed")
	ErrDataReplay  = errors.New("openvpn: data replay detected")
	ErrDataIDSpace = errors.New("openvpn: data packet id exhausted")
)

// DataCipher applies AES-256-GCM to one direction of the data channel.
// Each packet's nonce is `[4B packet_id BE | 8B implicit IV]`; the
// packet_id is also bound as AAD so wire-level rewrites are detected.
//
// Wire layout produced by Seal / consumed by Open:
//
//	[4B packet_id] [ciphertext] [16B AEAD tag]
//
// This is the inner payload of a P_DATA_V2 packet; the outer 1B
// opcode|key_id + 3B peer_id are handled by DataPacket.
type DataCipher struct {
	aead       cipher.AEAD
	implicitIV [DataImplicitIVSize]byte
	sendID     atomic.Uint32
	replay     *ReplayWindow
}

// NewDataCipherPair derives (send, recv) cipher instances for one end of
// the link from the 32-byte session key produced by Handshake. The
// `isServer` flag swaps the directional info-strings so that a client's
// send-key matches a server's recv-key and vice versa.
func NewDataCipherPair(sessionKey []byte, isServer bool) (send, recv *DataCipher, err error) {
	c2s, err := newDataCipher(sessionKey, "openvpn-shape-data-c2s-v1")
	if err != nil {
		return nil, nil, err
	}
	s2c, err := newDataCipher(sessionKey, "openvpn-shape-data-s2c-v1")
	if err != nil {
		return nil, nil, err
	}
	if isServer {
		return s2c, c2s, nil
	}
	return c2s, s2c, nil
}

func newDataCipher(sessionKey []byte, info string) (*DataCipher, error) {
	var key [DataKeySize]byte
	if _, err := io.ReadFull(hkdf.New(sha256.New, sessionKey, nil, []byte(info+"-key")), key[:]); err != nil {
		return nil, err
	}
	var iv [DataImplicitIVSize]byte
	if _, err := io.ReadFull(hkdf.New(sha256.New, sessionKey, nil, []byte(info+"-iv")), iv[:]); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &DataCipher{aead: aead, implicitIV: iv, replay: NewReplayWindow()}, nil
}

func (c *DataCipher) nonce(packetID uint32) []byte {
	n := make([]byte, c.aead.NonceSize())
	binary.BigEndian.PutUint32(n[:DataPacketIDSize], packetID)
	copy(n[DataPacketIDSize:], c.implicitIV[:])
	return n
}

// Seal encrypts plaintext under a fresh packet id. The packet id is the
// first 4 bytes of the returned slice and is bound as AAD.
func (c *DataCipher) Seal(plaintext []byte) ([]byte, error) {
	id := c.sendID.Add(1)
	if id == 0 {
		return nil, ErrDataIDSpace
	}
	out := make([]byte, DataPacketIDSize, DataPacketIDSize+len(plaintext)+DataAEADTagSize)
	binary.BigEndian.PutUint32(out[:DataPacketIDSize], id)
	nonce := c.nonce(id)
	out = c.aead.Seal(out, nonce, plaintext, out[:DataPacketIDSize])
	return out, nil
}

// Open decrypts a wire frame, rejects replays, returns the plaintext.
func (c *DataCipher) Open(wire []byte) ([]byte, error) {
	if len(wire) < DataPacketIDSize+DataAEADTagSize {
		return nil, ErrDataShort
	}
	id := binary.BigEndian.Uint32(wire[:DataPacketIDSize])
	nonce := c.nonce(id)
	pt, err := c.aead.Open(nil, nonce, wire[DataPacketIDSize:], wire[:DataPacketIDSize])
	if err != nil {
		return nil, ErrDataAEAD
	}
	if !c.replay.Accept(id) {
		return nil, ErrDataReplay
	}
	return pt, nil
}
