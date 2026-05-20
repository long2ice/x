package openvpn

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// The data channel carries encrypted IP packets once the handshake is
// done. With an AEAD cipher the wire layout of a P_DATA_V2 packet is:
//
//	opcode|key_id   1B  ┐ AAD
//	peer_id         3B  ┤
//	packet_id       4B  ┘ also the explicit half of the AEAD nonce
//	auth_tag        16B
//	ciphertext      var
//
// The 12-byte nonce is packet_id (4B) || implicit_iv (8B), the implicit
// IV coming from the derived HMAC key slot.
const (
	aeadTagSize  = 16
	aeadNonceLen = 12

	// PeerIDUnset is the 24-bit all-ones sentinel: the data channel uses
	// P_DATA_V1 (no peer-id field) until the server assigns a real id.
	PeerIDUnset uint32 = 0xffffff
)

// KeepalivePing is OpenVPN's fixed keepalive payload (src/openvpn/ping.h).
// It travels as an ordinary data-channel packet; the receiver recognises
// and drops it rather than handing it to the tun layer.
var KeepalivePing = []byte{
	0x2a, 0x18, 0x7b, 0xf3, 0x64, 0x1e, 0xb4, 0xcb,
	0x07, 0xed, 0x2d, 0x0a, 0x98, 0x1f, 0xc7, 0x48,
}

// IsKeepalive reports whether a decrypted data packet is a keepalive ping.
func IsKeepalive(pkt []byte) bool { return bytes.Equal(pkt, KeepalivePing) }

// cipherKeyLen returns the data-channel key length for a cipher name.
func cipherKeyLen(name string) int {
	if strings.EqualFold(name, "AES-128-GCM") {
		return 16
	}
	return 32 // AES-256-GCM, CHACHA20-POLY1305
}

// replayWindow is a 64-slot sliding bitmap rejecting duplicate or stale
// packet ids while still accepting in-window reordering (real networks
// reorder; a strict high-water mark would drop legitimate packets).
type replayWindow struct {
	high uint32
	bits uint64
	seen bool
}

func (w *replayWindow) accept(id uint32) bool {
	if id == 0 {
		return false
	}
	if !w.seen {
		w.seen, w.high, w.bits = true, id, 1
		return true
	}
	if id > w.high {
		if shift := id - w.high; shift >= 64 {
			w.bits = 1
		} else {
			w.bits = (w.bits << shift) | 1
		}
		w.high = id
		return true
	}
	diff := w.high - id
	if diff >= 64 {
		return false
	}
	mask := uint64(1) << diff
	if w.bits&mask != 0 {
		return false
	}
	w.bits |= mask
	return true
}

// DataChannel encrypts and decrypts data-channel packets for one end.
type DataChannel struct {
	send cipher.AEAD
	recv cipher.AEAD

	sendImplicitIV [aeadNonceLen]byte
	recvImplicitIV [aeadNonceLen]byte

	keyID  uint8
	peerID uint32

	mu           sync.Mutex
	sendPacketID uint32
	replay       replayWindow

	lastSend atomic.Int64 // unix nano of the last Encrypt, for keepalive
}

func newAEAD(cipherName string, key []byte) (cipher.AEAD, error) {
	if strings.EqualFold(cipherName, "CHACHA20-POLY1305") {
		return chacha20poly1305.New(key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithTagSize(block, aeadTagSize)
}

// NewDataChannel builds a data channel from derived key material. peerID
// is the local side's peer id placed in outbound P_DATA_V2 headers
// (PeerIDUnset to send P_DATA_V1 instead). cipherName selects the AEAD.
func NewDataChannel(keys *KeyMaterial, peerID uint32, cipherName string) (*DataChannel, error) {
	if keys == nil {
		return nil, errors.New("openvpn: nil key material")
	}
	klen := cipherKeyLen(cipherName)
	if len(keys.SendCipherKey) < klen || len(keys.RecvCipherKey) < klen {
		return nil, errors.New("openvpn: cipher key material too short")
	}
	send, err := newAEAD(cipherName, keys.SendCipherKey[:klen])
	if err != nil {
		return nil, fmt.Errorf("openvpn: send cipher: %w", err)
	}
	recv, err := newAEAD(cipherName, keys.RecvCipherKey[:klen])
	if err != nil {
		return nil, fmt.Errorf("openvpn: recv cipher: %w", err)
	}
	if len(keys.SendHMACKey) < aeadNonceLen-4 || len(keys.RecvHMACKey) < aeadNonceLen-4 {
		return nil, errors.New("openvpn: implicit IV key material too short")
	}
	d := &DataChannel{send: send, recv: recv, peerID: peerID}
	copy(d.sendImplicitIV[4:], keys.SendHMACKey[:aeadNonceLen-4])
	copy(d.recvImplicitIV[4:], keys.RecvHMACKey[:aeadNonceLen-4])
	d.lastSend.Store(time.Now().UnixNano())
	return d, nil
}

// Encrypt seals one plaintext IP packet into a wire data packet.
func (d *DataChannel) Encrypt(packet []byte) ([]byte, error) {
	d.mu.Lock()
	d.sendPacketID++
	packetID := d.sendPacketID
	d.mu.Unlock()
	if packetID == 0 {
		return nil, errors.New("openvpn: data packet id space exhausted")
	}
	d.lastSend.Store(time.Now().UnixNano())

	header := d.header()
	var pid [4]byte
	binary.BigEndian.PutUint32(pid[:], packetID)
	nonce := d.nonce(packetID, d.sendImplicitIV)

	ad := append(append(make([]byte, 0, len(header)+4), header...), pid[:]...)
	sealed := d.send.Seal(nil, nonce[:], packet, ad)

	// Go's AEAD appends the tag; OpenVPN puts it before the ciphertext.
	out := make([]byte, 0, len(header)+4+len(sealed))
	out = append(out, header...)
	out = append(out, pid[:]...)
	out = append(out, sealed[len(sealed)-aeadTagSize:]...)
	out = append(out, sealed[:len(sealed)-aeadTagSize]...)
	return out, nil
}

// Decrypt opens one wire data packet, returning the plaintext IP packet.
func (d *DataChannel) Decrypt(packet []byte) ([]byte, error) {
	if len(packet) < 1 {
		return nil, errors.New("openvpn: empty data packet")
	}
	opcode, _ := parseOpcodeKeyID(packet[0])
	headerSize := 1
	if opcode == PDataV2 {
		headerSize = 4
	} else if opcode != PDataV1 {
		return nil, fmt.Errorf("openvpn: %s is not a data opcode", opcode)
	}
	if len(packet) < headerSize+4+aeadTagSize {
		return nil, errors.New("openvpn: data packet too short")
	}
	header := packet[:headerSize]
	pid := packet[headerSize : headerSize+4]
	packetID := binary.BigEndian.Uint32(pid)
	tag := packet[headerSize+4 : headerSize+4+aeadTagSize]
	ciphertext := packet[headerSize+4+aeadTagSize:]

	combined := append(append(make([]byte, 0, len(ciphertext)+aeadTagSize), ciphertext...), tag...)
	ad := append(append(make([]byte, 0, len(header)+4), header...), pid...)
	nonce := d.nonce(packetID, d.recvImplicitIV)
	plain, err := d.recv.Open(nil, nonce[:], combined, ad)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	ok := d.replay.accept(packetID)
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("openvpn: replayed data packet id %d", packetID)
	}
	return plain, nil
}

// IdleFor reports how long since the last outbound data packet.
func (d *DataChannel) IdleFor() time.Duration {
	return time.Since(time.Unix(0, d.lastSend.Load()))
}

func (d *DataChannel) header() []byte {
	if d.peerID != PeerIDUnset {
		return []byte{
			opcodeKeyID(PDataV2, d.keyID),
			byte(d.peerID >> 16), byte(d.peerID >> 8), byte(d.peerID),
		}
	}
	return []byte{opcodeKeyID(PDataV1, d.keyID)}
}

func (d *DataChannel) nonce(packetID uint32, implicit [aeadNonceLen]byte) [aeadNonceLen]byte {
	nonce := implicit
	binary.BigEndian.PutUint32(nonce[:4], binary.BigEndian.Uint32(nonce[:4])^packetID)
	return nonce
}
