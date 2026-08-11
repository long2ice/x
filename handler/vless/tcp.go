package vless

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/go-gost/core/bypass"
	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/observer/stats"
	xctx "github.com/go-gost/x/ctx"
	ictx "github.com/go-gost/x/internal/ctx"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/util/sniffing"
	traffic_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	stats_wrapper "github.com/go-gost/x/observer/stats/wrapper"
	xrecorder "github.com/go-gost/x/recorder"
)

func (h *vlessHandler) handleTCP(ctx context.Context, conn net.Conn, network, address string, ro *xrecorder.HandlerRecorderObject, log logger.Logger) error {
	log = log.WithFields(map[string]any{
		"dst":  address,
		"cmd":  "tcp",
		"host": address,
	})
	log.Debugf("%s >> %s", conn.RemoteAddr(), address)

	conn, done := h.wrapClientConn(ctx, conn, network, address)
	defer done()

	if h.options.Bypass != nil && h.options.Bypass.Contains(ctx, network, address, bypass.WithService(h.options.Service)) {
		log.Debug("bypass: ", address)
		return nil
	}

	switch h.md.hash {
	case "host":
		ctx = xctx.ContextWithHash(ctx, &xctx.Hash{Source: address})
	}

	var buf bytes.Buffer
	cc, err := h.options.Router.Dial(ictx.ContextWithBuffer(ctx, &buf), network, address)
	ro.Route = buf.String()
	if err != nil {
		log.Error(err)
		return err
	}
	defer cc.Close()

	log = log.WithFields(map[string]any{"src": cc.LocalAddr().String(), "dst": cc.RemoteAddr().String()})
	ro.SrcAddr = cc.LocalAddr().String()
	ro.DstAddr = cc.RemoteAddr().String()

	if h.md.sniffing {
		if h.md.sniffingTimeout > 0 {
			conn.SetReadDeadline(time.Now().Add(h.md.sniffingTimeout))
		}

		br := bufio.NewReader(conn)
		proto, _ := sniffing.Sniff(ctx, br)
		ro.Proto = proto

		if h.md.sniffingTimeout > 0 {
			conn.SetReadDeadline(time.Time{})
		}

		dial := func(ctx context.Context, network, address string) (net.Conn, error) {
			return cc, nil
		}
		dialTLS := func(ctx context.Context, network, address string, cfg *tls.Config) (net.Conn, error) {
			return cc, nil
		}
		sniffer := &sniffing.Sniffer{
			Websocket:           h.md.sniffingWebsocket,
			WebsocketSampleRate: h.md.sniffingWebsocketSampleRate,
			Recorder:            h.recorder.Recorder,
			RecorderOptions:     h.recorder.Options,
			Certificate:         h.md.certificate,
			PrivateKey:          h.md.privateKey,
			NegotiatedProtocol:  h.md.alpn,
			CertPool:            h.certPool,
			MitmBypass:          h.md.mitmBypass,
			ReadTimeout:         h.md.readTimeout,
			IdleTimeout:         h.md.idleTimeout,
		}

		conn = xnet.NewReadWriteConn(br, conn, conn)
		switch proto {
		case sniffing.ProtoHTTP:
			return sniffer.HandleHTTP(ctx, "tcp", conn,
				sniffing.WithService(h.options.Service),
				sniffing.WithDial(dial),
				sniffing.WithDialTLS(dialTLS),
				sniffing.WithRecorderObject(ro),
				sniffing.WithLog(log),
			)
		case sniffing.ProtoTLS:
			return sniffer.HandleTLS(ctx, "tcp", conn,
				sniffing.WithService(h.options.Service),
				sniffing.WithDial(dial),
				sniffing.WithDialTLS(dialTLS),
				sniffing.WithRecorderObject(ro),
				sniffing.WithLog(log),
			)
		}
	}

	t := time.Now()
	log.Infof("%s <-> %s", conn.RemoteAddr(), address)
	xnet.PipeIdle(ctx, conn, cc, h.md.idleTimeout)
	log.WithFields(map[string]any{
		"duration": time.Since(t),
	}).Infof("%s >-< %s", conn.RemoteAddr(), address)

	return nil
}

// wrapClientConn applies the per client traffic limiter and stats to conn.
// The returned function must be called once the connection is done with.
func (h *vlessHandler) wrapClientConn(ctx context.Context, conn net.Conn, network, address string) (net.Conn, func()) {
	clientID := xctx.ClientIDFromContext(ctx)

	rw := traffic_wrapper.WrapReadWriter(
		h.limiter,
		conn,
		string(clientID),
		limiter.ServiceOption(h.options.Service),
		limiter.ScopeOption(limiter.ScopeClient),
		limiter.NetworkOption(network),
		limiter.AddrOption(address),
		limiter.ClientOption(string(clientID)),
		limiter.SrcOption(conn.RemoteAddr().String()),
	)

	done := func() {}
	if h.options.Observer != nil {
		pstats := h.stats.Stats(string(clientID))
		pstats.Add(stats.KindTotalConns, 1)
		pstats.Add(stats.KindCurrentConns, 1)
		done = func() {
			pstats.Add(stats.KindCurrentConns, -1)
		}
		rw = stats_wrapper.WrapReadWriter(rw, pstats)
	}

	return xnet.NewReadWriteConn(rw, rw, conn), done
}
