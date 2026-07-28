package vless

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/limiter/traffic"
	md "github.com/go-gost/core/metadata"
	"github.com/go-gost/core/observer/stats"
	"github.com/go-gost/core/recorder"
	xctx "github.com/go-gost/x/ctx"
	stats_util "github.com/go-gost/x/internal/util/stats"
	tls_util "github.com/go-gost/x/internal/util/tls"
	"github.com/go-gost/x/internal/util/vision"
	xvless "github.com/go-gost/x/internal/util/vless"
	rate_limiter "github.com/go-gost/x/limiter/rate"
	cache_limiter "github.com/go-gost/x/limiter/traffic/cache"
	xstats "github.com/go-gost/x/observer/stats"
	stats_wrapper "github.com/go-gost/x/observer/stats/wrapper"
	xrecorder "github.com/go-gost/x/recorder"
	"github.com/go-gost/x/registry"
)

var (
	ErrNoUser       = errors.New("vless: no user")
	ErrUnauthorized = errors.New("vless: unauthorized")
	ErrUnknownCmd   = errors.New("vless: unknown command")
	ErrUDPDisabled  = errors.New("vless: UDP is disabled")
)

func init() {
	registry.HandlerRegistry().Register("vless", NewHandler)
}

type vlessHandler struct {
	users    map[xvless.UUID]string
	md       metadata
	options  handler.Options
	stats    *stats_util.HandlerStats
	limiter  traffic.TrafficLimiter
	cancel   context.CancelFunc
	recorder recorder.RecorderObject
	certPool tls_util.CertPool
}

func NewHandler(opts ...handler.Option) handler.Handler {
	options := handler.Options{}
	for _, opt := range opts {
		opt(&options)
	}

	return &vlessHandler{
		options: options,
	}
}

func (h *vlessHandler) Init(md md.Metadata) (err error) {
	if err = h.parseMetadata(md); err != nil {
		return
	}

	if h.users, err = h.parseUsers(); err != nil {
		return
	}
	if len(h.users) == 0 {
		return ErrNoUser
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

	if h.md.certificate != nil && h.md.privateKey != nil {
		h.certPool = tls_util.NewMemoryCertPool()
	}

	return
}

// parseUsers maps every configured user ID to its name. Users come from the
// users metadata, and from the handler auth where the username is the user
// name and the password its ID.
func (h *vlessHandler) parseUsers() (map[xvless.UUID]string, error) {
	users := make(map[xvless.UUID]string)

	add := func(name, id string) error {
		uuid, err := xvless.ParseUUID(id)
		if err != nil {
			return err
		}
		users[uuid] = name
		return nil
	}

	for name, id := range h.md.users {
		if err := add(name, id); err != nil {
			return nil, err
		}
	}

	if h.options.Auth != nil {
		name := h.options.Auth.Username()
		id, _ := h.options.Auth.Password()
		if id == "" {
			name, id = "", name
		}
		if id != "" {
			if err := add(name, id); err != nil {
				return nil, err
			}
		}
	}

	return users, nil
}

func (h *vlessHandler) Handle(ctx context.Context, conn net.Conn, opts ...handler.HandleOption) (err error) {
	defer conn.Close()

	start := time.Now()

	// XTLS Vision needs the transport below the TLS layer, which only the
	// listener connection carries, so take it before wrapping anything.
	raw, _ := conn.(vision.RawConner)
	var vc *vision.Conn

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

		fields := map[string]any{
			"network":     ro.Network,
			"duration":    time.Since(start),
			"inputBytes":  ro.InputBytes,
			"outputBytes": ro.OutputBytes,
		}
		if vc != nil {
			read, write := vc.DirectCopy()
			fields["visionDirect"] = fmt.Sprintf("%v/%v", read, write)
		}
		log.WithFields(fields).Infof("%s >< %s", conn.RemoteAddr(), conn.LocalAddr())
	}()

	if !h.checkRateLimit(conn.RemoteAddr()) {
		return rate_limiter.ErrRateLimit
	}

	conn.SetReadDeadline(time.Now().Add(h.md.readTimeout))
	req, err := xvless.ReadRequest(conn)
	if err != nil {
		log.Error(err)
		return err
	}
	conn.SetReadDeadline(time.Time{})

	clientID, ok := h.users[req.ID]
	if !ok {
		log.Errorf("%s: %s", ErrUnauthorized, req.ID)
		return ErrUnauthorized
	}
	if clientID != "" {
		ctx = xctx.ContextWithClientID(ctx, xctx.ClientID(clientID))
		log = log.WithFields(map[string]any{"user": clientID, "clientID": clientID})
		ro.ClientID = clientID
	}

	// The response header is sent along with the first response data.
	conn = xvless.ServerConn(conn)

	switch req.Flow {
	case "":
	case xvless.FlowVision:
		if raw == nil {
			err = fmt.Errorf("vless: flow %s needs the reality listener", req.Flow)
			log.Error(err)
			return err
		}
		vc = vision.NewConn(conn, req.ID, &statsRawConner{RawConner: raw, stats: &pStats}, h.md.visionDirect)
		conn = vc
	default:
		err = fmt.Errorf("vless: unsupported flow %s", req.Flow)
		log.Error(err)
		return err
	}

	address := req.Addr()
	ro.Host = address

	switch req.Command {
	case xvless.CmdTCP:
		return h.handleTCP(ctx, conn, "tcp", address, ro, log)
	case xvless.CmdUDP:
		ro.Network = "udp"
		return h.handleUDP(ctx, conn, "udp", address, ro, log)
	case xvless.CmdMux:
		if !xvless.IsMuxAddr(req.Host, req.Port) {
			err = fmt.Errorf("vless: mux is only supported for UDP (%s)", address)
			log.Error(err)
			return err
		}
		ro.Network = "udp"
		return h.handleXUDP(ctx, conn, "udp", ro, log)
	default:
		err = fmt.Errorf("%w: %d", ErrUnknownCmd, req.Command)
		log.Error(err)
		return err
	}
}

// statsRawConner keeps the traffic of a connection that switched to direct
// copy in the byte counts of the recorder.
type statsRawConner struct {
	vision.RawConner
	stats *xstats.Stats
}

func (c *statsRawConner) RawConn() net.Conn {
	return stats_wrapper.WrapConn(c.RawConner.RawConn(), c.stats)
}

func (h *vlessHandler) Close() error {
	if h.cancel != nil {
		h.cancel()
	}
	return nil
}

func (h *vlessHandler) checkRateLimit(addr net.Addr) bool {
	if h.options.RateLimiter == nil {
		return true
	}
	host, _, _ := net.SplitHostPort(addr.String())
	if limiter := h.options.RateLimiter.Limiter(host); limiter != nil {
		return limiter.Allow(1)
	}

	return true
}

func (h *vlessHandler) observeStats(ctx context.Context) {
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
