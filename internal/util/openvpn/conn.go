package openvpn

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	ictx "github.com/go-gost/x/internal/ctx"
	tun_util "github.com/go-gost/x/internal/util/tun"
	mdx "github.com/go-gost/x/metadata"
)

// endpoint is the post-handshake IP-packet interface shared by the
// client (dialer) and a server-side session (listener).
type endpoint interface {
	ReadIPPacket(ctx context.Context) ([]byte, error)
	WriteIPPacket(ctx context.Context, packet []byte) error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	Close() error
}

// Conn adapts an OpenVPN endpoint into a net.Conn whose Read/Write carry
// raw IP packets, mirroring the wireguard transport. The go-gost tun
// handler consumes it; the tunnel config travels in Context().
type Conn struct {
	ep     endpoint
	ctx    context.Context
	cancel context.CancelFunc

	onClose   func()
	closeOnce sync.Once
}

// SetOnClose registers a callback run once when the conn is closed
// (e.g. to release a pooled address).
func (c *Conn) SetOnClose(fn func()) { c.onClose = fn }

// NewConn wraps an endpoint. cfg is published to the tun handler through
// the connection context.
func NewConn(ep endpoint, cfg *tun_util.Config) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = ictx.ContextWithMetadata(ctx, mdx.NewMetadata(map[string]any{
		"config": cfg,
	}))
	return &Conn{ep: ep, ctx: ctx, cancel: cancel}
}

func (c *Conn) Read(b []byte) (int, error) {
	pkt, err := c.ep.ReadIPPacket(c.ctx)
	if err != nil {
		return 0, err
	}
	return copy(b, pkt), nil
}

func (c *Conn) Write(b []byte) (int, error) {
	if err := c.ep.WriteIPPacket(c.ctx, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.ep.Close()
}

func (c *Conn) Context() context.Context { return c.ctx }

func (c *Conn) LocalAddr() net.Addr  { return c.ep.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.ep.RemoteAddr() }

func (c *Conn) SetDeadline(time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(time.Time) error { return nil }

var _ net.Conn = (*Conn)(nil)

// NewClientConn wraps a handshaked client into an IP-packet net.Conn,
// publishing the pushed tunnel config to the tun handler.
func NewClientConn(client *Client, push *PushReply, mtu int) *Conn {
	return NewConn(client, tunConfigFromPush(push, mtu))
}

// NewServerConn wraps an accepted server session into an IP-packet
// net.Conn for the tun handler. The config carries the client's
// assigned address within the server subnet.
func NewServerConn(sess *ServerSession, subnet netip.Prefix, mtu int) *Conn {
	cfg := &tun_util.Config{MTU: mtu}
	cfg.Net = []net.IPNet{{
		IP:   sess.ClientIP().AsSlice(),
		Mask: net.CIDRMask(subnet.Bits(), subnet.Addr().BitLen()),
	}}
	return NewConn(sess, cfg)
}

// tunConfigFromPush turns a server PUSH_REPLY into a tun handler config.
func tunConfigFromPush(push *PushReply, mtu int) *tun_util.Config {
	cfg := &tun_util.Config{MTU: mtu}
	for _, p := range push.Prefixes {
		cfg.Net = append(cfg.Net, prefixToIPNet(p))
	}
	for _, d := range push.DNS {
		cfg.DNS = append(cfg.DNS, d.AsSlice())
	}
	if push.TunMTU > 0 {
		cfg.MTU = push.TunMTU
	}
	return cfg
}

func prefixToIPNet(p netip.Prefix) net.IPNet {
	return net.IPNet{
		IP:   p.Addr().AsSlice(),
		Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen()),
	}
}
