package mieru

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/constant"
	"github.com/enfein/mieru/v3/apis/model"
	"github.com/go-gost/core/bypass"
	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/limiter/traffic"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	"github.com/go-gost/core/observer/stats"
	"github.com/go-gost/core/recorder"
	xctx "github.com/go-gost/x/ctx"
	ictx "github.com/go-gost/x/internal/ctx"
	xnet "github.com/go-gost/x/internal/net"
	umieru "github.com/go-gost/x/internal/util/mieru"
	stats_util "github.com/go-gost/x/internal/util/stats"
	rate_limiter "github.com/go-gost/x/limiter/rate"
	cache_limiter "github.com/go-gost/x/limiter/traffic/cache"
	traffic_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	xstats "github.com/go-gost/x/observer/stats"
	stats_wrapper "github.com/go-gost/x/observer/stats/wrapper"
	xrecorder "github.com/go-gost/x/recorder"
	"github.com/go-gost/x/registry"
)

var (
	ErrNoRequest   = errors.New("mieru: no request")
	ErrUnknownCmd  = errors.New("mieru: unknown command")
	ErrUDPDisabled = errors.New("mieru: UDP is disabled")
)

func init() {
	registry.HandlerRegistry().Register("mieru", NewHandler)
}

type mieruHandler struct {
	md       metadata
	options  handler.Options
	stats    *stats_util.HandlerStats
	limiter  traffic.TrafficLimiter
	cancel   context.CancelFunc
	recorder recorder.RecorderObject
}

func NewHandler(opts ...handler.Option) handler.Handler {
	options := handler.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &mieruHandler{
		options: options,
	}
}

func (h *mieruHandler) Init(md md.Metadata) (err error) {
	if err = h.parseMetadata(md); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	if h.options.Observer != nil {
		h.stats = stats_util.NewHandlerStats(h.options.Service, h.md.observerResetTraffic)
		go h.observeStats(ctx)
	}

	if h.options.Limiter != nil {
		h.limiter = cache_limiter.NewCachedTrafficLimiter(h.options.Limiter,
			cache_limiter.RefreshIntervalOption(h.md.limiterRefreshInterval),
			cache_limiter.CleanupIntervalOption(h.md.limiterCleanupInterval),
			cache_limiter.ScopeOption(limiter.ScopeClient),
		)
	}

	for _, ro := range h.options.Recorders {
		if ro.Record == xrecorder.RecorderServiceHandler {
			h.recorder = ro
			break
		}
	}
	return
}

func (h *mieruHandler) Handle(ctx context.Context, conn net.Conn, opts ...handler.HandleOption) (err error) {
	defer conn.Close()

	start := time.Now()
	req := umieru.RequestFromContext(ctx)
	if req == nil {
		return ErrNoRequest
	}

	ro := &xrecorder.HandlerRecorderObject{
		Network:    "tcp",
		Service:    h.options.Service,
		RemoteAddr: conn.RemoteAddr().String(),
		LocalAddr:  conn.LocalAddr().String(),
		SID:        xctx.SidFromContext(ctx).String(),
		Time:       start,
	}

	if srcAddr := xctx.SrcAddrFromContext(ctx); srcAddr != nil {
		ro.ClientAddr = srcAddr.String()
	}

	log := h.options.Logger.WithFields(map[string]any{
		"network": ro.Network,
		"remote":  conn.RemoteAddr().String(),
		"local":   conn.LocalAddr().String(),
		"client":  ro.ClientAddr,
		"sid":     ro.SID,
	})
	log.Infof("%s <> %s", conn.RemoteAddr(), conn.LocalAddr())

	pStats := xstats.Stats{}
	conn = stats_wrapper.WrapConn(conn, &pStats)

	defer func() {
		if err != nil {
			ro.Err = err.Error()
		}
		ro.InputBytes = pStats.Get(stats.KindInputBytes)
		ro.OutputBytes = pStats.Get(stats.KindOutputBytes)
		ro.Duration = time.Since(start)
		if err := ro.Record(ctx, h.recorder.Recorder); err != nil {
			log.Errorf("record: %v", err)
		}

		log.WithFields(map[string]any{
			"network":     ro.Network,
			"duration":    time.Since(start),
			"inputBytes":  ro.InputBytes,
			"outputBytes": ro.OutputBytes,
		}).Infof("%s >< %s", conn.RemoteAddr(), conn.LocalAddr())
	}()

	if !h.checkRateLimit(conn.RemoteAddr()) {
		return rate_limiter.ErrRateLimit
	}

	if clientID := xctx.ClientIDFromContext(ctx); clientID != "" {
		log = log.WithFields(map[string]any{"user": clientID, "clientID": clientID})
		ro.ClientID = clientID.String()
	}

	address := req.DstAddr.String()
	ro.Host = address

	switch req.Command {
	case constant.Socks5ConnectCmd:
		return h.handleTCP(ctx, conn, "tcp", address, ro, log)
	case constant.Socks5UDPAssociateCmd:
		ro.Network = "udp"
		return h.handleUDP(ctx, conn, ro, log)
	default:
		resp := &model.Response{Reply: constant.Socks5ReplyCommandNotSupported}
		_ = resp.WriteToSocks5(conn)
		return fmt.Errorf("%w: %d", ErrUnknownCmd, req.Command)
	}
}

func (h *mieruHandler) handleTCP(ctx context.Context, conn net.Conn, network, address string, ro *xrecorder.HandlerRecorderObject, log logger.Logger) error {
	log = log.WithFields(map[string]any{
		"dst":  address,
		"cmd":  "tcp",
		"host": address,
	})
	log.Debugf("%s >> %s", conn.RemoteAddr(), address)

	conn, done := h.wrapClientConn(ctx, conn, network, address)
	defer done()

	if h.options.Bypass != nil && h.options.Bypass.Contains(ctx, network, address, bypass.WithService(h.options.Service)) {
		resp := &model.Response{Reply: constant.Socks5ReplyNotAllowedByRuleSet}
		_ = resp.WriteToSocks5(conn)
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
		resp := &model.Response{Reply: constant.Socks5ReplyHostUnreachable}
		_ = resp.WriteToSocks5(conn)
		log.Error(err)
		return err
	}
	defer cc.Close()

	log = log.WithFields(map[string]any{"src": cc.LocalAddr().String(), "dst": cc.RemoteAddr().String()})
	ro.SrcAddr = cc.LocalAddr().String()
	ro.DstAddr = cc.RemoteAddr().String()

	local, ok := cc.LocalAddr().(*net.TCPAddr)
	if !ok {
		err = fmt.Errorf("mieru: upstream local address is not TCP")
		log.Error(err)
		return err
	}
	resp := &model.Response{
		Reply:    constant.Socks5ReplySuccess,
		BindAddr: model.AddrSpec{IP: local.IP, Port: local.Port},
	}
	if err := resp.WriteToSocks5(conn); err != nil {
		log.Error(err)
		return err
	}

	t := time.Now()
	log.Infof("%s <-> %s", conn.RemoteAddr(), address)
	xnet.PipeIdle(ctx, conn, cc, h.md.idleTimeout)
	log.WithFields(map[string]any{"duration": time.Since(t)}).
		Infof("%s >-< %s", conn.RemoteAddr(), address)
	return nil
}

func (h *mieruHandler) handleUDP(ctx context.Context, conn net.Conn, ro *xrecorder.HandlerRecorderObject, log logger.Logger) error {
	log = log.WithFields(map[string]any{"cmd": "udp"})

	if !h.md.enableUDP {
		resp := &model.Response{Reply: constant.Socks5ReplyNotAllowedByRuleSet}
		_ = resp.WriteToSocks5(conn)
		log.Error(ErrUDPDisabled)
		return ErrUDPDisabled
	}

	conn, done := h.wrapClientConn(ctx, conn, "udp", "")
	defer done()

	var buf bytes.Buffer
	cc, err := h.options.Router.Dial(ictx.ContextWithBuffer(ctx, &buf), "udp", "")
	ro.Route = buf.String()
	if err != nil {
		resp := &model.Response{Reply: constant.Socks5ReplyServerFailure}
		_ = resp.WriteToSocks5(conn)
		log.Error(err)
		return err
	}
	defer cc.Close()

	pc, ok := cc.(net.PacketConn)
	if !ok {
		err = errors.New("mieru: router UDP dial did not return PacketConn")
		resp := &model.Response{Reply: constant.Socks5ReplyServerFailure}
		_ = resp.WriteToSocks5(conn)
		log.Error(err)
		return err
	}

	ro.SrcAddr = pc.LocalAddr().String()

	resp := &model.Response{
		Reply:    constant.Socks5ReplySuccess,
		BindAddr: model.AddrSpec{IP: net.IPv4zero, Port: 0},
	}
	if la, ok := pc.LocalAddr().(*net.UDPAddr); ok && la.Port > 0 {
		resp.BindAddr = model.AddrSpec{IP: la.IP, Port: la.Port}
	}
	if err := resp.WriteToSocks5(conn); err != nil {
		log.Error(err)
		return err
	}

	tunnel := apicommon.NewPacketOverStreamTunnel(conn)

	t := time.Now()
	log.Infof("%s <-> %s", conn.RemoteAddr(), pc.LocalAddr())
	err = runUDPAssociateLoop(ctx, tunnel, pc, &net.Resolver{}, udpAssociateOptions{
		service: h.options.Service,
		bypass:  h.options.Bypass,
		logger:  log,
	})
	log.WithFields(map[string]any{"duration": time.Since(t)}).
		Infof("%s >-< %s", conn.RemoteAddr(), pc.LocalAddr())
	if err != nil {
		log.Error(err)
	}
	return err
}

func (h *mieruHandler) wrapClientConn(ctx context.Context, conn net.Conn, network, address string) (net.Conn, func()) {
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

func (h *mieruHandler) Close() error {
	if h.cancel != nil {
		h.cancel()
	}
	return nil
}

func (h *mieruHandler) checkRateLimit(addr net.Addr) bool {
	if h.options.RateLimiter == nil {
		return true
	}
	host, _, _ := net.SplitHostPort(addr.String())
	if limiter := h.options.RateLimiter.Limiter(host); limiter != nil {
		return limiter.Allow(1)
	}
	return true
}

func (h *mieruHandler) observeStats(ctx context.Context) {
	if h.options.Observer == nil {
		return
	}

	ticker := time.NewTicker(h.md.observerPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if evs := h.stats.Events(); len(evs) > 0 {
				h.options.Observer.Observe(ctx, evs)
			}
		case <-ctx.Done():
			return
		}
	}
}
