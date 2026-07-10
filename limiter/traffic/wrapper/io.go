package wrapper

import (
	"context"
	"io"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/limiter/traffic"
)

// readWriter is an io.ReadWriter with traffic limiter supported.
type readWriter struct {
	io.ReadWriter
	limiter traffic.TrafficLimiter
	opts    []limiter.Option
	key     string
}

func WrapReadWriter(limiter traffic.TrafficLimiter, rw io.ReadWriter, key string, opts ...limiter.Option) io.ReadWriter {
	if limiter == nil {
		return rw
	}

	return &readWriter{
		ReadWriter: rw,
		limiter:    limiter,
		opts:       opts,
		key:        key,
	}
}

func (p *readWriter) Read(b []byte) (n int, err error) {
	limiter := p.limiter.In(context.Background(), p.key, p.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return p.ReadWriter.Read(b)
	}

	// Wait before reading so upstream backpressure engages immediately.
	n = limiter.Wait(context.Background(), len(b))
	if n <= 0 {
		return 0, nil
	}
	return p.ReadWriter.Read(b[:n])
}

func (p *readWriter) Write(b []byte) (n int, err error) {
	limiter := p.limiter.Out(context.Background(), p.key, p.opts...)
	if limiter == nil || limiter.Limit() <= 0 {
		return p.ReadWriter.Write(b)
	}

	nn := 0
	for len(b) > 0 {
		nn, err = p.ReadWriter.Write(b[:limiter.Wait(context.Background(), len(b))])
		n += nn
		if err != nil {
			return
		}
		b = b[nn:]
	}

	return
}
