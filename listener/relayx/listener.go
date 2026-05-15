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
	"github.com/go-gost/x/internal/util/mux"
	xtls "github.com/go-gost/x/internal/util/tls"
	ws_util "github.com/go-gost/x/internal/util/ws"
	"github.com/go-gost/x/internal/util/wspad"
	climiter "github.com/go-gost/x/limiter/conn/wrapper"
	traffic_limiter "github.com/go-gost/x/limiter/traffic"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/hkdf"
)

const (
	tokenMuxFlagBit     byte = 0x80
	respMuxFlagBit      byte = 0x01
	minResponseBodySize      = 64
	maxResponseBodySize      = 512
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
	addr     net.Addr
	server   *http.Server
	upgrader *websocket.Upgrader
	cqueue   chan net.Conn
	errChan  chan error
	log      logger.Logger
	md       metadata
	options  listener.Options

	authKey []byte

	replayMu         sync.Mutex
	replayLastSec    int64
	replayWindow     int64
	replayMaxEntries int
	replayIndex      map[[24]byte]int64
	replayBuckets    []map[[24]byte]struct{}

	// tunnels tracks every hijacked websocket conn (mux or single-shot).
	// http.Server.Close does not touch hijacked conns, so we close them
	// ourselves on Close() to cut zombie mux sessions that would otherwise
	// keep clients pinned to a dead service after a config reload.
	tunnelsMu sync.Mutex
	tunnels   map[net.Conn]struct{}
	closed    bool
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

	l.upgrader = &websocket.Upgrader{
		HandshakeTimeout:  l.md.readTimeout,
		CheckOrigin:       func(*http.Request) bool { return true },
		EnableCompression: false,
	}

	sm := http.NewServeMux()
	sm.Handle("/", http.HandlerFunc(l.serveHTTP))

	l.server = &http.Server{
		Addr:              l.options.Addr,
		Handler:           sm,
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
	ln = admission.WrapListener(l.options.Service, l.options.Admission, ln)
	ln = xtls.NewListener(ln, l.options.TLSConfig)
	// NOTE: accounting and conn-limiter wrapping intentionally happen in
	// Accept() at the user-conn level (per smux stream / per WebSocket
	// tunnel), NOT at this TCP listener level. Wrapping here would count smux
	// session-level NOP keepalive frames as user traffic and keep the client IP
	// "active" after all user streams have closed.

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
		// Apply per-IP conn limiter at the user-conn level (per stream /
		// per single-shot tunnel). When the quota is exhausted, the conn is
		// closed; subsequent Read/Write by the handler will return an error.
		if l.options.ConnLimiter != nil {
			host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			if lim := l.options.ConnLimiter.Limiter(host); lim != nil {
				if lim.Allow(1) {
					conn = climiter.WrapConn(lim, conn)
				} else {
					conn.Close()
				}
			}
		}
		// Count user bytes only (smux NOP keepalive on the underlying TCP
		// session is excluded because it never reaches this layer).
		conn = metrics.WrapConn(l.options.Service, conn)
		conn = stats.WrapConn(conn, l.options.Stats)
		conn = limiter_wrapper.WrapConn(
			conn,
			l.options.TrafficLimiter,
			traffic_limiter.ServiceLimitKey,
			limiter.ScopeOption(limiter.ScopeService),
			limiter.ServiceOption(l.options.Service),
			limiter.NetworkOption(conn.LocalAddr().Network()),
		)
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

func (l *relayxListener) Close() error {
	// Forcibly close every hijacked tunnel before tearing down the http.Server,
	// so in-flight mux sessions cannot survive listener close. Without this,
	// clients keep complete confidence in their cached mux session (the TCP
	// stays ESTABLISHED, smux keepalives keep replying from the orphaned
	// serveMux goroutine) and never reconnect to the new listener.
	l.tunnelsMu.Lock()
	l.closed = true
	tunnels := l.tunnels
	l.tunnels = nil
	l.tunnelsMu.Unlock()
	for c := range tunnels {
		_ = c.Close()
	}
	return l.server.Close()
}

func (l *relayxListener) addTunnel(c net.Conn) bool {
	l.tunnelsMu.Lock()
	defer l.tunnelsMu.Unlock()
	if l.closed {
		return false
	}
	if l.tunnels == nil {
		l.tunnels = make(map[net.Conn]struct{})
	}
	l.tunnels[c] = struct{}{}
	return true
}

func (l *relayxListener) removeTunnel(c net.Conn) {
	l.tunnelsMu.Lock()
	defer l.tunnelsMu.Unlock()
	delete(l.tunnels, c)
}

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

	if !websocket.IsWebSocketUpgrade(r) {
		l.serveProbe(w, r)
		return
	}
	if l.md.path != "" && r.URL.Path != l.md.path {
		l.serveNotFound(w)
		return
	}
	muxRequested, err := l.validateToken(r.Header.Get("Authorization"))
	if err != nil {
		log.WithFields(map[string]any{"reason": err.Error()}).Warn("probe resistance: unauthenticated request")
		l.serveNotFound(w)
		return
	}

	useMux := l.md.mux && muxRequested

	respHeaders := http.Header{}
	respHeaders.Set("Server", l.md.serverHeader)

	wsConn, err := l.upgrader.Upgrade(w, r, respHeaders)
	if err != nil {
		log.Error(err)
		return
	}

	baseCtx := context.WithoutCancel(r.Context())
	if cc, ok := wsConn.NetConn().(xctx.Context); ok && cc.Context() != nil {
		baseCtx = cc.Context()
	}
	if clientIP != nil {
		baseCtx = xctx.ContextWithSrcAddr(baseCtx, &net.TCPAddr{IP: clientIP})
	}

	var tunnel net.Conn
	if l.md.wspad {
		// Listener side uses the light bucket set so server→client frames
		// carry minimal extra padding; the matching dialer uses heavy
		// buckets, skewing padding overhead to the client→server direction.
		tunnel = wspad.ListenerConn(ws_util.ContextConn(baseCtx, wsConn))
	} else {
		tunnel = ws_util.ContextConn(baseCtx, wsConn)
	}

	muxSignal := make([]byte, minResponseBodySize+randIntn(maxResponseBodySize-minResponseBodySize+1))
	if _, err := crand.Read(muxSignal); err != nil {
		tunnel.Close()
		return
	}
	if useMux {
		muxSignal[0] |= respMuxFlagBit
	} else {
		muxSignal[0] &^= respMuxFlagBit
	}
	if _, err := tunnel.Write(muxSignal); err != nil {
		tunnel.Close()
		return
	}

	// Register the hijacked tunnel so listener.Close() can break zombie mux
	// sessions; if the listener already closed in the gap between Upgrade and
	// here, drop the tunnel immediately.
	if !l.addTunnel(tunnel) {
		tunnel.Close()
		return
	}

	if useMux {
		go func() {
			defer l.removeTunnel(tunnel)
			l.serveMux(tunnel, baseCtx, log)
		}()
		return
	}

	c := &contextConn{
		Conn:     tunnel,
		ctx:      baseCtx,
		listener: l,
	}

	select {
	case l.cqueue <- c:
	default:
		c.Close()
		log.Warnf("connection queue is full, client %s discarded", c.RemoteAddr())
	}
}

func (l *relayxListener) serveMux(tunnel net.Conn, baseCtx context.Context, log logger.Logger) {
	defer tunnel.Close()

	session, err := mux.ServerSession(tunnel, l.md.muxCfg)
	if err != nil {
		log.Error(err)
		return
	}
	defer session.Close()

	for {
		stream, err := session.Accept()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Debugf("mux accept: %v", err)
			}
			return
		}
		c := &contextConn{Conn: stream, ctx: baseCtx}
		select {
		case l.cqueue <- c:
		default:
			c.Close()
			log.Warnf("connection queue is full, stream from %s discarded", tunnel.RemoteAddr())
		}
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

func (l *relayxListener) validateToken(authHeader string) (bool, error) {
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false, errUnauthorized
	}
	raw, err := base64.RawURLEncoding.DecodeString(authHeader[7:])
	if err != nil || len(raw) != 56 {
		return false, errUnauthorized
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
		return false, errTokenExpired
	}

	h := hmac.New(sha256.New, l.authKey)
	h.Write(nonce)
	h.Write(tsBuf)
	if !hmac.Equal(mac, h.Sum(nil)) {
		return false, errTokenMismatch
	}
	if !l.rememberToken(raw[:24], int64(ts)+int64(windowSec)) {
		return false, errTokenReplay
	}
	return nonce[0]&tokenMuxFlagBit != 0, nil
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

func (l *relayxListener) serveProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		l.serveDecoy(w)
		return
	}
	l.serveNotFound(w)
}

func (l *relayxListener) serveDecoy(w http.ResponseWriter) {
	body := l.md.decoyBody
	if body == "" {
		body = defaultDecoyHTML
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Server", l.md.serverHeader)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func (l *relayxListener) serveNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Server", l.md.serverHeader)
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notFoundHTML))
}

func randIntn(n int) int {
	if n <= 1 {
		return 0
	}
	return rand.IntN(n)
}

type contextConn struct {
	net.Conn
	ctx context.Context
	// listener, when non-nil, is notified on Close so the hijacked tunnel
	// it represents is removed from the listener's tracking set. Mux
	// substreams leave this nil — only the parent tunnel is tracked.
	listener *relayxListener
	once     sync.Once
}

func (c *contextConn) Context() context.Context { return c.ctx }

func (c *contextConn) Close() error {
	if c.listener != nil {
		c.once.Do(func() {
			c.listener.removeTunnel(c.Conn)
		})
	}
	return c.Conn.Close()
}

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

const notFoundHTML = `<html>
<head><title>404 Not Found</title></head>
<body>
<center><h1>404 Not Found</h1></center>
<hr><center>nginx</center>
</body>
</html>
`
