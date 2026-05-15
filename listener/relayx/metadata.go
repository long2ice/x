package relayx

import (
	"math/rand/v2"
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

	path         string
	decoyBody    string
	serverHeader string

	backlog          int
	readTimeout      time.Duration
	replayWindow     time.Duration
	maxReplayEntries int
	maxHeaderBytes   int

	mux    bool
	muxCfg *mux.Config

	// wspad toggles the WebSocket padding obfuscation layer. When true (the
	// default) every binary frame is padded to a bucket size; when false the
	// inner stream is sent as raw WS binary messages. Both peers MUST agree.
	wspad bool
}

var serverHeaderPool = []string{
	"nginx/1.24.0",
	"nginx/1.26.2",
	"nginx",
	"Apache/2.4.58",
	"Apache",
	"cloudflare",
	"Caddy",
	"AmazonS3",
}

func pickServerHeader() string {
	return serverHeaderPool[rand.IntN(len(serverHeaderPool))]
}

func (l *relayxListener) parseMetadata(md mdata.Metadata) error {
	l.md.key = mdutil.GetString(md, "key")
	l.md.path = mdutil.GetString(md, "path")
	l.md.decoyBody = mdutil.GetString(md, "decoyBody", "decoy")
	l.md.serverHeader = mdutil.GetString(md, "serverHeader")
	if l.md.serverHeader == "" {
		l.md.serverHeader = pickServerHeader()
	}

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

	if mdutil.IsExists(md, "wspad") {
		l.md.wspad = mdutil.GetBool(md, "wspad")
	} else {
		l.md.wspad = true
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
