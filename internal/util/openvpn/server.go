package openvpn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ServerConfig is the static, per-listener server configuration. The
// per-client bits (assigned IP, peer id) are passed to Accept.
type ServerConfig struct {
	Proto    string // "udp" or "tcp"
	Cipher   string // data-channel cipher, e.g. "AES-256-GCM"
	Auth     string // OCC auth name
	CA       []byte // PEM CA bundle to verify client certificates
	Cert     []byte // PEM server certificate
	Key      []byte // PEM server private key
	TLSCrypt []byte // 256-byte tls-crypt static key (raw)

	Gateway netip.Addr // server tunnel IP, pushed as route-gateway
	Netmask netip.Addr // tunnel netmask
	TunMTU  int

	HandshakeTimeout time.Duration

	tlsConfig *tls.Config
}

// buildTLSConfig parses the server certificate/CA once and caches it.
func (c *ServerConfig) buildTLSConfig() (*tls.Config, error) {
	if c.tlsConfig != nil {
		return c.tlsConfig, nil
	}
	cert, err := tls.X509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, fmt.Errorf("openvpn: parse server certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(c.CA) {
		return nil, errors.New("openvpn: parse CA certificate")
	}
	c.tlsConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	return c.tlsConfig, nil
}

// ServerSession is one accepted client. It implements endpoint.
type ServerSession struct {
	mux     *PacketMux
	control *ControlChannel
	tlsConn *tls.Conn
	data    *DataChannel
	runCtx  context.Context
	cancel  context.CancelFunc

	clientIP netip.Addr
	peerID   uint32
}

// Accept runs the server side of a handshake for one client transport
// and returns a ready session. assignedIP and peerID come from the
// listener's address pool.
func Accept(pio PacketIO, cfg *ServerConfig, assignedIP netip.Addr, peerID uint32) (*ServerSession, error) {
	if cfg == nil || pio == nil {
		return nil, errors.New("openvpn: nil server config or transport")
	}
	tlsConfig, err := cfg.buildTLSConfig()
	if err != nil {
		return nil, err
	}
	crypt, err := NewTLSCrypt(cfg.TLSCrypt, false)
	if err != nil {
		return nil, err
	}
	local, err := NewSessionID()
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	mux := NewPacketMux(pio)
	go mux.Run(runCtx)

	s := &ServerSession{
		mux:      mux,
		control:  NewControlChannel(mux, crypt, local),
		runCtx:   runCtx,
		cancel:   cancel,
		clientIP: assignedIP,
		peerID:   peerID,
	}

	timeout := cfg.HandshakeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, hsCancel := context.WithTimeout(context.Background(), timeout)
	defer hsCancel()

	if err := s.handshake(ctx, cfg, tlsConfig); err != nil {
		cancel()
		_ = mux.Close()
		return nil, err
	}
	return s, nil
}

func (s *ServerSession) handshake(ctx context.Context, cfg *ServerConfig, tlsConfig *tls.Config) error {
	// 1. Reset handshake: read the client hard reset, answer with ours.
	if err := s.waitClientReset(ctx); err != nil {
		return err
	}
	if _, err := s.control.Send(ctx, PControlHardResetServerV2, nil); err != nil {
		return fmt.Errorf("send hard reset: %w", err)
	}

	// 2. Control-channel TLS (server side).
	s.tlsConn = tls.Server(NewControlConn(ctx, s.control), tlsConfig)
	stop := startRetransmit(ctx, s.control)
	if err := s.tlsConn.HandshakeContext(ctx); err != nil {
		stop()
		return fmt.Errorf("control-channel tls handshake: %w", err)
	}
	stop()

	// 3. Key method 2: read the client record, answer with the server's.
	clientRecord, err := s.readClientKeyMethod(ctx)
	if err != nil {
		return err
	}
	serverRecord, err := NewServerKeyMethod2Record(occOptionsString(cfg.Proto, cfg.Cipher, cfg.Auth, true))
	if err != nil {
		return err
	}
	if _, err := s.tlsConn.Write(serverRecord.MarshalServer()); err != nil {
		return fmt.Errorf("write server key method 2: %w", err)
	}

	sources := clientRecord.Sources
	sources.Server = serverRecord.Sources.Server
	keys, err := DeriveKeyMaterial(sources, s.control.RemoteSessionID(), s.control.LocalSessionID(), true)
	if err != nil {
		return fmt.Errorf("derive data channel keys: %w", err)
	}

	// 4. PUSH: wait for PUSH_REQUEST, answer with PUSH_REPLY.
	if err := s.waitPushRequest(ctx); err != nil {
		return err
	}
	reply := PushConfig{
		ClientIP:  s.clientIP,
		Netmask:   cfg.Netmask,
		Gateway:   cfg.Gateway,
		PeerID:    s.peerID,
		Cipher:    cfg.Cipher,
		TunMTU:    cfg.TunMTU,
		PingEvery: 10,
		PingExpit: 120,
	}
	if _, err := s.tlsConn.Write([]byte(reply.Build())); err != nil {
		return fmt.Errorf("write push reply: %w", err)
	}

	if s.data, err = NewDataChannel(keys, s.peerID, cfg.Cipher); err != nil {
		return err
	}
	go s.keepaliveLoop()
	return nil
}

// keepaliveLoop sends an OpenVPN keepalive ping whenever the data channel
// has been idle, so the client's ping-restart timer never fires.
func (s *ServerSession) keepaliveLoop() {
	const interval = 8 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.runCtx.Done():
			return
		case <-t.C:
			if s.data != nil && s.data.IdleFor() >= interval {
				if err := s.WriteIPPacket(s.runCtx, KeepalivePing); err != nil {
					return
				}
			}
		}
	}
}

func (s *ServerSession) waitClientReset(ctx context.Context) error {
	for {
		pkt, err := s.control.Read(ctx)
		if err != nil {
			return fmt.Errorf("read client hard reset: %w", err)
		}
		switch pkt.Opcode {
		case PControlHardResetClientV2:
			return nil
		case PControlHardResetClientV1, PControlHardResetClientV3:
			return fmt.Errorf("openvpn: unsupported client reset %s", pkt.Opcode)
		}
	}
}

func (s *ServerSession) readClientKeyMethod(ctx context.Context) (*KeyMethod2Record, error) {
	buf, tmp := []byte(nil), make([]byte, 4096)
	for {
		if dl, ok := ctx.Deadline(); ok {
			_ = s.tlsConn.SetReadDeadline(dl)
		}
		n, err := s.tlsConn.Read(tmp)
		if err != nil {
			return nil, fmt.Errorf("read client key method 2: %w", err)
		}
		buf = append(buf, tmp[:n]...)
		record, err := ParseClientKeyMethod2Record(buf)
		if err == nil {
			return record, nil
		}
		if !errors.Is(err, errKeyMethodTruncated) {
			return nil, err
		}
	}
}

func (s *ServerSession) waitPushRequest(ctx context.Context) error {
	buf, tmp := []byte(nil), make([]byte, 1024)
	for {
		if dl, ok := ctx.Deadline(); ok {
			_ = s.tlsConn.SetReadDeadline(dl)
		}
		n, err := s.tlsConn.Read(tmp)
		if err != nil {
			return fmt.Errorf("read push request: %w", err)
		}
		buf = append(buf, tmp[:n]...)
		if strings.Contains(string(buf), PushRequest) {
			return nil
		}
	}
}

// --- endpoint -------------------------------------------------------------

func (s *ServerSession) WriteIPPacket(ctx context.Context, packet []byte) error {
	if s.data == nil {
		return errors.New("openvpn: data channel not ready")
	}
	wire, err := s.data.Encrypt(packet)
	if err != nil {
		return err
	}
	return s.mux.WritePacket(ctx, wire)
}

func (s *ServerSession) ReadIPPacket(ctx context.Context) ([]byte, error) {
	if s.data == nil {
		return nil, errors.New("openvpn: data channel not ready")
	}
	for {
		wire, err := s.mux.ReadDataPacket(ctx)
		if err != nil {
			return nil, err
		}
		plain, err := s.data.Decrypt(wire)
		if err != nil {
			continue
		}
		if IsKeepalive(plain) {
			continue // keepalive ping: never surfaced to the tun layer
		}
		return plain, nil
	}
}

func (s *ServerSession) ClientIP() netip.Addr { return s.clientIP }
func (s *ServerSession) PeerID() uint32       { return s.peerID }

func (s *ServerSession) LocalAddr() net.Addr  { return s.mux.LocalAddr() }
func (s *ServerSession) RemoteAddr() net.Addr { return s.mux.RemoteAddr() }

func (s *ServerSession) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.tlsConn != nil {
		_ = s.tlsConn.Close()
	}
	if s.mux != nil {
		return s.mux.Close()
	}
	return nil
}
