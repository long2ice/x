package reality

import (
	"bytes"
	"io"
	"net"
	"reflect"
	"sync"
	"unsafe"

	"github.com/go-gost/core/limiter"
	xio "github.com/go-gost/x/internal/io"
	climiter "github.com/go-gost/x/limiter/conn/wrapper"
	traffic_limiter "github.com/go-gost/x/limiter/traffic"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/xtls/reality"
)

// wrapListener applies the metrics, stats and limiter wrappers underneath
// REALITY instead of on top of it, so that they keep counting once XTLS
// Vision takes the transport away from the TLS layer. The bytes they see are
// the ones on the wire, which also covers the connections REALITY hands over
// to the dest server.
type wrapListener struct {
	net.Listener
	l *realityListener
}

func (ln *wrapListener) Accept() (net.Conn, error) {
	for {
		c, err := ln.Listener.Accept()
		if err != nil {
			return nil, err
		}

		// REALITY splices the transport with the dest server when the client
		// fails to authenticate, and needs CloseWrite to do so. Keep hold of
		// it, the wrappers below do not carry it over.
		cw, ok := c.(xio.CloseWrite)
		if !ok {
			ln.l.logger.Warnf("%s: connection does not support CloseWrite", c.RemoteAddr())
			c.Close()
			continue
		}

		opts := ln.l.options

		if opts.ConnLimiter != nil {
			host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			if lim := opts.ConnLimiter.Limiter(host); lim != nil {
				if !lim.Allow(1) {
					c.Close()
					continue
				}
				c = climiter.WrapConn(lim, c)
			}
		}

		c = metrics.WrapConn(opts.Service, c)
		c = stats.WrapConn(c, opts.Stats)
		c = limiter_wrapper.WrapConn(
			c,
			opts.TrafficLimiter,
			traffic_limiter.ServiceLimitKey,
			limiter.ScopeOption(limiter.ScopeService),
			limiter.ServiceOption(opts.Service),
			limiter.NetworkOption(c.LocalAddr().Network()),
		)
		c = limiter_wrapper.WrapConn(
			c,
			opts.TrafficLimiter,
			c.RemoteAddr().String(),
			limiter.ScopeOption(limiter.ScopeConn),
			limiter.ServiceOption(opts.Service),
			limiter.NetworkOption(c.LocalAddr().Network()),
			limiter.SrcOption(c.RemoteAddr().String()),
		)

		return &closeWriteConn{Conn: c, raw: cw}, nil
	}
}

type closeWriteConn struct {
	net.Conn
	raw xio.CloseWrite
}

func (c *closeWriteConn) CloseWrite() error {
	return c.raw.CloseWrite()
}

// realityConn hands the transport below the TLS layer to XTLS Vision.
type realityConn struct {
	*reality.Conn
	once sync.Once
}

// RawConn returns the connection REALITY runs on top of.
func (c *realityConn) RawConn() net.Conn {
	return c.Conn.NetConn()
}

// TLSBuffered returns what the TLS layer has read but not handed out yet: the
// plaintext of the record it decrypted last, followed by the bytes it has not
// processed. Once the peer has switched to direct copy those belong to the raw
// stream.
func (c *realityConn) TLSBuffered() (buffered io.Reader) {
	c.once.Do(func() {
		buffered = tlsBuffered(c.Conn)
	})
	return
}

var (
	tlsFields struct {
		sync.Once
		inputOffset    uintptr
		rawInputOffset uintptr
		ok             bool
	}
)

// tlsBuffered reaches into the private buffers of the TLS layer. They belong
// to the crypto/tls code REALITY is forked from and are not reachable in any
// other way, but XTLS Vision cannot hand the transport over without them.
// Xray does the same.
func tlsBuffered(c *reality.Conn) io.Reader {
	if !visionSupported() {
		return nil
	}

	p := unsafe.Pointer(c)
	input := (*bytes.Reader)(unsafe.Add(p, tlsFields.inputOffset))
	rawInput := (*bytes.Buffer)(unsafe.Add(p, tlsFields.rawInputOffset))

	return io.MultiReader(input, rawInput)
}

// visionSupported reports whether the TLS buffers can be reached, and so
// whether XTLS Vision can be offered.
func visionSupported() bool {
	tlsFields.Do(func() {
		t := reflect.TypeFor[reality.Conn]()
		input, ok1 := t.FieldByName("input")
		rawInput, ok2 := t.FieldByName("rawInput")
		if !ok1 || !ok2 ||
			input.Type != reflect.TypeFor[bytes.Reader]() ||
			rawInput.Type != reflect.TypeFor[bytes.Buffer]() {
			return
		}
		tlsFields.inputOffset = input.Offset
		tlsFields.rawInputOffset = rawInput.Offset
		tlsFields.ok = true
	})

	return tlsFields.ok
}
