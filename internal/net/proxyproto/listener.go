package proxyproto

import (
	"bufio"
	"context"
	"errors"
	"net"
	"time"

	"github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/net/accept"
	proxyproto "github.com/pires/go-proxyproto"
)

func prepareConn(setup context.Context, conn net.Conn, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	if limit, ok := setup.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 256)
	header, err := proxyproto.Read(reader)
	var ne net.Error
	// Preserve optional PROXY support for server-first protocols: a peer that
	// sent nothing may receive the server greeting after header detection ends.
	// Partial or malformed headers, however, are never handed to the handler.
	if errors.As(err, &ne) && ne.Timeout() && reader.Buffered() == 0 {
		err = proxyproto.ErrNoProxyProtocol
	}
	if err != nil && !errors.Is(err, proxyproto.ErrNoProxyProtocol) {
		return nil, err
	}
	if err := setup.Err(); err != nil {
		return nil, err
	}
	innerCtx := context.Background()
	if c, ok := conn.(ctx.Context); ok {
		if v := c.Context(); v != nil {
			innerCtx = v
		}
	}

	src, dst := conn.RemoteAddr(), conn.LocalAddr()
	if header != nil && !header.Command.IsLocal() {
		if header.SourceAddr != nil {
			src = header.SourceAddr
		}
		if header.DestinationAddr != nil {
			dst = header.DestinationAddr
		}
	}
	innerCtx = ctx.ContextWithSrcAddr(innerCtx, src)
	innerCtx = ctx.ContextWithDstAddr(innerCtx, dst)
	if reader.Buffered() == 0 {
		reader = nil
	}

	return &serverConn{Conn: conn, ctx: innerCtx, reader: reader}, nil
}

func WrapListener(ppv int, ln net.Listener, readHeaderTimeout time.Duration) net.Listener {
	if ppv <= 0 {
		return ln
	}

	if readHeaderTimeout <= 0 {
		readHeaderTimeout = 10 * time.Second
	}
	return accept.NewListener(ln, accept.Config{
		// The optional-header parser can fall back to plain TCP after its
		// timeout. Also bound the time spent waiting for the Accept consumer.
		Timeout: 2 * readHeaderTimeout,
		Prepare: func(ctx context.Context, conn net.Conn) (net.Conn, error) {
			return prepareConn(ctx, conn, readHeaderTimeout)
		},
	})
}
