package openvpn

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// tls-crypt wraps every control-channel packet with HMAC-SHA256
// authentication and AES-256-CTR encryption, keyed by a 256-byte static
// key shared out of band (the `<tls-crypt>` block in an .ovpn file).
//
// On-wire control packet under tls-crypt:
//
//	opcode|key_id   1B  ┐
//	session_id      8B  ├ authenticated, cleartext (the "header")
//	packet_id       4B  ├
//	net_time        4B  ┘
//	HMAC-SHA256     32B  tag over header+plaintext
//	ciphertext      var  AES-256-CTR(plaintext), IV = tag[:16]
const (
	tlsCryptHeaderSize = 1 + SessionIDSize // opcode|kid + session id
	tlsCryptPIDSize    = 4 + 4             // packet id + net time
	tlsCryptTagSize    = sha256.Size

	tlsCryptStaticKeySize = 256
	tlsCryptKeySlotSize   = 128
	tlsCryptCipherKeySize = 32
	tlsCryptHMACKeySize   = 32
)

// TLSCrypt holds the directional cipher/HMAC keys derived from a static key.
type TLSCrypt struct {
	encryptCipherKey []byte
	encryptHMACKey   []byte
	decryptCipherKey []byte
	decryptHMACKey   []byte
}

// NewTLSCrypt splits a 256-byte static key into directional keys. The
// static key holds two 128-byte slots; `client` selects which slot is
// used for sending vs receiving so the two ends mirror each other.
func NewTLSCrypt(staticKey []byte, client bool) (*TLSCrypt, error) {
	if len(staticKey) != tlsCryptStaticKeySize {
		return nil, fmt.Errorf("openvpn: tls-crypt static key is %d bytes, want %d", len(staticKey), tlsCryptStaticKeySize)
	}
	slot0 := staticKey[:tlsCryptKeySlotSize]
	slot1 := staticKey[tlsCryptKeySlotSize:]
	encrypt, decrypt := slot0, slot1
	if client {
		encrypt, decrypt = slot1, slot0
	}
	// Within a 128-byte slot: cipher key at [0:64], HMAC key at [64:128]
	// (only the first 32 bytes of each are used for AES-256 / SHA-256).
	return &TLSCrypt{
		encryptCipherKey: cloneBytes(encrypt[:tlsCryptCipherKeySize]),
		encryptHMACKey:   cloneBytes(encrypt[64 : 64+tlsCryptHMACKeySize]),
		decryptCipherKey: cloneBytes(decrypt[:tlsCryptCipherKeySize]),
		decryptHMACKey:   cloneBytes(decrypt[64 : 64+tlsCryptHMACKeySize]),
	}, nil
}

// Wrap authenticates and encrypts one control packet. header is the
// cleartext opcode|kid + session id; plaintext is the encoded body.
func (c *TLSCrypt) Wrap(header []byte, packetID, unixTime uint32, plaintext []byte) ([]byte, error) {
	if len(header) != tlsCryptHeaderSize {
		return nil, fmt.Errorf("openvpn: tls-crypt header is %d bytes, want %d", len(header), tlsCryptHeaderSize)
	}
	ad := make([]byte, 0, len(header)+tlsCryptPIDSize)
	ad = append(ad, header...)
	ad = binary.BigEndian.AppendUint32(ad, packetID)
	ad = binary.BigEndian.AppendUint32(ad, unixTime)

	tag := tlsCryptHMAC(c.encryptHMACKey, ad, plaintext)
	ciphertext, err := aes256ctr(c.encryptCipherKey, tag[:aes.BlockSize], plaintext)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(ad)+len(tag)+len(ciphertext))
	out = append(out, ad...)
	out = append(out, tag...)
	out = append(out, ciphertext...)
	return out, nil
}

// Unwrap reverses Wrap, returning the cleartext header and decrypted body.
func (c *TLSCrypt) Unwrap(packet []byte) (header []byte, packetID, unixTime uint32, plaintext []byte, err error) {
	if len(packet) < tlsCryptHeaderSize+tlsCryptPIDSize+tlsCryptTagSize {
		return nil, 0, 0, nil, errors.New("openvpn: tls-crypt packet too short")
	}
	adEnd := tlsCryptHeaderSize + tlsCryptPIDSize
	tagEnd := adEnd + tlsCryptTagSize
	ad := packet[:adEnd]
	tag := packet[adEnd:tagEnd]
	ciphertext := packet[tagEnd:]

	plaintext, err = aes256ctr(c.decryptCipherKey, tag[:aes.BlockSize], ciphertext)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	if !hmac.Equal(tag, tlsCryptHMAC(c.decryptHMACKey, ad, plaintext)) {
		return nil, 0, 0, nil, errors.New("openvpn: tls-crypt authentication failed")
	}
	header = cloneBytes(packet[:tlsCryptHeaderSize])
	packetID = binary.BigEndian.Uint32(packet[tlsCryptHeaderSize : tlsCryptHeaderSize+4])
	unixTime = binary.BigEndian.Uint32(packet[tlsCryptHeaderSize+4 : adEnd])
	return header, packetID, unixTime, plaintext, nil
}

func tlsCryptHMAC(key []byte, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, key)
	for _, p := range parts {
		_, _ = mac.Write(p)
	}
	return mac.Sum(nil)
}

func aes256ctr(key, iv, in []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := cloneBytes(in)
	cipher.NewCTR(block, iv).XORKeyStream(out, out)
	return out, nil
}

// DecodeStaticKey parses an OpenVPN static key file (the hex body of a
// `-----BEGIN OpenVPN Static key V1-----` block, or the inline
// `<tls-crypt>` contents) into its 256 raw bytes.
func DecodeStaticKey(block []byte) ([]byte, error) {
	var hexLines []string
	for _, raw := range strings.Split(string(block), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-----") {
			continue
		}
		hexLines = append(hexLines, line)
	}
	key, err := hex.DecodeString(strings.Join(hexLines, ""))
	if err != nil {
		return nil, fmt.Errorf("openvpn: decode static key: %w", err)
	}
	if len(key) != tlsCryptStaticKeySize {
		return nil, fmt.Errorf("openvpn: static key is %d bytes, want %d", len(key), tlsCryptStaticKeySize)
	}
	return key, nil
}
