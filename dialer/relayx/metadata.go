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

	mux    bool
	muxCfg *mux.Config
}

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
