package relayx

import (
	"bufio"
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-gost/core/dialer"
	md "github.com/go-gost/core/metadata"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/net/proxyproto"
	"github.com/go-gost/x/registry"
	"golang.org/x/crypto/hkdf"
)

func init() {
	registry.DialerRegistry().Register("relayx", NewDialer)
}

type relayxDialer struct {
	md      metadata
	authKey []byte
	options dialer.Options
}

func NewDialer(opts ...dialer.Option) dialer.Dialer {
	options := dialer.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &relayxDialer{options: options}
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

func (d *relayxDialer) Dial(ctx context.Context, addr string, opts ...dialer.DialOption) (net.Conn, error) {
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

	tlsConn := tls.Client(conn, d.options.TLSConfig)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	token, err := d.buildToken()
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	host := d.md.host
	if host == "" {
		if h, _, err := net.SplitHostPort(hopts.Addr); err == nil {
			host = h
		} else {
			host = hopts.Addr
		}
	}

	req := &http.Request{
		Method:        http.MethodPost,
		URL:           &url.URL{Scheme: "https", Host: host, Path: d.randomPath()},
		Host:          host,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		ContentLength: -1,
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", d.randomUserAgent())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "keep-alive")

	if err := req.Write(tlsConn); err != nil {
		tlsConn.Close()
		return nil, err
	}

	br := dialerBufReaderPool.Get().(*bufio.Reader)
	br.Reset(tlsConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		br.Reset(nil)
		dialerBufReaderPool.Put(br)
		tlsConn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		br.Reset(nil)
		dialerBufReaderPool.Put(br)
		tlsConn.Close()
		return nil, fmt.Errorf("relayx: server returned %s", resp.Status)
	}

	return &connWithBufReader{Conn: tlsConn, br: br}, nil
}

func (d *relayxDialer) buildToken() (string, error) {
	var nonce [16]byte
	if _, err := crand.Read(nonce[:]); err != nil {
		return "", err
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

type connWithBufReader struct {
	net.Conn
	br   *bufio.Reader
	once sync.Once
}

func (c *connWithBufReader) Read(b []byte) (int, error) {
	if c.br != nil {
		if c.br.Buffered() > 0 {
			return c.br.Read(b)
		}
		c.releaseReader()
	}
	return c.Conn.Read(b)
}

func (c *connWithBufReader) Close() error {
	c.releaseReader()
	return c.Conn.Close()
}

func (c *connWithBufReader) releaseReader() {
	c.once.Do(func() {
		if c.br == nil {
			return
		}
		c.br.Reset(nil)
		dialerBufReaderPool.Put(c.br)
		c.br = nil
	})
}

func randomIndex(n int) int {
	if n <= 1 {
		return 0
	}
	var b [1]byte
	if _, err := crand.Read(b[:]); err != nil {
		return 0
	}
	return int(b[0]) % n
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
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
}

var dialerBufReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 4096) },
}
