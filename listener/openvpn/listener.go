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

// openvpnListener accepts OpenVPN clients (stock or this project's
// dialer) and surfaces each as an IP-packet net.Conn for the tun handler.
type openvpnListener struct {
	ln      net.Listener   // TCP path
	pc      net.PacketConn // UDP path
	cqueue  chan net.Conn
	errChan chan error
	logger  logger.Logger
	md      metadata
	options listener.Options

	serverCfg *ovpn.ServerConfig
	pool      *ipPool

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

	l.pool = newIPPool(l.md.subnet)
	proto := "tcp"
	if l.md.udp {
		proto = "udp"
	}
	l.serverCfg = &ovpn.ServerConfig{
		Proto:            proto,
		Cipher:           l.md.cipher,
		Auth:             l.md.auth,
		CA:               l.md.ca,
		Cert:             l.md.cert,
		Key:              l.md.key,
		TLSCrypt:         l.md.tlsCrypt,
		Gateway:          l.pool.gateway,
		Netmask:          l.pool.netmask(),
		TunMTU:           l.md.mtu,
		HandshakeTimeout: l.md.handshakeTimeout,
	}

	l.cqueue = make(chan net.Conn, l.md.backlog)
	l.errChan = make(chan error, 1)
	l.closeCh = make(chan struct{})

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
	go l.serveUDP()
	go l.reapIdlePeers()
	return nil
}

func (l *openvpnListener) serveTCP() {
	for {
		raw, err := l.ln.Accept()
		if err != nil {
			l.errChan <- err
			close(l.errChan)
			return
		}
		go l.handshake(ovpn.NewStreamPacketIO(raw), raw)
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
		go l.handshake(ovpn.NewDatagramPacketIO(p), p)
	} else {
		l.peersMu.Unlock()
	}
	p.deliver(append([]byte(nil), pkt...))
}

func (l *openvpnListener) reapIdlePeers() {
	interval := l.md.idleTimeout / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cutoff := time.Now().Add(-l.md.idleTimeout)
			l.peersMu.Lock()
			var stale []*udpPeerConn
			for _, p := range l.peers {
				if p.lastActiveAt().Before(cutoff) {
					stale = append(stale, p)
				}
			}
			l.peersMu.Unlock()
			for _, p := range stale {
				_ = p.Close()
			}
		case <-l.closeCh:
			return
		}
	}
}

// handshake runs the OpenVPN server handshake for one client and, on
// success, enqueues an IP-packet conn for Accept.
func (l *openvpnListener) handshake(pio ovpn.PacketIO, raw interface{ Close() error }) {
	clientIP, peerID, err := l.pool.allocate()
	if err != nil {
		l.logger.Warnf("openvpn: %v", err)
		_ = raw.Close()
		return
	}

	sess, err := ovpn.Accept(pio, l.serverCfg, clientIP, peerID)
	if err != nil {
		l.logger.Debugf("openvpn: handshake failed: %v", err)
		l.pool.release(clientIP)
		_ = raw.Close()
		return
	}
	l.logger.Debugf("openvpn: client up, assigned %s peer-id %d", clientIP, peerID)

	conn := ovpn.NewServerConn(sess, l.md.subnet, l.md.mtu)
	conn.SetOnClose(func() { l.pool.release(clientIP) })

	select {
	case l.cqueue <- l.wrap(conn):
	case <-l.closeCh:
		_ = conn.Close()
	default:
		l.logger.Warnf("openvpn: connection queue full, dropping client %s", clientIP)
		_ = conn.Close()
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
		if host, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil {
			if lim := l.options.ConnLimiter.Limiter(host); lim != nil {
				if lim.Allow(1) {
					conn = climiter.WrapConn(lim, conn)
				} else {
					_ = conn.Close()
				}
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
	select {
	case <-l.closeCh:
	default:
		close(l.closeCh)
	}
	if l.ln != nil {
		return l.ln.Close()
	}
	return l.pc.Close()
}
