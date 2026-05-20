package openvpn

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
)

// After the control-channel TLS handshake completes, both ends exchange
// a "key method 2" record as TLS application data. Each record carries
// random seed material; the data-channel keys are then derived locally
// with the OpenVPN PRF (the TLS 1.0 PRF: MD5 ⊕ SHA1).
//
// This is the *legacy* key path. OpenVPN 2.6+ can instead export keys
// from the TLS session (tls-ekm); we deliberately advertise a low
// IV_PROTO so the peer falls back to this path.
const (
	keyMethod2 = 2

	keySourcePreMasterSize = 48
	keySourceRandomSize    = 32

	// OpenVPN reserves 64-byte slots for cipher and HMAC keys regardless
	// of the cipher actually negotiated; the key block is 4 such slots.
	maxCipherKeyLength = 64
	maxHMACKeyLength   = 64
	keyBlockSize       = 2 * (maxCipherKeyLength + maxHMACKeyLength)
)

var errKeyMethodTruncated = errors.New("openvpn: key method 2 record truncated")

// KeySource is one side's random seed contribution. Only the client
// fills PreMaster.
type KeySource struct {
	PreMaster [keySourcePreMasterSize]byte
	Random1   [keySourceRandomSize]byte
	Random2   [keySourceRandomSize]byte
}

// KeySource2 pairs the client and server seeds once both are known.
type KeySource2 struct {
	Client KeySource
	Server KeySource
}

// KeyMaterial holds the derived data-channel keys for one end. Send/Recv
// are relative to the local side.
type KeyMaterial struct {
	SendCipherKey []byte
	SendHMACKey   []byte
	RecvCipherKey []byte
	RecvHMACKey   []byte
}

// KeyMethod2Record is a decoded key-method-2 control message.
type KeyMethod2Record struct {
	Sources  KeySource2
	Options  string
	Username string
	Password string
	PeerInfo string
}

// NewClientKeyMethod2Record builds a client record with fresh random
// seed material.
func NewClientKeyMethod2Record(options, peerInfo, username, password string) (*KeyMethod2Record, error) {
	var r KeyMethod2Record
	for _, b := range [][]byte{r.Sources.Client.PreMaster[:], r.Sources.Client.Random1[:], r.Sources.Client.Random2[:]} {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
	}
	r.Options, r.PeerInfo, r.Username, r.Password = options, peerInfo, username, password
	return &r, nil
}

// NewServerKeyMethod2Record builds a server record (no pre-master) with
// fresh random seed material.
func NewServerKeyMethod2Record(options string) (*KeyMethod2Record, error) {
	var r KeyMethod2Record
	for _, b := range [][]byte{r.Sources.Server.Random1[:], r.Sources.Server.Random2[:]} {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
	}
	r.Options = options
	return &r, nil
}

// MarshalClient serializes the client key-method-2 record.
func (r *KeyMethod2Record) MarshalClient() []byte {
	out := make([]byte, 0, 256)
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, keyMethod2)
	out = append(out, r.Sources.Client.PreMaster[:]...)
	out = append(out, r.Sources.Client.Random1[:]...)
	out = append(out, r.Sources.Client.Random2[:]...)
	out = appendOpenVPNString(out, r.Options)
	out = appendOpenVPNString(out, r.Username)
	out = appendOpenVPNString(out, r.Password)
	out = appendOpenVPNString(out, r.PeerInfo)
	return out
}

// MarshalServer serializes the server key-method-2 record.
func (r *KeyMethod2Record) MarshalServer() []byte {
	out := make([]byte, 0, 128)
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, keyMethod2)
	out = append(out, r.Sources.Server.Random1[:]...)
	out = append(out, r.Sources.Server.Random2[:]...)
	out = appendOpenVPNString(out, r.Options)
	return out
}

// ParseServerKeyMethod2Record decodes a record received from a server.
func ParseServerKeyMethod2Record(packet []byte) (*KeyMethod2Record, error) {
	offset, err := keyMethod2Prefix(packet)
	if err != nil {
		return nil, err
	}
	if len(packet) < offset+keySourceRandomSize*2 {
		return nil, errKeyMethodTruncated
	}
	r := &KeyMethod2Record{}
	offset += copy(r.Sources.Server.Random1[:], packet[offset:])
	offset += copy(r.Sources.Server.Random2[:], packet[offset:])
	if r.Options, offset, err = readOpenVPNString(packet, offset); err != nil {
		return nil, fmt.Errorf("openvpn: read server options: %w", err)
	}
	r.Username, offset, _ = readOpenVPNString(packet, offset)
	r.Password, offset, _ = readOpenVPNString(packet, offset)
	r.PeerInfo, _, _ = readOpenVPNString(packet, offset)
	return r, nil
}

// ParseClientKeyMethod2Record decodes a record received from a client.
func ParseClientKeyMethod2Record(packet []byte) (*KeyMethod2Record, error) {
	offset, err := keyMethod2Prefix(packet)
	if err != nil {
		return nil, err
	}
	if len(packet) < offset+keySourcePreMasterSize+keySourceRandomSize*2 {
		return nil, errKeyMethodTruncated
	}
	r := &KeyMethod2Record{}
	offset += copy(r.Sources.Client.PreMaster[:], packet[offset:])
	offset += copy(r.Sources.Client.Random1[:], packet[offset:])
	offset += copy(r.Sources.Client.Random2[:], packet[offset:])
	if r.Options, offset, err = readOpenVPNString(packet, offset); err != nil {
		return nil, fmt.Errorf("openvpn: read client options: %w", err)
	}
	r.Username, offset, _ = readOpenVPNString(packet, offset)
	r.Password, offset, _ = readOpenVPNString(packet, offset)
	r.PeerInfo, _, _ = readOpenVPNString(packet, offset)
	return r, nil
}

func keyMethod2Prefix(packet []byte) (int, error) {
	if len(packet) < 5 {
		return 0, errKeyMethodTruncated
	}
	if binary.BigEndian.Uint32(packet[:4]) != 0 {
		return 0, errors.New("openvpn: key method 2 record missing zero prefix")
	}
	if packet[4]&0x0f != keyMethod2 {
		return 0, fmt.Errorf("openvpn: unsupported key method %d", packet[4]&0x0f)
	}
	return 5, nil
}

// dataCipherKeyMax is how many cipher-key bytes DeriveKeyMaterial keeps;
// enough for AES-256-GCM and ChaCha20-Poly1305 (AES-128 uses a prefix).
const dataCipherKeyMax = 32

// DeriveKeyMaterial runs the OpenVPN PRF over the combined seed material
// and splits the result into directional data-channel keys. isServer
// selects the local Send/Recv perspective. The data cipher is chosen
// later, so the full 32-byte cipher key is kept.
func DeriveKeyMaterial(sources KeySource2, clientSession, serverSession SessionID, isServer bool) (*KeyMaterial, error) {
	var master [keySourcePreMasterSize]byte
	if err := openvpnPRF(sources.Client.PreMaster[:], "OpenVPN master secret",
		sources.Client.Random1[:], sources.Server.Random1[:], nil, nil, master[:]); err != nil {
		return nil, err
	}
	keyBlock := make([]byte, keyBlockSize)
	if err := openvpnPRF(master[:], "OpenVPN key expansion",
		sources.Client.Random2[:], sources.Server.Random2[:], clientSession[:], serverSession[:], keyBlock); err != nil {
		return nil, err
	}
	// keyBlock = [ client->server slot | server->client slot ], each
	// 128 bytes = 64-byte cipher key + 64-byte HMAC key.
	const slot = maxCipherKeyLength + maxHMACKeyLength
	c2s, s2c := keyBlock[:slot], keyBlock[slot:]
	send, recv := c2s, s2c
	if isServer {
		send, recv = s2c, c2s
	}
	return &KeyMaterial{
		SendCipherKey: cloneBytes(send[:dataCipherKeyMax]),
		SendHMACKey:   cloneBytes(send[maxCipherKeyLength : maxCipherKeyLength+maxHMACKeyLength]),
		RecvCipherKey: cloneBytes(recv[:dataCipherKeyMax]),
		RecvHMACKey:   cloneBytes(recv[maxCipherKeyLength : maxCipherKeyLength+maxHMACKeyLength]),
	}, nil
}

// openvpnPRF is the TLS 1.0 PRF: split the secret in half, run P_MD5 over
// one half and P_SHA1 over the other, XOR the streams.
func openvpnPRF(secret []byte, label string, clientSeed, serverSeed, clientSession, serverSession, out []byte) error {
	seed := make([]byte, 0, len(label)+len(clientSeed)+len(serverSeed)+len(clientSession)+len(serverSession))
	seed = append(seed, label...)
	seed = append(seed, clientSeed...)
	seed = append(seed, serverSeed...)
	seed = append(seed, clientSession...)
	seed = append(seed, serverSession...)

	split := (len(secret) + 1) / 2
	s1 := secret[:split]
	s2 := secret[len(secret)-split:]
	md5Out := pHash(md5.New, s1, seed, len(out))
	sha1Out := pHash(sha1.New, s2, seed, len(out))
	for i := range out {
		out[i] = md5Out[i] ^ sha1Out[i]
	}
	return nil
}

func pHash(newHash func() hash.Hash, secret, seed []byte, size int) []byte {
	out := make([]byte, 0, size)
	a := hmacSum(newHash, secret, seed)
	for len(out) < size {
		out = append(out, hmacSum(newHash, secret, append(append([]byte(nil), a...), seed...))...)
		a = hmacSum(newHash, secret, a)
	}
	return out[:size]
}

func hmacSum(newHash func() hash.Hash, key, data []byte) []byte {
	mac := hmac.New(newHash, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func appendOpenVPNString(out []byte, s string) []byte {
	if s == "" {
		return binary.BigEndian.AppendUint16(out, 0)
	}
	if len(s)+1 > 0xffff {
		s = s[:0xfffe]
	}
	out = binary.BigEndian.AppendUint16(out, uint16(len(s)+1))
	out = append(out, s...)
	return append(out, 0)
}

func readOpenVPNString(packet []byte, offset int) (string, int, error) {
	if offset+2 > len(packet) {
		return "", offset, errKeyMethodTruncated
	}
	size := int(binary.BigEndian.Uint16(packet[offset : offset+2]))
	offset += 2
	if size == 0 {
		return "", offset, nil
	}
	if offset+size > len(packet) {
		return "", offset, errKeyMethodTruncated
	}
	raw := packet[offset : offset+size]
	offset += size
	if len(raw) > 0 && raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	return string(raw), offset, nil
}
