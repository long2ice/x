package openvpn

import (
	"errors"
	"time"

	md "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

const defaultHandshakeTimeout = 15 * time.Second

type metadata struct {
	key              []byte
	udp              bool
	handshakeTimeout time.Duration
}

func (d *openvpnDialer) parseMetadata(m md.Metadata) error {
	key := mdutil.GetString(m, "key")
	if key == "" {
		return errors.New("openvpn dialer: metadata 'key' is required")
	}
	d.md.key = []byte(key)

	d.md.udp = mdutil.GetBool(m, "udp")

	d.md.handshakeTimeout = mdutil.GetDuration(m, "handshakeTimeout")
	if d.md.handshakeTimeout <= 0 {
		d.md.handshakeTimeout = defaultHandshakeTimeout
	}
	return nil
}
