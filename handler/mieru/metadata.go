package mieru

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

type metadata struct {
	readTimeout  time.Duration
	idleTimeout  time.Duration
	enableUDP    bool
	hash         string

	observerPeriod       time.Duration
	observerResetTraffic bool

	limiterRefreshInterval time.Duration
	limiterCleanupInterval time.Duration
}

func (h *mieruHandler) parseMetadata(md mdata.Metadata) error {
	h.md.readTimeout = mdutil.GetDuration(md, "readTimeout")
	if h.md.readTimeout <= 0 {
		h.md.readTimeout = 15 * time.Second
	}

	h.md.idleTimeout = mdutil.GetDuration(md, "idleTimeout")
	if h.md.idleTimeout <= 0 {
		h.md.idleTimeout = 5 * time.Minute
	}

	h.md.enableUDP = true
	if md != nil && md.IsExists("udp") {
		h.md.enableUDP = mdutil.GetBool(md, "udp")
	}

	h.md.hash = mdutil.GetString(md, "hash")

	h.md.observerPeriod = mdutil.GetDuration(md, "observePeriod", "observer.period", "observer.observePeriod")
	if h.md.observerPeriod == 0 {
		h.md.observerPeriod = 5 * time.Second
	}
	if h.md.observerPeriod < time.Second {
		h.md.observerPeriod = time.Second
	}
	h.md.observerResetTraffic = mdutil.GetBool(md, "observer.resetTraffic")

	h.md.limiterRefreshInterval = mdutil.GetDuration(md, "limiter.refreshInterval")
	h.md.limiterCleanupInterval = mdutil.GetDuration(md, "limiter.cleanupInterval")
	return nil
}
