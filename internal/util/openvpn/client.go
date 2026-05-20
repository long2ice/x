package openvpn

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// ClientConfig configures a client-side (dialer) OpenVPN connection.
type ClientConfig struct {
	Proto    string // "udp" or "tcp"
	Cipher   string // "AES-256-GCM", "AES-128-GCM" or "CHACHA20-POLY1305"
	Auth     string // control-channel OCC auth name, e.g. "SHA256"
	CA       []byte // PEM CA bundle used to verify the server certificate
	Cert     []byte // PEM client certificate (optional)
	Key      []byte // PEM client private key (optional)
	TLSCrypt []byte // 256-byte tls-crypt static key (raw)
	Username string
	Password string

	HandshakeTimeout time.Duration
}

// Client drives the client side of an OpenVPN handshake and then carries
// IP packets over the data channel. It handles server-initiated TLS
// renegotiation transparently.
type Client struct {
	config  *ClientConfig
	mux     *PacketMux
	crypt   *TLSCrypt
	control *ControlChannel
	tlsConn *tls.Conn
	tlsCfg  *tls.Config
	push    *PushReply
	cipher  string // negotiated data-channel cipher

	data   atomic.Pointer[DataChannel] // swapped on renegotiation
	runCtx context.Context
	cancel context.CancelFunc
}

// NewClient wires a Client over the given packet transport. Call
// Handshake before exchanging IP packets.
func NewClient(config *ClientConfig, pio PacketIO) (*Client, error) {
	if config == nil || pio == nil {
		return nil, errors.New("openvpn: nil client config or transport")
	}
	crypt, err := NewTLSCrypt(config.TLSCrypt, true)
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
	return &Client{
		config:  config,
		mux:     mux,
		crypt:   crypt,
		control: NewControlChannel(mux, crypt, local),
		runCtx:  runCtx,
		cancel:  cancel,
	}, nil
}

// Handshake performs the full client handshake and returns the server's
// PUSH_REPLY.
func (c *Client) Handshake(ctx context.Context) (*PushReply, error) {
	if _, ok := ctx.Deadline(); !ok {
		timeout := c.config.HandshakeTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// 1. Reset handshake.
	if _, err := c.control.Send(ctx, PControlHardResetClientV2, nil); err != nil {
		return nil, fmt.Errorf("send hard reset: %w", err)
	}
	if err := c.waitServerReset(ctx); err != nil {
		return nil, err
	}

	// 2. Control-channel TLS, with a retransmit timer to survive loss.
	tlsCfg, err := c.tlsConfig()
	if err != nil {
		return nil, err
	}
	c.tlsCfg = tlsCfg
	c.tlsConn = tls.Client(NewControlConn(ctx, c.control), tlsCfg)
	stop := startRetransmit(ctx, c.control)
	if err := c.tlsConn.HandshakeContext(ctx); err != nil {
		stop()
		return nil, fmt.Errorf("control-channel tls handshake: %w", err)
	}
	stop()

	// 3. Key method 2 exchange.
	clientRecord, err := c.newKeyMethodRecord()
	if err != nil {
		return nil, err
	}
	if _, err := c.tlsConn.Write(clientRecord.MarshalClient()); err != nil {
		return nil, fmt.Errorf("write client key method 2: %w", err)
	}
	serverRecord, err := readServerKeyMethod(ctx, c.tlsConn)
	if err != nil {
		return nil, err
	}

	sources := clientRecord.Sources
	sources.Server = serverRecord.Sources.Server
	keys, err := DeriveKeyMaterial(sources, c.control.LocalSessionID(), c.control.RemoteSessionID(), false)
	if err != nil {
		return nil, fmt.Errorf("derive data channel keys: %w", err)
	}

	// 4. PUSH.
	if _, err := c.tlsConn.Write([]byte(PushRequest + "\x00")); err != nil {
		return nil, fmt.Errorf("write push request: %w", err)
	}
	push, err := c.readPushReply(ctx)
	if err != nil {
		return nil, err
	}
	c.push = push

	// The server's pushed cipher wins over our configured default (NCP).
	c.cipher = push.Cipher
	if c.cipher == "" {
		c.cipher = c.config.Cipher
	}
	data, err := NewDataChannel(keys, push.PeerID, c.cipher)
	if err != nil {
		return nil, err
	}
	c.data.Store(data)

	go c.keepaliveLoop()
	go c.renegLoop()
	return push, nil
}

// renegLoop handles server-initiated TLS renegotiation. OpenVPN rekeys
// every reneg-sec (3600s by default) by opening a fresh TLS handshake on
// the next key_id; the loop runs that handshake and swaps the data
// channel to the new keys.
func (c *Client) renegLoop() {
	keyID := uint8(1)
	for {
		rc := NewControlChannel(c.mux, c.crypt, c.control.LocalSessionID())
		rc.keyID = keyID
		rc.SetRemoteSessionID(c.control.RemoteSessionID())

		// Wait for the server's soft reset on this key_id.
		pkt, err := rc.Read(c.runCtx)
		if err != nil {
			return // transport closed
		}
		if pkt.Opcode != PControlSoftResetV1 {
			continue // unexpected; wait for a proper soft reset
		}
		if err := c.renegotiate(rc); err != nil {
			_ = c.Close() // renegotiation failed: drop so the caller redials
			return
		}
		if keyID++; keyID > 7 {
			keyID = 1 // key_id 0 is reserved for the initial handshake
		}
	}
}

// renegotiate runs one renegotiation handshake on rc (whose soft reset
// has already been received) and atomically swaps in the new keys.
func (c *Client) renegotiate(rc *ControlChannel) error {
	ctx, cancel := context.WithTimeout(c.runCtx, 30*time.Second)
	defer cancel()

	// Answer the server's soft reset with ours.
	if _, err := rc.Send(ctx, PControlSoftResetV1, nil); err != nil {
		return fmt.Errorf("send soft reset: %w", err)
	}

	tlsConn := tls.Client(NewControlConn(ctx, rc), c.tlsCfg)
	stop := startRetransmit(ctx, rc)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		stop()
		return fmt.Errorf("renegotiation tls handshake: %w", err)
	}
	stop()
	defer tlsConn.Close()

	clientRecord, err := c.newKeyMethodRecord()
	if err != nil {
		return err
	}
	if _, err := tlsConn.Write(clientRecord.MarshalClient()); err != nil {
		return fmt.Errorf("write renegotiation key method 2: %w", err)
	}
	serverRecord, err := readServerKeyMethod(ctx, tlsConn)
	if err != nil {
		return err
	}
	sources := clientRecord.Sources
	sources.Server = serverRecord.Sources.Server
	keys, err := DeriveKeyMaterial(sources, c.control.LocalSessionID(), c.control.RemoteSessionID(), false)
	if err != nil {
		return fmt.Errorf("derive renegotiated keys: %w", err)
	}
	data, err := NewDataChannel(keys, c.push.PeerID, c.cipher)
	if err != nil {
		return err
	}
	data.keyID = rc.keyID // post-reneg data packets carry the new key_id
	c.data.Store(data)
	return nil
}

func (c *Client) newKeyMethodRecord() (*KeyMethod2Record, error) {
	return NewClientKeyMethod2Record(
		occOptionsString(c.config.Proto, c.config.Cipher, c.config.Auth, false),
		peerInfoString(c.config.Cipher),
		strings.TrimSpace(c.config.Username), c.config.Password,
	)
}

// keepaliveLoop sends an OpenVPN keepalive ping whenever the data channel
// has been idle, so the peer's ping-restart timer never fires.
func (c *Client) keepaliveLoop() {
	const interval = 8 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.runCtx.Done():
			return
		case <-t.C:
			if d := c.data.Load(); d != nil && d.IdleFor() >= interval {
				if err := c.WriteIPPacket(c.runCtx, KeepalivePing); err != nil {
					return
				}
			}
		}
	}
}

// WriteIPPacket encrypts and sends one IP packet over the data channel.
func (c *Client) WriteIPPacket(ctx context.Context, packet []byte) error {
	d := c.data.Load()
	if d == nil {
		return errors.New("openvpn: data channel not ready")
	}
	wire, err := d.Encrypt(packet)
	if err != nil {
		return err
	}
	return c.mux.WritePacket(ctx, wire)
}

// ReadIPPacket receives and decrypts one IP packet from the data channel.
func (c *Client) ReadIPPacket(ctx context.Context) ([]byte, error) {
	for {
		wire, err := c.mux.ReadDataPacket(ctx)
		if err != nil {
			return nil, err
		}
		d := c.data.Load()
		if d == nil {
			return nil, errors.New("openvpn: data channel not ready")
		}
		plain, err := d.Decrypt(wire)
		if err != nil {
			continue // drop replays / undecryptable (incl. transient post-reneg)
		}
		if IsKeepalive(plain) {
			continue // keepalive ping: never surfaced to the tun layer
		}
		return plain, nil
	}
}

func (c *Client) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	if c.tlsConn != nil {
		_ = c.tlsConn.Close()
	}
	if c.mux != nil {
		return c.mux.Close()
	}
	return nil
}

func (c *Client) LocalAddr() net.Addr  { return c.mux.LocalAddr() }
func (c *Client) RemoteAddr() net.Addr { return c.mux.RemoteAddr() }

// PushReply returns the server's pushed configuration (valid after a
// successful Handshake).
func (c *Client) PushReply() *PushReply { return c.push }

func (c *Client) waitServerReset(ctx context.Context) error {
	for {
		pkt, err := c.control.Read(ctx)
		if err != nil {
			return fmt.Errorf("read hard reset response: %w", err)
		}
		switch pkt.Opcode {
		case PControlHardResetServerV2:
			return c.control.SendAck(ctx)
		case PControlHardResetServerV1:
			return errors.New("openvpn: server requested unsupported key method 1")
		}
	}
}

// startRetransmit re-sends a control channel's unacked packets every
// second until the returned stop func is called.
func startRetransmit(ctx context.Context, cc *ControlChannel) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = cc.Retransmit(ctx)
			}
		}
	}()
	var once bool
	return func() {
		if !once {
			once = true
			close(done)
		}
	}
}

func readServerKeyMethod(ctx context.Context, tlsConn *tls.Conn) (*KeyMethod2Record, error) {
	buf, tmp := []byte(nil), make([]byte, 4096)
	for {
		if dl, ok := ctx.Deadline(); ok {
			_ = tlsConn.SetReadDeadline(dl)
		}
		n, err := tlsConn.Read(tmp)
		if err != nil {
			return nil, fmt.Errorf("read server key method 2: %w", err)
		}
		buf = append(buf, tmp[:n]...)
		record, err := ParseServerKeyMethod2Record(buf)
		if err == nil {
			return record, nil
		}
		if !errors.Is(err, errKeyMethodTruncated) {
			return nil, err
		}
	}
}

func (c *Client) readPushReply(ctx context.Context) (*PushReply, error) {
	buf, tmp := []byte(nil), make([]byte, 4096)
	for {
		if dl, ok := ctx.Deadline(); ok {
			_ = c.tlsConn.SetReadDeadline(dl)
		}
		n, err := c.tlsConn.Read(tmp)
		if err != nil {
			return nil, fmt.Errorf("read push reply: %w", err)
		}
		buf = append(buf, tmp[:n]...)
		if idx := bytes.IndexByte(buf, 0); idx >= 0 {
			return ParsePushReply(string(buf[:idx]))
		}
	}
}

func (c *Client) tlsConfig() (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(c.config.CA) {
		return nil, errors.New("openvpn: parse CA certificate")
	}
	cfg := &tls.Config{
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("openvpn: server presented no certificate")
			}
			inter := x509.NewCertPool()
			for _, cert := range cs.PeerCertificates[1:] {
				inter.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: inter,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		},
	}
	if len(bytes.TrimSpace(c.config.Cert)) > 0 && len(bytes.TrimSpace(c.config.Key)) > 0 {
		cert, err := tls.X509KeyPair(c.config.Cert, c.config.Key)
		if err != nil {
			return nil, fmt.Errorf("openvpn: parse client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}
