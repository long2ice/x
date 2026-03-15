package relayx

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultBacklog = 128
)

type metadata struct {
	key string

	path      string
	decoyBody string

	backlog        int
	readTimeout    time.Duration
	replayWindow   time.Duration
	maxHeaderBytes int
}

func (l *relayxListener) parseMetadata(md mdata.Metadata) error {
	l.md.key = mdutil.GetString(md, "key")
	l.md.path = mdutil.GetString(md, "path")
	l.md.decoyBody = mdutil.GetString(md, "decoyBody", "decoy")

	l.md.backlog = mdutil.GetInt(md, "backlog")
	if l.md.backlog <= 0 {
		l.md.backlog = defaultBacklog
	}

	l.md.readTimeout = mdutil.GetDuration(md, "readTimeout")
	if l.md.readTimeout <= 0 {
		l.md.readTimeout = 15 * time.Second
	}

	l.md.replayWindow = mdutil.GetDuration(md, "replayWindow")
	if l.md.replayWindow <= 0 {
		l.md.replayWindow = 5 * time.Minute
	}

	l.md.maxHeaderBytes = mdutil.GetInt(md, "maxHeaderBytes")
	if l.md.maxHeaderBytes <= 0 {
		l.md.maxHeaderBytes = 32 << 10
	}

	return nil
}
