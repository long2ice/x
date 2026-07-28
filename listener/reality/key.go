package reality

import (
	"crypto/rand"
	"encoding/base64"

	"golang.org/x/crypto/curve25519"
)

// GenerateKeyPair returns a X25519 key pair in the base64 form the clients and
// the privateKey metadata use, the same one `xray x25519` prints.
func GenerateKeyPair() (privateKey, publicKey string, err error) {
	private := make([]byte, curve25519.ScalarSize)
	if _, err = rand.Read(private); err != nil {
		return "", "", err
	}

	// Clamp the scalar, as X25519 expects.
	private[0] &= 248
	private[31] &= 127
	private[31] |= 64

	public, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}

	return base64.RawURLEncoding.EncodeToString(private),
		base64.RawURLEncoding.EncodeToString(public), nil
}
