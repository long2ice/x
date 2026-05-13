package wireguard

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// KeyToHex accepts a WireGuard key in either base64 (44 chars) or hex (64
// chars) form and returns the hex form expected by wireguard-go's UAPI.
func KeyToHex(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) == 64 {
		if _, err := hex.DecodeString(s); err == nil {
			return strings.ToLower(s), nil
		}
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	if len(b) != 32 {
		return "", fmt.Errorf("expected 32-byte key, got %d", len(b))
	}
	return hex.EncodeToString(b), nil
}
