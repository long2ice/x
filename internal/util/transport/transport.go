package transport

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/go-gost/core/metadata"
	xnet "github.com/go-gost/x/internal/net"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	DefaultIdleTimeout = 5 * time.Minute
	DefaultBufferSize  = 16 * 1024
)

type Options struct {
	IdleTimeout time.Duration
	BufferSize  int
}

func Parse(md metadata.Metadata, defaultIdle time.Duration, idleKeys ...string) (Options, error) {
	options := Options{IdleTimeout: defaultIdle, BufferSize: DefaultBufferSize}
	if len(idleKeys) == 0 {
		idleKeys = []string{"idleTimeout"}
	}
	for _, key := range idleKeys {
		if md != nil && md.IsExists(key) {
			options.IdleTimeout = mdutil.GetDuration(md, key)
			if options.IdleTimeout < 0 {
				return Options{}, fmt.Errorf("%s must not be negative", key)
			}
			break
		}
	}
	if size := mdutil.GetInt(md, "tcp.bufferSize", "bufferSize"); size > 0 {
		options.BufferSize = size
	}
	return options, nil
}

func (o Options) Pipe(ctx context.Context, client, target io.ReadWriteCloser, network string) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return xnet.PipeIdleBuffer(ctx, client, target, o.IdleTimeout, o.BufferSize)
	default:
		// Datagram framing and buffer sizes follow their protocol's own policy.
		return xnet.Pipe(ctx, client, target)
	}
}
