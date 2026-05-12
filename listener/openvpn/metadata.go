package openvpn

import (
	"errors"
	"time"

	md "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultBacklog          = 128
	defaultHandshakeTimeout = 15 * time.Second
	defaultIdleTimeout      = 5 * time.Minute
)

type metadata struct {
	key              []byte
	udp              bool
	backlog          int
	handshakeTimeout time.Duration
	idleTimeout      time.Duration
}

func (l *openvpnListener) parseMetadata(m md.Metadata) error {
	key := mdutil.GetString(m, "key")
	if key == "" {
		return errors.New("openvpn listener: metadata 'key' is required")
	}
	l.md.key = []byte(key)

	l.md.udp = mdutil.GetBool(m, "udp")

	l.md.backlog = mdutil.GetInt(m, "backlog")
	if l.md.backlog <= 0 {
		l.md.backlog = defaultBacklog
	}
	l.md.handshakeTimeout = mdutil.GetDuration(m, "handshakeTimeout")
	if l.md.handshakeTimeout <= 0 {
		l.md.handshakeTimeout = defaultHandshakeTimeout
	}
	l.md.idleTimeout = mdutil.GetDuration(m, "idleTimeout")
	if l.md.idleTimeout <= 0 {
		l.md.idleTimeout = defaultIdleTimeout
	}
	return nil
}
