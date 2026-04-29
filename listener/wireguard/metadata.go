package wireguard

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultMTU      = 1420
	defaultQueueLen = 512
)

type peerConfig struct {
	publicKey           string // hex encoded
	presharedKey        string // hex encoded, optional
	allowedIPs          []string
	endpoint            string // optional, "host:port"
	persistentKeepalive int    // seconds, optional
}

type metadata struct {
	privateKey string // hex encoded
	mtu        int
	queueLen   int
	peers      []peerConfig
	logLevel   string // "silent" | "error" | "verbose"
}

func (l *wgListener) parseMetadata(md mdata.Metadata) error {
	pk := mdutil.GetString(md, "privateKey", "wireguard.privateKey")
	if pk == "" {
		return fmt.Errorf("wireguard: privateKey is required")
	}
	pkHex, err := keyToHex(pk)
	if err != nil {
		return fmt.Errorf("wireguard: invalid privateKey: %w", err)
	}
	l.md.privateKey = pkHex

	l.md.mtu = mdutil.GetInt(md, "mtu", "wireguard.mtu")
	if l.md.mtu <= 0 {
		l.md.mtu = defaultMTU
	}

	l.md.queueLen = mdutil.GetInt(md, "queueLen", "wireguard.queueLen")
	if l.md.queueLen <= 0 {
		l.md.queueLen = defaultQueueLen
	}

	l.md.logLevel = strings.ToLower(mdutil.GetString(md, "logLevel", "wireguard.logLevel"))

	peers, err := parsePeers(md)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return fmt.Errorf("wireguard: at least one peer is required")
	}
	l.md.peers = peers

	return nil
}

func parsePeers(md mdata.Metadata) ([]peerConfig, error) {
	raw := md.Get("peers")
	if raw == nil {
		raw = md.Get("wireguard.peers")
	}
	list, _ := raw.([]any)

	var peers []peerConfig
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("wireguard: peers[%d] is not a map", i)
		}

		pubRaw, _ := m["publicKey"].(string)
		if pubRaw == "" {
			return nil, fmt.Errorf("wireguard: peers[%d] missing publicKey", i)
		}
		pubHex, err := keyToHex(pubRaw)
		if err != nil {
			return nil, fmt.Errorf("wireguard: peers[%d] invalid publicKey: %w", i, err)
		}

		p := peerConfig{publicKey: pubHex}

		if psk, _ := m["presharedKey"].(string); psk != "" {
			pskHex, err := keyToHex(psk)
			if err != nil {
				return nil, fmt.Errorf("wireguard: peers[%d] invalid presharedKey: %w", i, err)
			}
			p.presharedKey = pskHex
		}

		switch v := m["allowedIPs"].(type) {
		case []any:
			for _, x := range v {
				if s, ok := x.(string); ok && s != "" {
					p.allowedIPs = append(p.allowedIPs, s)
				}
			}
		case string:
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					p.allowedIPs = append(p.allowedIPs, s)
				}
			}
		}
		if len(p.allowedIPs) == 0 {
			return nil, fmt.Errorf("wireguard: peers[%d] missing allowedIPs", i)
		}

		if ep, _ := m["endpoint"].(string); ep != "" {
			p.endpoint = ep
		}

		switch v := m["persistentKeepalive"].(type) {
		case int:
			p.persistentKeepalive = v
		case int64:
			p.persistentKeepalive = int(v)
		case float64:
			p.persistentKeepalive = int(v)
		}

		peers = append(peers, p)
	}
	return peers, nil
}

// keyToHex accepts a WireGuard key in either base64 (44 chars) or hex (64
// chars) form and returns the hex form expected by wireguard-go's UAPI.
func keyToHex(s string) (string, error) {
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
