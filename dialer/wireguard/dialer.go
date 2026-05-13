package wireguard

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/go-gost/core/dialer"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	ictx "github.com/go-gost/x/internal/ctx"
	tun_util "github.com/go-gost/x/internal/util/tun"
	wgutil "github.com/go-gost/x/internal/util/wireguard"
	mdx "github.com/go-gost/x/metadata"
	"github.com/go-gost/x/registry"
	wgconn "golang.zx2c4.com/wireguard/conn"
	wgdevice "golang.zx2c4.com/wireguard/device"
)

func init() {
	registry.DialerRegistry().Register("wireguard", NewDialer)
}

type wgDialer struct {
	md      metadata
	logger  logger.Logger
	options dialer.Options
}

func NewDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &wgDialer{
		logger:  options.Logger,
		options: options,
	}
}

func (d *wgDialer) Init(m md.Metadata) error {
	return d.parseMetadata(m)
}

// Dial brings up a wireguard-go client device whose peer endpoint is `addr`
// and returns a net.Conn carrying decrypted IP packets in both directions.
//
// The supplied options.Dialer is intentionally unused: wireguard-go owns its
// own UDP socket via Bind, and `addr` is taken as the peer endpoint, not as a
// stream target.
func (d *wgDialer) Dial(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("wireguard: invalid endpoint %q: %w", addr, err)
	}

	dev := wgutil.NewDevice("wg-client", d.md.mtu, d.md.queueLen)
	wgDev := wgdevice.NewDevice(dev, wgconn.NewDefaultBind(), wgutil.NewLogger(d.logger, d.md.logLevel))

	if err := wgDev.IpcSet(d.buildUAPIConfig(addr)); err != nil {
		wgDev.Close()
		return nil, fmt.Errorf("wireguard: ipc set: %w", err)
	}
	if err := wgDev.Up(); err != nil {
		wgDev.Close()
		return nil, fmt.Errorf("wireguard: device up: %w", err)
	}

	laddr := &wgutil.Addr{Name: "wg-client"}
	raddr := &wgutil.Addr{Name: addr}

	connCtx, cancel := context.WithCancel(ctx)
	connCtx = ictx.ContextWithMetadata(connCtx, mdx.NewMetadata(map[string]any{
		"config": &tun_util.Config{MTU: d.md.mtu},
	}))

	return wgutil.NewConn(dev, wgDev, laddr, raddr, connCtx, cancel, func() {
		wgDev.Close()
	}), nil
}

func (d *wgDialer) buildUAPIConfig(endpoint string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", d.md.privateKey)
	fmt.Fprintf(&b, "replace_peers=true\n")
	fmt.Fprintf(&b, "public_key=%s\n", d.md.peer.publicKey)
	if d.md.peer.presharedKey != "" {
		fmt.Fprintf(&b, "preshared_key=%s\n", d.md.peer.presharedKey)
	}
	fmt.Fprintf(&b, "endpoint=%s\n", endpoint)
	if d.md.peer.persistentKeepalive > 0 {
		fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", d.md.peer.persistentKeepalive)
	}
	fmt.Fprintf(&b, "replace_allowed_ips=true\n")
	for _, ip := range d.md.peer.allowedIPs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", ip)
	}
	return b.String()
}
