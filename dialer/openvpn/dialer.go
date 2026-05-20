package openvpn

import (
	"context"
	"net"

	"github.com/go-gost/core/dialer"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
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

// Dial brings up an OpenVPN client session to the server at addr and
// returns a net.Conn carrying decrypted IP packets. The pushed tunnel
// config travels in the conn's context for the tun handler.
func (d *openvpnDialer) Dial(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
	var options dialer.DialOptions
	for _, opt := range opts {
		opt(&options)
	}

	network := "tcp"
	proto := "tcp"
	if d.md.udp {
		network, proto = "udp", "udp"
	}
	raw, err := options.Dialer.Dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	pio := ovpn.NewStreamPacketIO(raw)
	if d.md.udp {
		pio = ovpn.NewDatagramPacketIO(raw)
	}

	client, err := ovpn.NewClient(&ovpn.ClientConfig{
		Proto:            proto,
		Cipher:           d.md.cipher,
		Auth:             d.md.auth,
		CA:               d.md.ca,
		Cert:             d.md.cert,
		Key:              d.md.key,
		TLSCrypt:         d.md.tlsCrypt,
		Username:         d.md.username,
		Password:         d.md.password,
		HandshakeTimeout: d.md.handshakeTimeout,
	}, pio)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}

	push, err := client.Handshake(ctx)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	d.logger.Debugf("openvpn: connected, assigned %v peer-id %d", push.Prefixes, push.PeerID)

	return ovpn.NewClientConn(client, push, d.md.mtu), nil
}
