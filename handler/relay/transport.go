package relay

import (
	"context"
	"net"

	xnet "github.com/go-gost/x/internal/net"
)

func (h *relayHandler) transport(ctx context.Context, client, target net.Conn, network string) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return xnet.PipeIdleBuffer(ctx, client, target, h.md.idleTimeout, h.md.bufferSize)
	default:
		// Keep datagram framing and buffer sizes unchanged for UDP-over-relay.
		return xnet.Pipe(ctx, client, target)
	}
}
