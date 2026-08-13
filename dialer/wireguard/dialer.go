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
	net_dialer "github.com/go-gost/x/internal/net/dialer"
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
// `addr` is taken as the peer endpoint, not as a stream target: wireguard-go
// owns its own UDP socket via Bind. The socket still honors the node's
// NetDialer options (interface, so_mark, netns) — see transportBind — so the
// encrypted transport escapes rule-based routing instead of being looped back
// into the tunnel it carries.
func (d *wgDialer) Dial(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("wireguard: invalid endpoint %q: %w", addr, err)
	}

	var options dialer.DialOptions
	for _, opt := range opts {
		opt(&options)
	}

	dev := wgutil.NewDevice("wg-client", d.md.mtu, d.md.queueLen)
	bind, fwmark := d.transportBind(ctx, &options)
	wgDev := wgdevice.NewDevice(dev, bind, wgutil.NewLogger(d.logger, d.md.logLevel))

	if err := wgDev.IpcSet(d.buildUAPIConfig(addr, fwmark)); err != nil {
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

// transportBind picks the UDP bind for the encrypted transport. When the
// chain's NetDialer carries socket options that must be applied at socket
// creation (interface binding, netns), the socket is created through it via
// DialBind. A mark alone is handed to the default bind through the fwmark
// UAPI key instead, which keeps wgconn's batched (GSO/GRO) I/O path.
func (d *wgDialer) transportBind(ctx context.Context, options *dialer.DialOptions) (wgconn.Bind, int) {
	nd, ok := options.Dialer.(*net_dialer.Dialer)
	if !ok || nd == nil || nd.DialFunc != nil {
		return wgconn.NewDefaultBind(), 0
	}
	if nd.Interface == "" && nd.Netns == "" {
		return wgconn.NewDefaultBind(), nd.Mark
	}
	// Copy so the fallback logger doesn't leak into the caller's dialer;
	// logger.Default() may be nil before the app installs one.
	ndCopy := *nd
	if ndCopy.Log == nil {
		ndCopy.Log = d.logger
	}
	return wgutil.NewDialBind(func() (*net.UDPConn, error) {
		conn, err := ndCopy.Dial(ctx, "udp", "")
		if err != nil {
			return nil, err
		}
		uc, ok := conn.(*net.UDPConn)
		if !ok {
			conn.Close()
			return nil, fmt.Errorf("wireguard: unexpected transport conn type %T", conn)
		}
		return uc, nil
	}), 0
}

func (d *wgDialer) buildUAPIConfig(endpoint string, fwmark int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", d.md.privateKey)
	if fwmark > 0 {
		fmt.Fprintf(&b, "fwmark=%d\n", fwmark)
	}
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
