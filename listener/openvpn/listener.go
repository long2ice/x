package openvpn

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	admission "github.com/go-gost/x/admission/wrapper"
	xnet "github.com/go-gost/x/internal/net"
	"github.com/go-gost/x/internal/net/proxyproto"
	ovpn "github.com/go-gost/x/internal/util/openvpn"
	climiter "github.com/go-gost/x/limiter/conn/wrapper"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
)

func init() {
	registry.ListenerRegistry().Register("openvpn", NewListener)
}

type openvpnListener struct {
	ln      net.Listener    // TCP path
	pc      net.PacketConn  // UDP path
	cqueue  chan net.Conn
	errChan chan error
	logger  logger.Logger
	md      metadata
	options listener.Options

	// UDP demux state
	peersMu sync.Mutex
	peers   map[string]*udpPeerConn
	closeCh chan struct{}
}

func NewListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &openvpnListener{
		logger:  options.Logger,
		options: options,
	}
}

func (l *openvpnListener) Init(m md.Metadata) error {
	if err := l.parseMetadata(m); err != nil {
		return err
	}
	l.cqueue = make(chan net.Conn, l.md.backlog)
	l.errChan = make(chan error, 1)
	if l.md.udp {
		return l.initUDP()
	}
	return l.initTCP()
}

func (l *openvpnListener) initTCP() error {
	network := "tcp"
	if xnet.IsIPv4(l.options.Addr) {
		network = "tcp4"
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), network, l.options.Addr)
	if err != nil {
		return err
	}
	ln = proxyproto.WrapListener(l.options.ProxyProtocol, ln, 10*time.Second)
	ln = admission.WrapListener(l.options.Service, l.options.Admission, ln)
	l.ln = ln
	go l.serveTCP()
	return nil
}

func (l *openvpnListener) initUDP() error {
	network := "udp"
	if xnet.IsIPv4(l.options.Addr) {
		network = "udp4"
	}
	pc, err := net.ListenPacket(network, l.options.Addr)
	if err != nil {
		return err
	}
	l.pc = pc
	l.peers = make(map[string]*udpPeerConn)
	l.closeCh = make(chan struct{})
	go l.serveUDP()
	go l.reapIdlePeers()
	return nil
}

func (l *openvpnListener) reapIdlePeers() {
	interval := l.md.idleTimeout / 2
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cutoff := time.Now().Add(-l.md.idleTimeout)
			l.peersMu.Lock()
			if l.peers == nil {
				l.peersMu.Unlock()
				return
			}
			var toClose []*udpPeerConn
			for _, p := range l.peers {
				if !p.isHandshakeDone() {
					continue
				}
				if p.lastActiveAt().Before(cutoff) {
					toClose = append(toClose, p)
				}
			}
			l.peersMu.Unlock()
			for _, p := range toClose {
				_ = p.Close()
			}
		case <-l.closeCh:
			return
		}
	}
}

func (l *openvpnListener) serveTCP() {
	for {
		raw, err := l.ln.Accept()
		if err != nil {
			l.errChan <- err
			close(l.errChan)
			return
		}
		go l.handshake(raw, true)
	}
}

func (l *openvpnListener) serveUDP() {
	defer func() {
		l.peersMu.Lock()
		for _, p := range l.peers {
			_ = p.Close()
		}
		l.peers = nil
		l.peersMu.Unlock()
	}()
	for {
		buf := make([]byte, udpReadBuffer)
		n, addr, err := l.pc.ReadFrom(buf)
		if err != nil {
			l.errChan <- err
			close(l.errChan)
			return
		}
		l.dispatchUDP(buf[:n], addr)
	}
}

func (l *openvpnListener) dispatchUDP(pkt []byte, addr net.Addr) {
	key := addr.String()
	l.peersMu.Lock()
	if l.peers == nil {
		l.peersMu.Unlock()
		return
	}
	p, ok := l.peers[key]
	if !ok {
		p = newUDPPeerConn(l.pc, addr, func() {
			l.peersMu.Lock()
			delete(l.peers, key)
			l.peersMu.Unlock()
		})
		l.peers[key] = p
		l.peersMu.Unlock()
		go l.handshake(p, false)
	} else {
		l.peersMu.Unlock()
	}
	p.deliver(pkt)
}

func (l *openvpnListener) handshake(raw net.Conn, framed bool) {
	if l.md.handshakeTimeout > 0 {
		_ = raw.SetDeadline(time.Now().Add(l.md.handshakeTimeout))
	}
	var (
		tunnel *ovpn.Tunnel
		err    error
	)
	if framed {
		tunnel, err = ovpn.ServerHandshake(raw, l.md.key)
	} else {
		tunnel, err = ovpn.ServerHandshakePacket(raw, l.md.key)
	}
	if err != nil {
		l.logger.Debugf("handshake from %s: %v", raw.RemoteAddr(), err)
		_ = raw.Close()
		return
	}
	_ = raw.SetDeadline(time.Time{})

	if pc, ok := raw.(*udpPeerConn); ok {
		pc.markHandshakeDone()
	}

	conn := l.wrap(tunnel)
	select {
	case l.cqueue <- conn:
	default:
		l.logger.Warnf("connection queue full, dropping client %s", raw.RemoteAddr())
		_ = tunnel.Close()
	}
}

func (l *openvpnListener) wrap(conn net.Conn) net.Conn {
	conn = limiter_wrapper.WrapConn(
		conn,
		l.options.TrafficLimiter,
		conn.RemoteAddr().String(),
		limiter.ScopeOption(limiter.ScopeConn),
		limiter.ServiceOption(l.options.Service),
		limiter.NetworkOption(conn.LocalAddr().Network()),
		limiter.SrcOption(conn.RemoteAddr().String()),
	)
	conn = metrics.WrapConn(l.options.Service, conn)
	conn = stats.WrapConn(conn, l.options.Stats)
	if l.options.ConnLimiter != nil {
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		if lim := l.options.ConnLimiter.Limiter(host); lim != nil {
			if lim.Allow(1) {
				conn = climiter.WrapConn(lim, conn)
			} else {
				_ = conn.Close()
			}
		}
	}
	return conn
}

func (l *openvpnListener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.cqueue:
		if !ok {
			return nil, listener.ErrClosed
		}
		return conn, nil
	case err, ok := <-l.errChan:
		if !ok {
			return nil, listener.ErrClosed
		}
		if err == nil {
			err = errors.New("openvpn listener: accept loop exited")
		}
		return nil, err
	}
}

func (l *openvpnListener) Addr() net.Addr {
	if l.ln != nil {
		return l.ln.Addr()
	}
	return l.pc.LocalAddr()
}

func (l *openvpnListener) Close() error {
	if l.closeCh != nil {
		select {
		case <-l.closeCh:
		default:
			close(l.closeCh)
		}
	}
	if l.ln != nil {
		return l.ln.Close()
	}
	return l.pc.Close()
}
