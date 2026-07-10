package wrapper

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/limiter/traffic"
	"github.com/go-gost/x/ctx"
	xio "github.com/go-gost/x/internal/io"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/net/udp"
)

var (
	errUnsupport = errors.New("unsupported operation")
)

// waitAll blocks until lim grants all n bytes. Needed for datagrams that
// cannot be fragmented: each Wait may return at most one burst.
func waitAll(ctx context.Context, lim traffic.Limiter, n int) bool {
	for n > 0 {
		v := lim.Wait(ctx, n)
		if v <= 0 {
			return false
		}
		n -= v
	}
	return true
}

// limitConn is a Conn with traffic limiter supported.
type limitConn struct {
	net.Conn
	limiter traffic.TrafficLimiter
	opts    []limiter.Option
	key     string
}

func WrapConn(c net.Conn, tlimiter traffic.TrafficLimiter, key string, opts ...limiter.Option) net.Conn {
	if tlimiter == nil {
		return c
	}

	return &limitConn{
		Conn:    c,
		limiter: tlimiter,
		opts:    opts,
		key:     key,
	}
}

func (c *limitConn) Read(b []byte) (n int, err error) {
	limiter := c.limiter.In(context.Background(), c.key, c.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return c.Conn.Read(b)
	}

	// Wait before reading so the kernel socket buffer backs up and TCP
	// windowing throttles the sender — no post-read burst into userspace.
	n = limiter.Wait(context.Background(), len(b))
	if n <= 0 {
		return 0, nil
	}
	return c.Conn.Read(b[:n])
}

func (c *limitConn) Write(b []byte) (n int, err error) {
	limiter := c.limiter.Out(context.Background(), c.key, c.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return c.Conn.Write(b)
	}

	nn := 0
	for len(b) > 0 {
		nn, err = c.Conn.Write(b[:limiter.Wait(context.Background(), len(b))])
		n += nn
		if err != nil {
			return
		}
		b = b[nn:]
	}

	return
}

func (c *limitConn) SyscallConn() (rc syscall.RawConn, err error) {
	if sc, ok := c.Conn.(syscall.Conn); ok {
		rc, err = sc.SyscallConn()
		return
	}
	err = errUnsupport
	return
}

func (c *limitConn) Context() context.Context {
	if innerCtx, ok := c.Conn.(ctx.Context); ok {
		return innerCtx.Context()
	}
	return nil
}

func (c *limitConn) CloseRead() error {
	if sc, ok := c.Conn.(xio.CloseRead); ok {
		return sc.CloseRead()
	}
	return xio.ErrUnsupported
}

func (c *limitConn) CloseWrite() error {
	if sc, ok := c.Conn.(xio.CloseWrite); ok {
		return sc.CloseWrite()
	}
	return xio.ErrUnsupported
}

type packetConn struct {
	net.PacketConn
	limiter traffic.TrafficLimiter
	opts    []limiter.Option
	key     string
}

func WrapPacketConn(pc net.PacketConn, lim traffic.TrafficLimiter, key string, opts ...limiter.Option) net.PacketConn {
	if lim == nil {
		return pc
	}
	return &packetConn{
		PacketConn: pc,
		limiter:    lim,
		opts:       opts,
		key:        key,
	}
}

func (c *packetConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, addr, err = c.PacketConn.ReadFrom(p)
	if err != nil {
		return
	}

	limiter := c.limiter.In(context.Background(), c.key, c.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return
	}

	// Pace the full datagram; do not discard (burst is intentionally tiny).
	if !waitAll(context.Background(), limiter, n) {
		return 0, addr, context.Canceled
	}
	return
}

func (c *packetConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	limiter := c.limiter.Out(context.Background(), c.key, c.opts...)
	if limiter != nil && limiter.Limit() > 0 {
		if !waitAll(context.Background(), limiter, len(p)) {
			return 0, context.Canceled
		}
	}

	return c.PacketConn.WriteTo(p, addr)
}

func (c *packetConn) Context() context.Context {
	if innerCtx, ok := c.PacketConn.(ctx.Context); ok {
		return innerCtx.Context()
	}
	return nil
}

type udpConn struct {
	net.PacketConn
	limiter traffic.TrafficLimiter
	opts    []limiter.Option
	key     string
}

func WrapUDPConn(pc net.PacketConn, limiter traffic.TrafficLimiter, key string, opts ...limiter.Option) udp.Conn {
	return &udpConn{
		PacketConn: pc,
		limiter:    limiter,
		opts:       opts,
		key:        key,
	}
}

func (c *udpConn) RemoteAddr() net.Addr {
	if nc, ok := c.PacketConn.(xnet.RemoteAddr); ok {
		return nc.RemoteAddr()
	}
	return nil
}

func (c *udpConn) SetReadBuffer(n int) error {
	if nc, ok := c.PacketConn.(xnet.SetBuffer); ok {
		return nc.SetReadBuffer(n)
	}
	return errUnsupport
}

func (c *udpConn) SetWriteBuffer(n int) error {
	if nc, ok := c.PacketConn.(xnet.SetBuffer); ok {
		return nc.SetWriteBuffer(n)
	}
	return errUnsupport
}

func (c *udpConn) Read(b []byte) (n int, err error) {
	nc, ok := c.PacketConn.(io.Reader)
	if !ok {
		err = errUnsupport
		return
	}

	n, err = nc.Read(b)
	if err != nil {
		return
	}

	if c.limiter == nil {
		return
	}

	limiter := c.limiter.In(context.Background(), c.key, c.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return
	}

	if !waitAll(context.Background(), limiter, n) {
		return 0, context.Canceled
	}
	return
}

func (c *udpConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	n, addr, err = c.PacketConn.ReadFrom(p)
	if err != nil {
		return
	}

	if c.limiter == nil {
		return
	}

	limiter := c.limiter.In(context.Background(), c.key, c.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return
	}

	if !waitAll(context.Background(), limiter, n) {
		return 0, addr, context.Canceled
	}
	return
}

func (c *udpConn) ReadFromUDP(b []byte) (n int, addr *net.UDPAddr, err error) {
	nc, ok := c.PacketConn.(udp.ReadUDP)
	if !ok {
		err = errUnsupport
		return
	}

	n, addr, err = nc.ReadFromUDP(b)
	if err != nil {
		return
	}

	if c.limiter == nil {
		return
	}

	limiter := c.limiter.In(context.Background(), c.key, c.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return
	}

	if !waitAll(context.Background(), limiter, n) {
		return 0, addr, context.Canceled
	}
	return
}

func (c *udpConn) ReadMsgUDP(b, oob []byte) (n, oobn, flags int, addr *net.UDPAddr, err error) {
	nc, ok := c.PacketConn.(udp.ReadUDP)
	if !ok {
		err = errUnsupport
		return
	}

	n, oobn, flags, addr, err = nc.ReadMsgUDP(b, oob)
	if err != nil {
		return
	}

	if c.limiter == nil {
		return
	}

	limiter := c.limiter.In(context.Background(), c.key, c.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return
	}

	if !waitAll(context.Background(), limiter, n) {
		return 0, 0, 0, addr, context.Canceled
	}
	return
}

func (c *udpConn) Write(p []byte) (n int, err error) {
	nc, ok := c.PacketConn.(io.Writer)
	if !ok {
		err = errUnsupport
		return
	}

	if c.limiter != nil {
		limiter := c.limiter.Out(context.Background(), c.key, c.opts...)
		if limiter != nil && limiter.Limit() > 0 {
			if !waitAll(context.Background(), limiter, len(p)) {
				return 0, context.Canceled
			}
		}
	}

	n, err = nc.Write(p)
	return
}

func (c *udpConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if c.limiter != nil {
		limiter := c.limiter.Out(context.Background(), c.key, c.opts...)
		if limiter != nil && limiter.Limit() > 0 {
			if !waitAll(context.Background(), limiter, len(p)) {
				return 0, context.Canceled
			}
		}
	}

	n, err = c.PacketConn.WriteTo(p, addr)
	return
}

func (c *udpConn) WriteToUDP(p []byte, addr *net.UDPAddr) (n int, err error) {
	nc, ok := c.PacketConn.(udp.WriteUDP)
	if !ok {
		err = errUnsupport
		return
	}

	if c.limiter != nil {
		limiter := c.limiter.Out(context.Background(), c.key, c.opts...)
		if limiter != nil && limiter.Limit() > 0 {
			if !waitAll(context.Background(), limiter, len(p)) {
				return 0, context.Canceled
			}
		}
	}

	n, err = nc.WriteToUDP(p, addr)
	return
}

func (c *udpConn) WriteMsgUDP(p, oob []byte, addr *net.UDPAddr) (n, oobn int, err error) {
	nc, ok := c.PacketConn.(udp.WriteUDP)
	if !ok {
		err = errUnsupport
		return
	}

	if c.limiter != nil {
		limiter := c.limiter.Out(context.Background(), c.key, c.opts...)
		if limiter != nil && limiter.Limit() > 0 {
			if !waitAll(context.Background(), limiter, len(p)) {
				return 0, 0, context.Canceled
			}
		}
	}

	n, oobn, err = nc.WriteMsgUDP(p, oob, addr)
	return
}

func (c *udpConn) SyscallConn() (rc syscall.RawConn, err error) {
	if nc, ok := c.PacketConn.(xnet.SyscallConn); ok {
		return nc.SyscallConn()
	}
	err = errUnsupport
	return
}

func (c *udpConn) SetDSCP(n int) error {
	if nc, ok := c.PacketConn.(xnet.SetDSCP); ok {
		return nc.SetDSCP(n)
	}
	return nil
}

func (c *udpConn) Context() context.Context {
	if innerCtx, ok := c.PacketConn.(ctx.Context); ok {
		return innerCtx.Context()
	}
	return nil
}
