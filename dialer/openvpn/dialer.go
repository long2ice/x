package openvpn

import (
	"context"
	"net"
	"time"

	"github.com/go-gost/core/dialer"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/net/proxyproto"
	ovpn "github.com/go-gost/x/internal/util/openvpn"
	"github.com/go-gost/x/registry"
)

func init() {
	registry.DialerRegistry().Register("openvpn", NewDialer)
}

type openvpnDialer struct {
	md      metadata
	logger  logger.Logger
	options dialer.Options
}

func NewDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &openvpnDialer{
		logger:  options.Logger,
		options: options,
	}
}

func (d *openvpnDialer) Init(m md.Metadata) error {
	return d.parseMetadata(m)
}

func (d *openvpnDialer) Dial(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
	var options dialer.DialOptions
	for _, opt := range opts {
		opt(&options)
	}

	network := "tcp"
	if d.md.udp {
		network = "udp"
	}
	raw, err := options.Dialer.Dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if !d.md.udp {
		raw = proxyproto.WrapClientConn(
			d.options.ProxyProtocol,
			xctx.SrcAddrFromContext(ctx),
			xctx.DstAddrFromContext(ctx),
			raw,
		)
	}

	if d.md.handshakeTimeout > 0 {
		_ = raw.SetDeadline(time.Now().Add(d.md.handshakeTimeout))
	}
	var tunnel *ovpn.Tunnel
	if d.md.udp {
		tunnel, err = ovpn.ClientHandshakePacket(raw, d.md.key)
	} else {
		tunnel, err = ovpn.ClientHandshake(raw, d.md.key)
	}
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return tunnel, nil
}
