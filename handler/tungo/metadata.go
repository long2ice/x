package tungo

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultBufferSize = 4096

	// The gVisor TCP endpoints created here are not loopback-local: they
	// terminate connections whose other end sits across the tunnel (WAN
	// RTTs). Window = throughput × RTT, so the stock 1MB/4MB gvisor buffer
	// limits cap a single flow at ~1MB/RTT. Start at 1MB and let both-side
	// auto-tuning grow toward an 8MB ceiling (~14MB/s at 550ms RTT).
	defaultTCPBufferSize    = 1 << 20
	defaultTCPMaxBufferSize = 8 << 20

	// cubic ramps far faster than tun2socks' default reno on long fat paths.
	defaultTCPCongestionControl = "cubic"
)

type metadata struct {
	udpTimeout    time.Duration
	udpBufferSize int

	// tcpIdleTimeout bounds how long a proxied TCP conn can sit fully idle
	// before being force-closed. Mobile peers (Wi-Fi/cellular switch, app
	// killed) often abandon connections without sending FIN, leaving the
	// server-side gVisor TCB and handler goroutine alive until TCP keepalive
	// reaps them ~5 min later. This caps that delay.
	tcpIdleTimeout time.Duration

	sniffing                bool
	sniffingUDP             bool
	sniffingTimeout         time.Duration
	sniffingResponseTimeout time.Duration
	sniffingFallback        bool

	observerPeriod       time.Duration
	observerResetTraffic bool

	limiterRefreshInterval time.Duration
	limiterCleanupInterval time.Duration

	multicastGroups []netip.Addr

	ipv6 bool

	tcpSendBufferSize        int
	tcpSendBufferMaxSize     int
	tcpReceiveBufferSize     int
	tcpReceiveBufferMaxSize  int
	tcpModerateReceiveBuffer bool
	tcpCongestionControl     string
}

func (h *tungoHandler) parseMetadata(md mdata.Metadata) (err error) {
	h.md.udpTimeout = mdutil.GetDuration(md, "udpTimeout", "tungo.udpTimeout")
	h.md.udpBufferSize = mdutil.GetInt(md, "udp.bufferSize", "udpBufferSize")

	h.md.tcpIdleTimeout = mdutil.GetDuration(md, "tcpIdleTimeout", "tungo.tcpIdleTimeout")
	if h.md.tcpIdleTimeout <= 0 {
		h.md.tcpIdleTimeout = 60 * time.Second
	}

	h.md.sniffing = mdutil.GetBool(md, "sniffing")
	h.md.sniffingUDP = mdutil.GetBool(md, "sniffing.udp")
	h.md.sniffingTimeout = mdutil.GetDuration(md, "sniffing.timeout")
	h.md.sniffingResponseTimeout = mdutil.GetDuration(md, "sniffing.responseTimeout")
	h.md.sniffingFallback = mdutil.GetBool(md, "sniffing.fallback")

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

	for _, v := range strings.Split(mdutil.GetString(md, "multicastGroups", "tungo.multicastGroups"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		addr, err := netip.ParseAddr(v)
		if err != nil {
			return err
		}
		if !addr.IsMulticast() {
			return fmt.Errorf("invalid multicast IP: %s", addr)
		}
		h.md.multicastGroups = append(h.md.multicastGroups, addr)
	}

	h.md.ipv6 = mdutil.GetBool(md, "ipv6")

	h.md.tcpSendBufferSize = mdutil.GetInt(md, "tcpSendBufferSize", "tungo.tcpSendBufferSize")
	if h.md.tcpSendBufferSize <= 0 {
		h.md.tcpSendBufferSize = defaultTCPBufferSize
	}
	h.md.tcpSendBufferMaxSize = mdutil.GetInt(md, "tcpSendBufferMaxSize", "tungo.tcpSendBufferMaxSize")
	if h.md.tcpSendBufferMaxSize <= 0 {
		h.md.tcpSendBufferMaxSize = defaultTCPMaxBufferSize
	}
	h.md.tcpReceiveBufferSize = mdutil.GetInt(md, "tcpReceiveBufferSize", "tungo.tcpReceiveBufferSize")
	if h.md.tcpReceiveBufferSize <= 0 {
		h.md.tcpReceiveBufferSize = defaultTCPBufferSize
	}
	h.md.tcpReceiveBufferMaxSize = mdutil.GetInt(md, "tcpReceiveBufferMaxSize", "tungo.tcpReceiveBufferMaxSize")
	if h.md.tcpReceiveBufferMaxSize <= 0 {
		h.md.tcpReceiveBufferMaxSize = defaultTCPMaxBufferSize
	}

	// Default on: receive-window auto-tuning is what lets the window grow
	// toward the max on long-RTT paths. Explicit metadata still wins.
	h.md.tcpModerateReceiveBuffer = true
	if md.IsExists("tcpModerateReceiveBuffer") || md.IsExists("tungo.tcpModerateReceiveBuffer") {
		h.md.tcpModerateReceiveBuffer = mdutil.GetBool(md, "tcpModerateReceiveBuffer", "tungo.tcpModerateReceiveBuffer")
	}

	h.md.tcpCongestionControl = mdutil.GetString(md, "tcpCongestionControl", "tungo.tcpCongestionControl")
	if h.md.tcpCongestionControl == "" {
		h.md.tcpCongestionControl = defaultTCPCongestionControl
	}

	return
}
