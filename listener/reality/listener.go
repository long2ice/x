package reality

import (
	"context"
	"encoding/base64"
	"net"
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

	l.ln = reality.NewListener(ln, cfg)

	if pub, err := curve25519.X25519(l.md.privateKey, curve25519.Basepoint); err == nil {
		l.logger.Infof("reality dest %s, public key %s",
			l.md.dest, base64.RawURLEncoding.EncodeToString(pub))
	}
	if !visionSupported() {
		l.logger.Warn("xtls-rprx-vision is unavailable, the TLS buffers of the reality package could not be located")
	}

	return
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
	err := l.ln.Close()

	// REALITY hands the connections that finished their handshake over an
	// unbuffered channel. Once nothing accepts them anymore the goroutine of
	// every handshake still in flight would block on it forever, holding its
	// connection, so drain until the listener reports it is done.
	go func() {
		for {
			conn, err := l.ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	return err
}
