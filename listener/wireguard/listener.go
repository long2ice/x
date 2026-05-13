package wireguard

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	mdata "github.com/go-gost/core/metadata"
	ictx "github.com/go-gost/x/internal/ctx"
	tun_util "github.com/go-gost/x/internal/util/tun"
	wgutil "github.com/go-gost/x/internal/util/wireguard"
	traffic_limiter "github.com/go-gost/x/limiter/traffic"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	mdx "github.com/go-gost/x/metadata"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
	wgconn "golang.zx2c4.com/wireguard/conn"
	wgdevice "golang.zx2c4.com/wireguard/device"
)

func init() {
	registry.ListenerRegistry().Register("wireguard", NewListener)
}

const (
	bindRetries  = 15
	bindInterval = time.Second
)

type wgListener struct {
	addr    net.Addr
	port    uint16
	cqueue  chan net.Conn
	closed  chan struct{}
	log     logger.Logger
	md      metadata
	options listener.Options

	dev      *wgutil.Device
	wgDev    *wgdevice.Device
	closeWG  sync.Once
	stopOnce sync.Once
}

func NewListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &wgListener{
		log:     options.Logger,
		options: options,
	}
}

func (l *wgListener) Init(md mdata.Metadata) error {
	if err := l.parseMetadata(md); err != nil {
		return err
	}

	port, err := parseListenPort(l.options.Addr)
	if err != nil {
		return err
	}
	l.port = port
	l.addr = &wgutil.Addr{Name: l.options.Addr}

	l.cqueue = make(chan net.Conn, 1)
	l.closed = make(chan struct{})

	dev, wgDev, err := l.createDeviceWithRetry()
	if err != nil {
		return err
	}
	l.dev = dev
	l.wgDev = wgDev

	ctx, cancel := context.WithCancel(context.Background())
	ctx = ictx.ContextWithMetadata(ctx, mdx.NewMetadata(map[string]any{
		"config": &tun_util.Config{MTU: l.md.mtu},
	}))

	var c net.Conn = wgutil.NewConn(dev, wgDev, l.addr, l.addr, ctx, cancel, l.shutdownDevice)
	c = metrics.WrapConn(l.options.Service, c)
	c = stats.WrapConn(c, l.options.Stats)
	c = limiter_wrapper.WrapConn(
		c,
		l.options.TrafficLimiter,
		traffic_limiter.ServiceLimitKey,
		limiter.ScopeOption(limiter.ScopeService),
		limiter.ServiceOption(l.options.Service),
		limiter.NetworkOption(c.LocalAddr().Network()),
	)

	l.cqueue <- c
	return nil
}

func (l *wgListener) createDeviceWithRetry() (*wgutil.Device, *wgdevice.Device, error) {
	var lastErr error
	for i := 0; i < bindRetries; i++ {
		select {
		case <-l.closed:
			return nil, nil, listener.ErrClosed
		default:
		}

		dev, wgDev, err := l.createDevice()
		if err == nil {
			return dev, wgDev, nil
		}
		lastErr = err
		if !isAddrInUse(err) {
			return nil, nil, err
		}
		l.log.Warnf("wireguard: udp/%d in use, retry %d/%d", l.port, i+1, bindRetries)
		select {
		case <-time.After(bindInterval):
		case <-l.closed:
			return nil, nil, listener.ErrClosed
		}
	}
	return nil, nil, lastErr
}

func (l *wgListener) createDevice() (*wgutil.Device, *wgdevice.Device, error) {
	dev := wgutil.NewDevice("wg0", l.md.mtu, l.md.queueLen)
	wgDev := wgdevice.NewDevice(dev, wgconn.NewDefaultBind(), wgutil.NewLogger(l.log, l.md.logLevel))

	if err := wgDev.IpcSet(l.buildUAPIConfig()); err != nil {
		wgDev.Close()
		return nil, nil, fmt.Errorf("ipc set: %w", err)
	}
	if err := wgDev.Up(); err != nil {
		wgDev.Close()
		return nil, nil, fmt.Errorf("device up: %w", err)
	}

	l.log.Infof("wireguard listening on udp/%d, peers=%d, mtu=%d",
		l.port, len(l.md.peers), l.md.mtu)
	return dev, wgDev, nil
}

func (l *wgListener) shutdownDevice() {
	l.closeWG.Do(func() {
		if l.wgDev != nil {
			l.wgDev.Close()
		}
	})
}

func (l *wgListener) buildUAPIConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", l.md.privateKey)
	fmt.Fprintf(&b, "listen_port=%d\n", l.port)
	fmt.Fprintf(&b, "replace_peers=true\n")
	for _, p := range l.md.peers {
		fmt.Fprintf(&b, "public_key=%s\n", p.publicKey)
		if p.presharedKey != "" {
			fmt.Fprintf(&b, "preshared_key=%s\n", p.presharedKey)
		}
		if p.endpoint != "" {
			fmt.Fprintf(&b, "endpoint=%s\n", p.endpoint)
		}
		if p.persistentKeepalive > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.persistentKeepalive)
		}
		fmt.Fprintf(&b, "replace_allowed_ips=true\n")
		for _, ip := range p.allowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", ip)
		}
	}
	return b.String()
}

func (l *wgListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.cqueue:
		return c, nil
	case <-l.closed:
	}
	return nil, listener.ErrClosed
}

func (l *wgListener) Addr() net.Addr { return l.addr }

func (l *wgListener) Close() error {
	var firstErr error
	l.stopOnce.Do(func() {
		close(l.closed)
		l.shutdownDevice()
	})
	if firstErr == nil {
		select {
		case <-l.closed:
		default:
			firstErr = net.ErrClosed
		}
	}
	return nil
}

func parseListenPort(s string) (uint16, error) {
	_, p, err := net.SplitHostPort(s)
	if err != nil {
		return 0, fmt.Errorf("wireguard: invalid listen addr %q: %w", s, err)
	}
	port, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("wireguard: invalid listen port %q: %w", p, err)
	}
	return uint16(port), nil
}

func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "address already in use")
}
