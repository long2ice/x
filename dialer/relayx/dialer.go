package relayx

import (
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/go-gost/core/dialer"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/net/proxyproto"
	"github.com/go-gost/x/internal/util/mux"
	ws_util "github.com/go-gost/x/internal/util/ws"
	"github.com/go-gost/x/internal/util/wspad"
	"github.com/go-gost/x/registry"
	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/hkdf"
)

const (
	tokenMuxFlagBit byte = 0x80
	respMuxFlagBit  byte = 0x01
)

func init() {
	registry.DialerRegistry().Register("relayx", NewDialer)
}

type relayxDialer struct {
	md      metadata
	authKey []byte
	logger  logger.Logger
	options dialer.Options
}

func NewDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &relayxDialer{
		logger:  options.Logger,
		options: options,
	}
}

func (d *relayxDialer) Init(m md.Metadata) error {
	if err := d.parseMetadata(m); err != nil {
		return err
	}
	d.authKey = make([]byte, 32)
	r := hkdf.New(sha256.New, []byte(d.md.key), nil, []byte("relayx-auth-v1"))
	if _, err := io.ReadFull(r, d.authKey); err != nil {
		return fmt.Errorf("relayx: derive authKey: %w", err)
	}
	return nil
}

// Multiplex implements dialer.Multiplexer interface.
func (d *relayxDialer) Multiplex() bool {
	return d.md.mux
}

func (d *relayxDialer) Dial(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
	if !d.md.mux {
		return d.dialRaw(ctx, addr, opts...)
	}

	key := d.sessionKey(ctx, addr)
	entry := getSharedEntry(key)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.session != nil && entry.session.IsClosed() {
		entry.clearSessionLocked()
	}
	if entry.session == nil {
		conn, err := d.dialRaw(ctx, addr, opts...)
		if err != nil {
			return nil, err
		}
		entry.session = &muxSession{conn: conn}
	}
	return entry.session.conn, nil
}

func (d *relayxDialer) dialRaw(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
	var options dialer.DialOptions
	for _, opt := range opts {
		opt(&options)
	}

	conn, err := options.Dialer.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return proxyproto.WrapClientConn(
		d.options.ProxyProtocol,
		xctx.SrcAddrFromContext(ctx),
		xctx.DstAddrFromContext(ctx),
		conn,
	), nil
}

func (d *relayxDialer) Handshake(ctx context.Context, conn net.Conn, opts ...dialer.HandshakeOption) (net.Conn, error) {
	hopts := &dialer.HandshakeOptions{}
	for _, opt := range opts {
		opt(hopts)
	}

	if d.md.handshakeTimeout > 0 {
		conn.SetDeadline(time.Now().Add(d.md.handshakeTimeout))
		defer conn.SetDeadline(time.Time{})
	}

	if !d.md.mux {
		tunnel, _, err := d.doHandshake(ctx, conn, hopts, false)
		if err != nil {
			conn.Close()
			return nil, err
		}
		return tunnel, nil
	}

	key := d.sessionKey(ctx, hopts.Addr)
	entry := getSharedEntry(key)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	session := entry.session
	if session != nil && session.conn != conn {
		// The caller handed us a conn for a now-stale entry; tell them to retry.
		conn.Close()
		return nil, errors.New("relayx: unrecognized connection")
	}
	if session == nil {
		session = &muxSession{conn: conn}
		entry.session = session
	}

	if session.session != nil {
		cc, err := session.GetConn()
		if err != nil {
			session.Close()
			entry.clearSessionLocked()
			dropSharedEntry(key, entry)
			return nil, err
		}
		return cc, nil
	}

	tunnel, muxNegotiated, err := d.doHandshake(ctx, conn, hopts, true)
	if err != nil {
		conn.Close()
		entry.clearSessionLocked()
		dropSharedEntry(key, entry)
		return nil, err
	}

	if !muxNegotiated {
		// Peer does not speak mux; use this conn single-shot and forget the
		// shared entry so the next caller opens a fresh TCP.
		entry.clearSessionLocked()
		dropSharedEntry(key, entry)
		return tunnel, nil
	}

	s, err := mux.ClientSession(tunnel, d.md.muxCfg)
	if err != nil {
		tunnel.Close()
		entry.clearSessionLocked()
		dropSharedEntry(key, entry)
		return nil, err
	}
	session.session = s
	entry.startIdleReaperLocked(key, session, d.md.muxIdleTimeout)

	cc, err := session.GetConn()
	if err != nil {
		session.Close()
		entry.clearSessionLocked()
		dropSharedEntry(key, entry)
		return nil, err
	}
	return cc, nil
}

// doHandshake performs the uTLS handshake then upgrades the connection to a
// WebSocket session. The mux flag is encoded covertly in the auth token nonce;
// the server echoes its decision in the first byte of the first WS binary
// message after the upgrade response.
func (d *relayxDialer) doHandshake(ctx context.Context, conn net.Conn, hopts *dialer.HandshakeOptions, advertiseMux bool) (net.Conn, bool, error) {
	uTLSConfig := &utls.Config{}
	if tlsCfg := d.options.TLSConfig; tlsCfg != nil {
		uTLSConfig.ServerName = tlsCfg.ServerName
		uTLSConfig.InsecureSkipVerify = tlsCfg.InsecureSkipVerify
		uTLSConfig.RootCAs = tlsCfg.RootCAs
		uTLSConfig.NextProtos = tlsCfg.NextProtos
	}
	tlsConn := utls.UClient(conn, uTLSConfig, utls.HelloChrome_Auto)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, false, err
	}

	token, err := d.buildToken(advertiseMux)
	if err != nil {
		return nil, false, err
	}

	host := d.md.host
	if host == "" {
		if h, _, err := net.SplitHostPort(hopts.Addr); err == nil {
			host = h
		} else {
			host = hopts.Addr
		}
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("User-Agent", d.randomUserAgent())
	headers.Set("Origin", "https://"+host)
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("Accept-Language", "en-US,en;q=0.9")

	wsd := &websocket.Dialer{
		NetDialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return tlsConn, nil
		},
		HandshakeTimeout:  d.md.handshakeTimeout,
		EnableCompression: false,
	}

	u := &url.URL{Scheme: "ws", Host: host, Path: d.randomPath()}
	wsConn, resp, err := wsd.DialContext(ctx, u.String(), headers)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, false, fmt.Errorf("relayx: ws upgrade: %w", err)
	}
	resp.Body.Close()

	tunnel := wspad.Conn(ws_util.Conn(wsConn))

	muxBuf := make([]byte, 4096)
	n, err := tunnel.Read(muxBuf)
	if err != nil {
		tunnel.Close()
		return nil, false, fmt.Errorf("relayx: read mux signal: %w", err)
	}
	if n == 0 {
		tunnel.Close()
		return nil, false, errors.New("relayx: empty mux signal")
	}
	muxNegotiated := advertiseMux && muxBuf[0]&respMuxFlagBit != 0

	return tunnel, muxNegotiated, nil
}

func (d *relayxDialer) buildToken(advertiseMux bool) (string, error) {
	var nonce [16]byte
	if _, err := crand.Read(nonce[:]); err != nil {
		return "", err
	}
	if advertiseMux {
		nonce[0] |= tokenMuxFlagBit
	} else {
		nonce[0] &^= tokenMuxFlagBit
	}
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(time.Now().Unix()))

	h := hmac.New(sha256.New, d.authKey)
	h.Write(nonce[:])
	h.Write(tsBuf[:])
	mac := h.Sum(nil)

	var raw [56]byte
	copy(raw[:16], nonce[:])
	copy(raw[16:24], tsBuf[:])
	copy(raw[24:], mac)
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (d *relayxDialer) randomPath() string {
	if d.md.path != "" {
		return d.md.path
	}
	return defaultPaths[randomIndex(len(defaultPaths))]
}

func (d *relayxDialer) randomUserAgent() string {
	if d.md.userAgent != "" {
		return d.md.userAgent
	}
	return defaultUserAgents[randomIndex(len(defaultUserAgents))]
}

func randomIndex(n int) int {
	if n <= 1 {
		return 0
	}
	return rand.IntN(n)
}

var defaultPaths = []string{
	"/api/v1/upload",
	"/api/v2/data",
	"/api/v1/sync",
	"/upload",
	"/api/data/push",
	"/v1/events",
	"/api/stream",
	"/data/ingest",
}

var defaultUserAgents = []string{
	// Chrome
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	// Edge
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36 Edg/137.0.0.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36 Edg/136.0.0.0",
	// Firefox
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:138.0) Gecko/20100101 Firefox/138.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14.5; rv:138.0) Gecko/20100101 Firefox/138.0",
	"Mozilla/5.0 (X11; Linux x86_64; rv:138.0) Gecko/20100101 Firefox/138.0",
	// Safari
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.4 Safari/605.1.15",
}
