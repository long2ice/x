package wireguard

import (
	"fmt"
	"strings"

	md "github.com/go-gost/core/metadata"
	wgutil "github.com/go-gost/x/internal/util/wireguard"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultMTU                 = 1420
	defaultQueueLen            = 512
	defaultPersistentKeepalive = 25 // seconds; standard NAT-traversal heartbeat
)

type peerConfig struct {
	publicKey           string // hex encoded
	presharedKey        string // hex encoded, optional
	allowedIPs          []string
	persistentKeepalive int // seconds
}

type metadata struct {
	privateKey string
	mtu        int
	queueLen   int
	logLevel   string
	peer       peerConfig
}

func (d *wgDialer) parseMetadata(m md.Metadata) error {
	pk := mdutil.GetString(m, "privateKey", "wireguard.privateKey")
	if pk == "" {
		return fmt.Errorf("wireguard: privateKey is required")
	}
	pkHex, err := wgutil.KeyToHex(pk)
	if err != nil {
		return fmt.Errorf("wireguard: invalid privateKey: %w", err)
	}
	d.md.privateKey = pkHex

	pub := mdutil.GetString(m, "publicKey", "wireguard.publicKey", "peerPublicKey")
	if pub == "" {
		return fmt.Errorf("wireguard: peer publicKey is required")
	}
	pubHex, err := wgutil.KeyToHex(pub)
	if err != nil {
		return fmt.Errorf("wireguard: invalid peer publicKey: %w", err)
	}
	d.md.peer.publicKey = pubHex

	if psk := mdutil.GetString(m, "presharedKey", "wireguard.presharedKey"); psk != "" {
		pskHex, err := wgutil.KeyToHex(psk)
		if err != nil {
			return fmt.Errorf("wireguard: invalid presharedKey: %w", err)
		}
		d.md.peer.presharedKey = pskHex
	}

	d.md.peer.allowedIPs = parseAllowedIPs(m)
	if len(d.md.peer.allowedIPs) == 0 {
		// Default: full tunnel; a dialer rarely wants packets dropped just
		// because the user forgot to set allowedIPs.
		d.md.peer.allowedIPs = []string{"0.0.0.0/0", "::/0"}
	}

	if v := mdutil.GetInt(m, "persistentKeepalive", "wireguard.persistentKeepalive"); v > 0 {
		d.md.peer.persistentKeepalive = v
	} else {
		d.md.peer.persistentKeepalive = defaultPersistentKeepalive
	}

	d.md.mtu = mdutil.GetInt(m, "mtu", "wireguard.mtu")
	if d.md.mtu <= 0 {
		d.md.mtu = defaultMTU
	}
	d.md.queueLen = mdutil.GetInt(m, "queueLen", "wireguard.queueLen")
	if d.md.queueLen <= 0 {
		d.md.queueLen = defaultQueueLen
	}

	d.md.logLevel = strings.ToLower(mdutil.GetString(m, "logLevel", "wireguard.logLevel"))

	return nil
}

func parseAllowedIPs(m md.Metadata) []string {
	raw := m.Get("allowedIPs")
	if raw == nil {
		raw = m.Get("wireguard.allowedIPs")
	}
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
