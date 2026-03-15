package relayx

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

type metadata struct {
	key       string
	host      string
	path      string
	userAgent string

	handshakeTimeout time.Duration
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
	return nil
}
