package relayx

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	"github.com/go-gost/x/internal/util/mux"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultBacklog = 128
)

type metadata struct {
	key string

	path      string
	decoyBody string

	backlog          int
	readTimeout      time.Duration
	replayWindow     time.Duration
	maxReplayEntries int
	maxHeaderBytes   int

	mux    bool
	muxCfg *mux.Config
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

	l.md.maxReplayEntries = mdutil.GetInt(md, "maxReplayEntries")

	l.md.maxHeaderBytes = mdutil.GetInt(md, "maxHeaderBytes")
	if l.md.maxHeaderBytes <= 0 {
		l.md.maxHeaderBytes = 32 << 10
	}

	if mdutil.IsExists(md, "mux") {
		l.md.mux = mdutil.GetBool(md, "mux")
	} else {
		l.md.mux = true
	}
	l.md.muxCfg = &mux.Config{
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
