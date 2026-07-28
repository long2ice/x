package vless

import (
	"bytes"
	"context"
	"errors"
	"net"
	"time"

	"github.com/go-gost/core/logger"
	ictx "github.com/go-gost/x/internal/ctx"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/net/udp"
	xvless "github.com/go-gost/x/internal/util/vless"
	xrecorder "github.com/go-gost/x/recorder"
)

func (h *vlessHandler) handleUDP(ctx context.Context, conn net.Conn, network, address string, ro *xrecorder.HandlerRecorderObject, log logger.Logger) error {
	log = log.WithFields(map[string]any{
		"dst":  address,
		"cmd":  "udp",
		"host": address,
	})
	log.Debugf("%s >> %s", conn.RemoteAddr(), address)

	if !h.md.enableUDP {
		log.Error(ErrUDPDisabled)
		return ErrUDPDisabled
	}

	conn, done := h.wrapClientConn(ctx, conn, network, address)
	defer done()

	// The target of every packet is the address carried by the request header,
	// resolve it once so that the relay can write to an unconnected socket.
	raddr, err := h.resolveUDPAddr(ctx, address, log)
	if err != nil {
		log.Error(err)
		return err
	}

	var buf bytes.Buffer
	c, err := h.options.Router.Dial(ictx.ContextWithBuffer(ctx, &buf), network, "") // UDP association
	ro.Route = buf.String()
	if err != nil {
		log.Error(err)
		return err
	}
	defer c.Close()

	pc, ok := c.(net.PacketConn)
	if !ok {
		err := errors.New("vless: wrong connection type")
		log.Error(err)
		return err
	}

	log = log.WithFields(map[string]any{"src": pc.LocalAddr().String()})
	ro.SrcAddr = pc.LocalAddr().String()
	ro.DstAddr = raddr.String()

	r := udp.NewRelay(xvless.PacketConn(conn, raddr), pc).
		WithService(h.options.Service).
		WithBypass(h.options.Bypass).
		WithBufferSize(h.md.udpBufferSize).
		WithLogger(log)

	t := time.Now()
	log.Infof("%s <-> %s", conn.RemoteAddr(), address)
	r.Run(ctx)
	log.WithFields(map[string]any{
		"duration": time.Since(t),
	}).Infof("%s >-< %s", conn.RemoteAddr(), address)

	return nil
}

// handleXUDP relays the UDP packets of an XUDP stream, which unlike a plain
// VLESS UDP stream carries a destination per packet.
func (h *vlessHandler) handleXUDP(ctx context.Context, conn net.Conn, network string, ro *xrecorder.HandlerRecorderObject, log logger.Logger) error {
	log = log.WithFields(map[string]any{
		"cmd": "xudp",
	})

	if !h.md.enableUDP {
		log.Error(ErrUDPDisabled)
		return ErrUDPDisabled
	}

	conn, done := h.wrapClientConn(ctx, conn, network, "")
	defer done()

	var buf bytes.Buffer
	c, err := h.options.Router.Dial(ictx.ContextWithBuffer(ctx, &buf), network, "") // UDP association
	ro.Route = buf.String()
	if err != nil {
		log.Error(err)
		return err
	}
	defer c.Close()

	pc, ok := c.(net.PacketConn)
	if !ok {
		err := errors.New("vless: wrong connection type")
		log.Error(err)
		return err
	}

	log = log.WithFields(map[string]any{"src": pc.LocalAddr().String()})
	ro.SrcAddr = pc.LocalAddr().String()

	resolve := func(address string) (*net.UDPAddr, error) {
		return h.resolveUDPAddr(ctx, address, log)
	}

	r := udp.NewRelay(xvless.XUDPConn(conn, resolve), pc).
		WithService(h.options.Service).
		WithBypass(h.options.Bypass).
		WithBufferSize(h.md.udpBufferSize).
		WithLogger(log)

	t := time.Now()
	log.Infof("%s <-> %s", conn.RemoteAddr(), pc.LocalAddr())
	r.Run(ctx)
	log.WithFields(map[string]any{
		"duration": time.Since(t),
	}).Infof("%s >-< %s", conn.RemoteAddr(), pc.LocalAddr())

	return nil
}

// resolveUDPAddr resolves address with the router resolver, falling back to
// the system resolver.
func (h *vlessHandler) resolveUDPAddr(ctx context.Context, address string, log logger.Logger) (*net.UDPAddr, error) {
	addr := address
	if opts := h.options.Router.Options(); opts != nil {
		var err error
		if addr, err = xnet.Resolve(ctx, "ip", address, opts.Resolver, opts.HostMapper, log); err != nil {
			return nil, err
		}
	}
	return net.ResolveUDPAddr("udp", addr)
}
