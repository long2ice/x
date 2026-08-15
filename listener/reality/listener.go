package reality

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	admission "github.com/go-gost/x/admission/wrapper"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/net/proxyproto"
	"github.com/go-gost/x/registry"
	"github.com/xtls/reality"
	"golang.org/x/crypto/curve25519"
)

func init() {
	registry.ListenerRegistry().Register("reality", NewListener)
}

type realityListener struct {
	ln      net.Listener
	cfg     *reality.Config
	conns   chan net.Conn
	done    chan struct{}
	err     error
	closed  sync.Once
	logger  logger.Logger
	md      metadata
	options listener.Options
}

func NewListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &realityListener{
		logger:  options.Logger,
		options: options,
	}
}

func (l *realityListener) Init(md md.Metadata) (err error) {
	if err = l.parseMetadata(md); err != nil {
		return
	}

	network := "tcp"
	if xnet.IsIPv4(l.options.Addr) {
		network = "tcp4"
	}

	lc := net.ListenConfig{}
	if l.md.mptcp {
		lc.SetMultipathTCP(true)
		l.logger.Debugf("mptcp enabled: %v", lc.MultipathTCP())
	}
	ln, err := lc.Listen(context.Background(), network, l.options.Addr)
	if err != nil {
		return
	}

	ln = proxyproto.WrapListener(l.options.ProxyProtocol, ln, 10*time.Second)
	ln = admission.WrapListener(l.options.Service, l.options.Admission, ln)
	ln = &wrapListener{Listener: ln, l: l}

	cfg := &reality.Config{
		Show:         l.md.show,
		Type:         l.md.typ,
		Dest:         l.md.dest,
		Xver:         l.md.xver,
		PrivateKey:   l.md.privateKey,
		MinClientVer: l.md.minClientVer,
		MaxClientVer: l.md.maxClientVer,
		MaxTimeDiff:  l.md.maxTimeDiff,
		ServerNames:  make(map[string]bool),
		ShortIds:     make(map[[8]byte]bool),

		// REALITY pads its own records to the lengths of the ones the dest
		// server sent. It never gets a length for a session ticket, so it
		// must not send any.
		SessionTicketsDisabled: true,
		NextProtos:             nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: l.md.dialTimeout}
			return d.DialContext(ctx, network, addr)
		},
	}
	for _, name := range l.md.serverNames {
		cfg.ServerNames[name] = true
	}
	for _, id := range l.md.shortIDs {
		cfg.ShortIds[id] = true
	}

	// reality.NewListener is not used on purpose: its accept loop exits for
	// good on the first transient error (e.g. ECONNABORTED) while keeping the
	// socket open, so the kernel keeps completing handshakes into a backlog
	// nobody drains and the port silently goes dark. Run the handshakes from
	// our own loop instead, which survives transient errors, and hand the
	// established connections over a buffered channel.
	l.ln = ln
	l.cfg = cfg
	l.conns = make(chan net.Conn, 128)
	l.done = make(chan struct{})
	go reality.DetectPostHandshakeRecordsLens(cfg)
	go l.acceptLoop()

	if pub, err := curve25519.X25519(l.md.privateKey, curve25519.Basepoint); err == nil {
		l.logger.Infof("reality dest %s, public key %s",
			l.md.dest, base64.RawURLEncoding.EncodeToString(pub))
	}
	if !visionSupported() {
		l.logger.Warn("xtls-rprx-vision is unavailable, the TLS buffers of the reality package could not be located")
	}

	return
}

// acceptLoop accepts raw connections and runs the REALITY handshake of each
// in its own goroutine, since the handshake dials the dest server and must
// not hold up the loop. Transient accept errors are retried with a backoff.
func (l *realityListener) acceptLoop() {
	var tempDelay time.Duration
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				close(l.conns)
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 1 * time.Second
				} else {
					tempDelay *= 2
				}
				if max := 5 * time.Second; tempDelay > max {
					tempDelay = max
				}
				l.logger.Warnf("reality: accept: %v, retrying in %v", err, tempDelay)
				select {
				case <-time.After(tempDelay):
				case <-l.done:
				}
				continue
			}
			l.err = err
			close(l.conns)
			return
		}
		tempDelay = 0

		go l.handshake(conn)
	}
}

func (l *realityListener) handshake(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			l.logger.Errorf("reality: handshake panic: %v", r)
			conn.Close()
		}
	}()

	c, err := reality.Server(context.Background(), conn, l.cfg)
	if err != nil {
		// Includes the connections REALITY proxied to the dest server after
		// a failed authentication; either way this end is done with them.
		conn.Close()
		return
	}

	select {
	case l.conns <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *realityListener) Accept() (net.Conn, error) {
	conn, ok := <-l.conns
	if !ok {
		if l.err != nil {
			return nil, l.err
		}
		return nil, net.ErrClosed
	}

	// The connection is already wrapped underneath, see wrapListener.
	if rc, ok := conn.(*reality.Conn); ok && visionSupported() {
		return &realityConn{Conn: rc}, nil
	}
	return conn, nil
}

func (l *realityListener) Addr() net.Addr {
	return l.ln.Addr()
}

func (l *realityListener) Close() error {
	l.closed.Do(func() {
		close(l.done)
	})
	return l.ln.Close()
}
