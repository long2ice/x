package vless

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"time"

	"github.com/go-gost/core/bypass"
	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
	"github.com/go-gost/x/registry"
)

type metadata struct {
	users       map[string]string
	readTimeout time.Duration
	// idleTimeout bounds how long a proxied TCP conn can sit fully idle
	// before it is reclaimed. Proxy clients (e.g. video players opening a
	// connection per media request) may keep finished conns open forever;
	// without this bound they pile up until the kernel runs out of TCP
	// memory.
	idleTimeout   time.Duration
	bufferSize    int
	hash          string
	enableUDP     bool
	udpBufferSize int
	visionDirect  bool

	observerPeriod       time.Duration
	observerResetTraffic bool

	sniffing                    bool
	sniffingTimeout             time.Duration
	sniffingWebsocket           bool
	sniffingWebsocketSampleRate float64

	certificate *x509.Certificate
	privateKey  crypto.PrivateKey
	alpn        string
	mitmBypass  bypass.Bypass

	limiterRefreshInterval time.Duration
	limiterCleanupInterval time.Duration
}

func (h *vlessHandler) parseMetadata(md mdata.Metadata) (err error) {
	h.md.readTimeout = mdutil.GetDuration(md, "readTimeout")
	if h.md.readTimeout <= 0 {
		h.md.readTimeout = 15 * time.Second
	}

	h.md.idleTimeout = mdutil.GetDuration(md, "idleTimeout")
	if h.md.idleTimeout <= 0 {
		h.md.idleTimeout = 90 * time.Second
	}

	h.md.bufferSize = mdutil.GetInt(md, "tcp.bufferSize", "bufferSize")
	if h.md.bufferSize <= 0 {
		// VLESS nodes often carry many concurrent streams. A smaller per-direction
		// copy buffer substantially reduces their live heap without changing the
		// buffer used by other handlers.
		h.md.bufferSize = 16 * 1024
	}

	h.md.users = mdutil.GetStringMapString(md, "users")
	if uuid := mdutil.GetString(md, "uuid", "id"); uuid != "" {
		if h.md.users == nil {
			h.md.users = make(map[string]string)
		}
		h.md.users[mdutil.GetString(md, "user", "name")] = uuid
	}

	h.md.hash = mdutil.GetString(md, "hash")

	// UDP over TCP is an integral part of VLESS, keep it on unless disabled.
	h.md.enableUDP = true
	if md != nil && md.IsExists("udp") {
		h.md.enableUDP = mdutil.GetBool(md, "udp")
	}
	h.md.udpBufferSize = mdutil.GetInt(md, "udp.bufferSize", "udpBufferSize")
	if h.md.udpBufferSize <= 0 {
		// XUDP clients drop anything above 7526 bytes anyway.
		h.md.udpBufferSize = 8192
	}

	// With xtls-rprx-vision, hand the transport over to direct copy once the
	// proxied traffic is known to be TLS 1.3, as Xray does.
	h.md.visionDirect = true
	if md != nil && md.IsExists("vision.direct") {
		h.md.visionDirect = mdutil.GetBool(md, "vision.direct")
	}

	h.md.observerPeriod = mdutil.GetDuration(md, "observePeriod", "observer.period", "observer.observePeriod")
	if h.md.observerPeriod == 0 {
		h.md.observerPeriod = 5 * time.Second
	}
	if h.md.observerPeriod < time.Second {
		h.md.observerPeriod = time.Second
	}
	h.md.observerResetTraffic = mdutil.GetBool(md, "observer.resetTraffic")

	h.md.sniffing = mdutil.GetBool(md, "sniffing")
	h.md.sniffingTimeout = mdutil.GetDuration(md, "sniffing.timeout")
	if h.md.sniffingTimeout <= 0 {
		// Server-first protocols (VNC, SMTP, MySQL, etc.) never send the
		// first byte, so Peek would block forever and deadlock the dial.
		h.md.sniffingTimeout = 500 * time.Millisecond
	}
	h.md.sniffingWebsocket = mdutil.GetBool(md, "sniffing.websocket")
	h.md.sniffingWebsocketSampleRate = mdutil.GetFloat(md, "sniffing.websocket.sampleRate")

	certFile := mdutil.GetString(md, "mitm.certFile", "mitm.caCertFile")
	keyFile := mdutil.GetString(md, "mitm.keyFile", "mitm.caKeyFile")
	if certFile != "" && keyFile != "" {
		tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return err
		}
		h.md.certificate, err = x509.ParseCertificate(tlsCert.Certificate[0])
		if err != nil {
			return err
		}
		h.md.privateKey = tlsCert.PrivateKey
	}
	h.md.alpn = mdutil.GetString(md, "mitm.alpn")
	h.md.mitmBypass = registry.BypassRegistry().Get(mdutil.GetString(md, "mitm.bypass"))

	h.md.limiterRefreshInterval = mdutil.GetDuration(md, "limiter.refreshInterval")
	h.md.limiterCleanupInterval = mdutil.GetDuration(md, "limiter.cleanupInterval")

	return nil
}
