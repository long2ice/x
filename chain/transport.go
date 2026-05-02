package chain

import (
	"context"
	"net"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/connector"
	"github.com/go-gost/core/dialer"
	xctx "github.com/go-gost/x/ctx"
	net_dialer "github.com/go-gost/x/internal/net/dialer"
)

type Transport struct {
	dialer    dialer.Dialer
	connector connector.Connector
	options   chain.TransportOptions
}

func NewTransport(d dialer.Dialer, c connector.Connector, opts ...chain.TransportOption) *Transport {
	tr := &Transport{
		dialer:    d,
		connector: c,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&tr.options)
		}
	}

	return tr
}

func (tr *Transport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	netd := &net_dialer.Dialer{
		Interface: tr.options.IfceName,
		Netns:     tr.options.Netns,
	}
	if tr.options.SockOpts != nil {
		netd.Mark = tr.options.SockOpts.Mark
	}
	if tr.options.Route != nil && len(tr.options.Route.Nodes()) > 0 {
		netd.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tr.options.Route.Dial(ctx, network, addr)
		}
	}
	if id := routeIDFromTransport(tr); id != "" {
		ctx = xctx.ContextWithRouteID(ctx, id)
	}
	opts := []dialer.DialOption{
		dialer.HostDialOption(tr.options.Addr),
		dialer.NetDialerDialOption(netd),
	}
	return tr.dialer.Dial(ctx, addr, opts...)
}

// routeIDFromTransport extracts the owning chain's name from the Transport's
// Route, so multiplexing dialers can disambiguate sessions per-chain.
func routeIDFromTransport(tr *Transport) string {
	if tr == nil || tr.options.Route == nil {
		return ""
	}
	cr, ok := tr.options.Route.(*chainRoute)
	if !ok || cr == nil || cr.options.Chain == nil {
		return ""
	}
	if cn, ok := cr.options.Chain.(chainNamer); ok && cn != nil {
		return cn.Name()
	}
	return ""
}

func (tr *Transport) Handshake(ctx context.Context, conn net.Conn) (net.Conn, error) {
	if id := routeIDFromTransport(tr); id != "" {
		ctx = xctx.ContextWithRouteID(ctx, id)
	}
	var err error
	if hs, ok := tr.dialer.(dialer.Handshaker); ok {
		conn, err = hs.Handshake(ctx, conn,
			dialer.AddrHandshakeOption(tr.options.Addr))
		if err != nil {
			return nil, err
		}
	}
	if hs, ok := tr.connector.(connector.Handshaker); ok {
		return hs.Handshake(ctx, conn)
	}
	return conn, nil
}

func (tr *Transport) Connect(ctx context.Context, conn net.Conn, network, address string) (net.Conn, error) {
	netd := &net_dialer.Dialer{
		Interface: tr.options.IfceName,
		Netns:     tr.options.Netns,
	}
	if tr.options.SockOpts != nil {
		netd.Mark = tr.options.SockOpts.Mark
	}
	// Route any auxiliary dial the connector performs (e.g. SOCKS5 UDP
	// ASSOCIATE relay address) through the preceding chain, so it doesn't
	// bypass the tunnel and go direct over the public internet.
	if tr.options.Route != nil && len(tr.options.Route.Nodes()) > 0 {
		netd.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tr.options.Route.Dial(ctx, network, addr)
		}
	}
	return tr.connector.Connect(ctx, conn, network, address,
		connector.DialerConnectOption(netd),
	)
}

func (tr *Transport) Bind(ctx context.Context, conn net.Conn, network, address string, opts ...connector.BindOption) (net.Listener, error) {
	if binder, ok := tr.connector.(connector.Binder); ok {
		return binder.Bind(ctx, conn, network, address, opts...)
	}
	return nil, connector.ErrBindUnsupported
}

func (tr *Transport) Multiplex() bool {
	if mux, ok := tr.dialer.(dialer.Multiplexer); ok {
		return mux.Multiplex()
	}
	return false
}

func (tr *Transport) Options() *chain.TransportOptions {
	if tr != nil {
		return &tr.options
	}
	return nil
}

func (tr *Transport) Copy() chain.Transporter {
	tr2 := &Transport{}
	*tr2 = *tr
	return tr2
}
