package relayx

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	"github.com/go-gost/x/internal/util/mux"
	mdutil "github.com/go-gost/x/metadata/util"
)

type metadata struct {
	key       string
	host      string
	path      string
	userAgent string

	handshakeTimeout time.Duration
	muxIdleTimeout   time.Duration

	mux    bool
	muxCfg *mux.Config

	// wspad toggles the WebSocket padding obfuscation layer on the outgoing
	// session. Defaults to false. Both peers MUST agree: configure the same
	// value on the matching listener or the wire format will not line up.
	wspad bool
}

const defaultMuxIdleTimeout = 30 * time.Second

func (d *relayxDialer) parseMetadata(md mdata.Metadata) error {
	d.md.key = mdutil.GetString(md, "key")
	d.md.host = mdutil.GetString(md, "host")
	d.md.path = mdutil.GetString(md, "path")
	d.md.userAgent = mdutil.GetString(md, "userAgent")
	d.md.handshakeTimeout = mdutil.GetDuration(md, "handshakeTimeout")
	if d.md.handshakeTimeout <= 0 {
		d.md.handshakeTimeout = 15 * time.Second
	}

	if mdutil.IsExists(md, "mux") {
		d.md.mux = mdutil.GetBool(md, "mux")
	} else {
		d.md.mux = true
	}

	d.md.wspad = mdutil.GetBool(md, "wspad")
	if mdutil.IsExists(md, "mux.idleTimeout", "mux.idle.timeout") {
		d.md.muxIdleTimeout = mdutil.GetDuration(md, "mux.idleTimeout", "mux.idle.timeout")
	} else {
		d.md.muxIdleTimeout = defaultMuxIdleTimeout
	}
	d.md.muxCfg = &mux.Config{
		Version:           mdutil.GetInt(md, "mux.version"),
		KeepAliveInterval: mdutil.GetDuration(md, "mux.keepaliveInterval"),
		KeepAliveDisabled: mdutil.GetBool(md, "mux.keepaliveDisabled"),
		KeepAliveTimeout:  mdutil.GetDuration(md, "mux.keepaliveTimeout"),
		MaxFrameSize:      mdutil.GetInt(md, "mux.maxFrameSize"),
		MaxReceiveBuffer:  mdutil.GetInt(md, "mux.maxReceiveBuffer"),
		MaxStreamBuffer:   mdutil.GetInt(md, "mux.maxStreamBuffer"),
	}

	return nil
}
