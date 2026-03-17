package relayx

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	admission "github.com/go-gost/x/admission/wrapper"
	xctx "github.com/go-gost/x/ctx"
	xnet "github.com/go-gost/x/internal/net"
	xhttp "github.com/go-gost/x/internal/net/http"
	"github.com/go-gost/x/internal/net/proxyproto"
	xtls "github.com/go-gost/x/internal/util/tls"
	climiter "github.com/go-gost/x/limiter/conn/wrapper"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
	"golang.org/x/crypto/hkdf"
)

func init() {
	registry.ListenerRegistry().Register("relayx", NewListener)
}

var (
	errUnauthorized  = errors.New("relayx: unauthorized")
	errTokenExpired  = errors.New("relayx: token expired")
	errTokenMismatch = errors.New("relayx: token mismatch")
	errTokenReplay   = errors.New("relayx: token replay")
)

type relayxListener struct {
	addr    net.Addr
	server  *http.Server
	cqueue  chan net.Conn
	errChan chan error
	log     logger.Logger
	md      metadata
	options listener.Options

	authKey []byte

	replayMu         sync.Mutex
	replayLastSec    int64
	replayWindow     int64
	replayMaxEntries int
	replayIndex      map[[24]byte]int64
	replayBuckets    []map[[24]byte]struct{}
}

func NewListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &relayxListener{
		log:     options.Logger,
		options: options,
	}
}

func (l *relayxListener) Init(m md.Metadata) error {
	if err := l.parseMetadata(m); err != nil {
		return err
	}
	if err := l.deriveAuthKey(); err != nil {
		return err
	}
	l.initReplayCache()

	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(l.serveHTTP))

	l.server = &http.Server{
		Addr:              l.options.Addr,
		Handler:           mux,
		ReadHeaderTimeout: l.md.readTimeout,
		MaxHeaderBytes:    l.md.maxHeaderBytes,
	}
	l.cqueue = make(chan net.Conn, l.md.backlog)
	l.errChan = make(chan error, 1)

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
	ln = metrics.WrapListener(l.options.Service, ln)
	ln = stats.WrapListener(ln, l.options.Stats)
	ln = admission.WrapListener(l.options.Service, l.options.Admission, ln)
	ln = limiter_wrapper.WrapListener(l.options.Service, ln, l.options.TrafficLimiter)
	ln = climiter.WrapListener(l.options.ConnLimiter, ln)
	ln = xtls.NewListener(ln, l.options.TLSConfig)

	l.addr = ln.Addr()
	go func() {
		err := l.server.Serve(ln)
		if err != nil {
			l.errChan <- err
		}
		close(l.errChan)
	}()
	return nil
}

func (l *relayxListener) Accept() (conn net.Conn, err error) {
	var ok bool
	select {
	case conn = <-l.cqueue:
		conn = limiter_wrapper.WrapConn(
			conn,
			l.options.TrafficLimiter,
			conn.RemoteAddr().String(),
			limiter.ScopeOption(limiter.ScopeConn),
			limiter.ServiceOption(l.options.Service),
			limiter.NetworkOption(conn.LocalAddr().Network()),
			limiter.SrcOption(conn.RemoteAddr().String()),
		)
	case err, ok = <-l.errChan:
		if !ok {
			err = listener.ErrClosed
		}
	}
	return
}

func (l *relayxListener) Addr() net.Addr { return l.addr }

func (l *relayxListener) Close() error { return l.server.Close() }

func (l *relayxListener) serveHTTP(w http.ResponseWriter, r *http.Request) {
	clientIP := xhttp.GetClientIP(r)
	cip := ""
	if clientIP != nil {
		cip = clientIP.String()
	}
	log := l.log.WithFields(map[string]any{
		"local":  l.addr.String(),
		"remote": r.RemoteAddr,
		"client": cip,
	})
	if log.IsLevelEnabled(logger.TraceLevel) {
		dump, _ := httputil.DumpRequest(r, false)
		log.Trace(string(dump))
	}

	if r.Method != http.MethodPost {
		l.serveDecoy(w)
		return
	}
	if l.md.path != "" && r.URL.Path != l.md.path {
		l.serveDecoy(w)
		return
	}
	if err := l.validateToken(r.Header.Get("Authorization")); err != nil {
		log.WithFields(map[string]any{"reason": err.Error()}).Warn("probe resistance: unauthenticated request")
		l.serveDecoy(w)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	conn, rw, err := hj.Hijack()
	if err != nil {
		log.Error(err)
		return
	}

	if _, err := rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nCache-Control: no-store\r\nServer: nginx/1.27.4\r\n\r\n"); err != nil {
		conn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return
	}

	ctx := context.WithoutCancel(r.Context())
	if cc, ok := conn.(xctx.Context); ok && cc.Context() != nil {
		ctx = cc.Context()
	}
	if clientIP != nil {
		ctx = xctx.ContextWithSrcAddr(ctx, &net.TCPAddr{IP: clientIP})
	}

	c := &contextConn{
		Conn: &connWithBufReader{
			Conn: conn,
			br:   rw.Reader,
		},
		ctx: ctx,
	}

	select {
	case l.cqueue <- c:
	default:
		c.Close()
		log.Warnf("connection queue is full, client %s discarded", c.RemoteAddr())
	}
}

func (l *relayxListener) deriveAuthKey() error {
	l.authKey = make([]byte, 32)
	r := hkdf.New(sha256.New, []byte(l.md.key), nil, []byte("relayx-auth-v1"))
	if _, err := io.ReadFull(r, l.authKey); err != nil {
		return fmt.Errorf("relayx: derive authKey: %w", err)
	}
	return nil
}

func (l *relayxListener) validateToken(authHeader string) error {
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return errUnauthorized
	}
	raw, err := base64.RawURLEncoding.DecodeString(authHeader[7:])
	if err != nil || len(raw) != 56 {
		return errUnauthorized
	}

	nonce := raw[:16]
	tsBuf := raw[16:24]
	mac := raw[24:56]

	ts := binary.BigEndian.Uint64(tsBuf)
	now := uint64(time.Now().Unix())
	windowSec := uint64(l.md.replayWindow / time.Second)
	if windowSec == 0 {
		windowSec = 300
	}
	if ts+windowSec < now || ts > now+windowSec {
		return errTokenExpired
	}

	h := hmac.New(sha256.New, l.authKey)
	h.Write(nonce)
	h.Write(tsBuf)
	if !hmac.Equal(mac, h.Sum(nil)) {
		return errTokenMismatch
	}
	if !l.rememberToken(raw[:24], int64(ts)+int64(windowSec)) {
		return errTokenReplay
	}
	return nil
}

func (l *relayxListener) rememberToken(tokenID []byte, expiresAt int64) bool {
	now := time.Now().Unix()
	var key [24]byte
	copy(key[:], tokenID)

	l.replayMu.Lock()
	defer l.replayMu.Unlock()

	l.advanceReplayLocked(now)
	if _, ok := l.replayIndex[key]; ok {
		return false
	}
	if l.replayMaxEntries > 0 && len(l.replayIndex) >= l.replayMaxEntries {
		return false
	}
	if expiresAt <= now {
		expiresAt = now + l.replayWindow
	}
	idx := int(expiresAt % int64(len(l.replayBuckets)))
	bucket := l.replayBuckets[idx]
	if bucket == nil {
		bucket = make(map[[24]byte]struct{})
		l.replayBuckets[idx] = bucket
	}
	bucket[key] = struct{}{}
	l.replayIndex[key] = expiresAt
	return true
}

func (l *relayxListener) initReplayCache() {
	window := int64(l.md.replayWindow / time.Second)
	if window <= 0 {
		window = 300
	}
	l.replayWindow = window
	l.replayMaxEntries = l.md.maxReplayEntries
	if l.replayMaxEntries <= 0 {
		l.replayMaxEntries = 100000
	}
	l.replayIndex = make(map[[24]byte]int64)
	l.replayBuckets = make([]map[[24]byte]struct{}, window+1)
	l.replayLastSec = time.Now().Unix()
}

func (l *relayxListener) advanceReplayLocked(now int64) {
	if now <= l.replayLastSec {
		return
	}
	steps := now - l.replayLastSec
	if steps >= int64(len(l.replayBuckets)) {
		clear(l.replayIndex)
		clear(l.replayBuckets)
		l.replayLastSec = now
		return
	}
	for sec := l.replayLastSec + 1; sec <= now; sec++ {
		idx := int(sec % int64(len(l.replayBuckets)))
		bucket := l.replayBuckets[idx]
		for key := range bucket {
			delete(l.replayIndex, key)
		}
		clear(bucket)
		l.replayBuckets[idx] = nil
	}
	l.replayLastSec = now
}

func (l *relayxListener) serveDecoy(w http.ResponseWriter) {
	body := l.md.decoyBody
	if body == "" {
		body = defaultDecoyHTML
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Server", "nginx/1.27.4")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

type connWithBufReader struct {
	net.Conn
	br *bufio.Reader
}

func (c *connWithBufReader) Read(b []byte) (int, error) {
	if c.br != nil {
		if c.br.Buffered() > 0 {
			return c.br.Read(b)
		}
		c.br = nil
	}
	return c.Conn.Read(b)
}

type contextConn struct {
	net.Conn
	ctx context.Context
}

func (c *contextConn) Context() context.Context { return c.ctx }

const defaultDecoyHTML = `<!DOCTYPE html>
<html>
<head><title>Welcome to nginx!</title></head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>
<p><em>Thank you for using nginx.</em></p>
</body>
</html>`
