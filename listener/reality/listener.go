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
	ln           net.Listener
	cfg          *reality.Config
	conns        chan net.Conn
	done         chan struct{}
	err          error
	closed       sync.Once
	closeConns   sync.Once
	destResolver *cachedResolver
	logger       logger.Logger
	md           metadata
	options      listener.Options

	// inflight counts the live connections each source address holds on this
	// port, so no single client can occupy the whole port.
	inflightMu sync.Mutex
	inflight   map[string]int
}

// acquire reserves a connection slot for host, reporting whether one was
// free. The slot is held for the connection's whole lifetime and returned by
// release, which the connection's Close triggers.
func (l *realityListener) acquire(host string) bool {
	if l.md.maxConnsPerIP <= 0 {
		return true
	}

	l.inflightMu.Lock()
	defer l.inflightMu.Unlock()
	if l.inflight[host] >= l.md.maxConnsPerIP {
		return false
	}
	l.inflight[host]++
	return true
}

func (l *realityListener) release(host string) {
	if l.md.maxConnsPerIP <= 0 {
		return
	}

	l.inflightMu.Lock()
	defer l.inflightMu.Unlock()
	if n := l.inflight[host]; n <= 1 {
		delete(l.inflight, host)
	} else {
		l.inflight[host] = n - 1
	}
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

	// Go enables MPTCP for TCP listeners by default. ISP middleboxes that
	// mangle the MPTCP options leave those clients' connections sitting in
	// the accept queue as children that never become acceptable, until the
	// queue is full of them and the port goes dark. Set it explicitly so it
	// is off unless asked for.
	lc := net.ListenConfig{}
	lc.SetMultipathTCP(l.md.mptcp)
	if l.md.mptcp {
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
			return l.destResolver.dial(ctx, &d, network, addr)
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
	l.destResolver = destResolver
	l.inflight = make(map[string]int)
	l.conns = make(chan net.Conn, 128)
	l.done = make(chan struct{})
	go reality.DetectPostHandshakeRecordsLens(cfg)
	for range 4 {
		go l.acceptLoop()
	}

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
// Several instances run per listener (see Init): accepting from multiple
// goroutines is safe, and on a busy box it multiplies the chances that one of
// them is scheduled promptly, so the accept queue drains under load instead
// of waiting behind thousands of runnable goroutines.
func (l *realityListener) acceptLoop() {
	var tempDelay time.Duration
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				l.closeConns.Do(func() { close(l.conns) })
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
			l.closeConns.Do(func() {
				l.err = err
				close(l.conns)
			})
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

	// Bound the whole handshake. REALITY reads the ClientHello off conn with
	// no deadline; a client that connects and then stalls would otherwise
	// hold this goroutine (and the connection) forever. The deadline also
	// caps how long a failed-auth fallback splice to the dest can run.
	if l.md.handshakeTimeout > 0 {
		conn.SetDeadline(time.Now().Add(l.md.handshakeTimeout))
	}

	c, err := reality.Server(context.Background(), conn, l.cfg)
	if err != nil {
		// Includes the connections REALITY proxied to the dest server after
		// a failed authentication; either way this end is done with them.
		conn.Close()
		return
	}

	// Clear it so it does not later interrupt the proxied traffic.
	if l.md.handshakeTimeout > 0 {
		c.SetDeadline(time.Time{})
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
