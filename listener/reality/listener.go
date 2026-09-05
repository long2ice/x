package reality

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	admission "github.com/go-gost/x/admission/wrapper"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/net/accept"
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
	// a bounded setup listener instead, which survives transient errors and
	// retains ownership until the completed connection is handed to Accept.
	l.cfg = cfg
	l.destResolver = destResolver
	l.inflight = make(map[string]int)
	startRecordDetection(cfg)
	l.ln = accept.NewListener(ln, accept.Config{
		AcceptLoops: l.md.acceptLoops,
		MaxPending:  l.md.maxPending,
		Timeout:     l.md.handshakeTimeout,
		Prepare:     l.handshake,
	})

	if pub, err := curve25519.X25519(l.md.privateKey, curve25519.Basepoint); err == nil {
		l.logger.Infof("reality dest %s, public key %s",
			l.md.dest, base64.RawURLEncoding.EncodeToString(pub))
	}
	if !visionSupported() {
		l.logger.Warn("xtls-rprx-vision is unavailable, the TLS buffers of the reality package could not be located")
	}

	return
}

func (l *realityListener) handshake(ctx context.Context, conn net.Conn) (result net.Conn, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("reality: handshake panic: %v", r)
			conn.Close()
		}
	}()
	// Upstream waits for this global cache without consulting ctx. Only enter
	// Server once detection has finished; cancellation stays effective here.
	if err := waitRecordDetection(ctx, l.cfg); err != nil {
		return nil, err
	}
	cfg := l.cfg.Clone()
	var target net.Conn
	var stop func() bool
	defer func() {
		if stop != nil {
			stop()
		}
		if target != nil {
			target.Close()
		}
	}()
	cfg.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
		var err error
		target, err = l.cfg.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		// The client deadline cannot interrupt MirrorConn.Target.Write or a
		// fallback read from dest. Own and cancel BOTH sockets, including dial.
		if deadline, ok := ctx.Deadline(); ok {
			if err = target.SetDeadline(deadline); err != nil {
				target.Close()
				return nil, err
			}
		}
		stop = context.AfterFunc(ctx, func() { target.Close() })
		return target, nil
	}
	return reality.Server(ctx, conn, cfg)
}

func (l *realityListener) Accept() (net.Conn, error) {
	conn, err := l.ln.Accept()
	if err != nil {
		return nil, err
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
	return l.ln.Close()
}
