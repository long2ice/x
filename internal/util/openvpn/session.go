package openvpn

import (
	"crypto/rand"
	"encoding/hex"
)

type SessionID [8]byte

func NewSessionID() (SessionID, error) {
	var s SessionID
	_, err := rand.Read(s[:])
	return s, err
}

func (s SessionID) String() string { return hex.EncodeToString(s[:]) }

func (s SessionID) IsZero() bool {
	for _, b := range s {
		if b != 0 {
			return false
		}
	}
	return true
}
