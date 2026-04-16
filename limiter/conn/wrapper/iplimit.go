package wrapper

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	limiter "github.com/go-gost/core/limiter/conn"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/internal/loader"
	xlogger "github.com/go-gost/x/logger"
)

// ClientCounter can report the number of unique client IPs with active connections.
type ClientCounter interface {
	ClientCount() int
	ClientIPs() []string
}

type options struct {
	limits      []string
	fileLoader  loader.Loader
	redisLoader loader.Loader
	httpLoader  loader.Loader
	period      time.Duration
	logger      logger.Logger
}

type Option func(opts *options)

func LimitsOption(limits ...string) Option {
	return func(opts *options) {
		opts.limits = limits
	}
}

func ReloadPeriodOption(period time.Duration) Option {
	return func(opts *options) {
		opts.period = period
	}
}

func FileLoaderOption(fileLoader loader.Loader) Option {
	return func(opts *options) {
		opts.fileLoader = fileLoader
	}
}

func RedisLoaderOption(redisLoader loader.Loader) Option {
	return func(opts *options) {
		opts.redisLoader = redisLoader
	}
}

func HTTPLoaderOption(httpLoader loader.Loader) Option {
	return func(opts *options) {
		opts.httpLoader = httpLoader
	}
}

func LoggerOption(logger logger.Logger) Option {
	return func(opts *options) {
		opts.logger = logger
	}
}

// clientLimiter limits the number of unique client IPs that may hold
// active connections simultaneously. It implements conn.ConnLimiter.
type clientLimiter struct {
	maxClients int
	mu         sync.Mutex
	ipConns    map[string]int // IP -> active connection count
	cancelFunc context.CancelFunc
	options    options
	logger     logger.Logger
}

func NewClientLimiter(opts ...Option) limiter.ConnLimiter {
	var options options
	for _, opt := range opts {
		opt(&options)
	}

	ctx, cancel := context.WithCancel(context.TODO())
	lim := &clientLimiter{
		ipConns:    make(map[string]int),
		options:    options,
		cancelFunc: cancel,
		logger:     options.logger,
	}
	if lim.logger == nil {
		lim.logger = xlogger.Nop()
	}

	go lim.periodReload(ctx)

	return lim
}

func (l *clientLimiter) ClientCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ipConns)
}

func (l *clientLimiter) ClientIPs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ips := make([]string, 0, len(l.ipConns))
	for ip := range l.ipConns {
		ips = append(ips, ip)
	}
	return ips
}

func (l *clientLimiter) Limiter(key string) limiter.Limiter {
	return &ipTrackingLimiter{
		key:    key,
		parent: l,
	}
}

func (l *clientLimiter) Close() error {
	l.cancelFunc()
	if l.options.fileLoader != nil {
		l.options.fileLoader.Close()
	}
	if l.options.redisLoader != nil {
		l.options.redisLoader.Close()
	}
	return nil
}

func (l *clientLimiter) periodReload(ctx context.Context) error {
	if err := l.reload(ctx); err != nil {
		l.logger.Warnf("reload: %v", err)
	}

	period := l.options.period
	if period <= 0 {
		return nil
	}
	if period < time.Second {
		period = time.Second
	}

	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := l.reload(ctx); err != nil {
				l.logger.Warnf("reload: %v", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (l *clientLimiter) reload(ctx context.Context) error {
	v, err := l.load(ctx)
	if err != nil {
		return err
	}

	lines := append(l.options.limits, v...)

	var maxClients int
	for _, s := range lines {
		key, limit := l.parseLimit(s)
		if key == "" || limit <= 0 {
			continue
		}
		if key == "$" {
			maxClients = limit
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.maxClients = maxClients

	return nil
}

func (l *clientLimiter) load(ctx context.Context) (patterns []string, err error) {
	if l.options.fileLoader != nil {
		if lister, ok := l.options.fileLoader.(loader.Lister); ok {
			list, er := lister.List(ctx)
			if er != nil {
				l.logger.Warnf("file loader: %v", er)
			}
			for _, s := range list {
				if line := l.parseLine(s); line != "" {
					patterns = append(patterns, line)
				}
			}
		} else {
			r, er := l.options.fileLoader.Load(ctx)
			if er != nil {
				l.logger.Warnf("file loader: %v", er)
			}
			if v, _ := l.parsePatterns(r); v != nil {
				patterns = append(patterns, v...)
			}
		}
	}
	if l.options.redisLoader != nil {
		if lister, ok := l.options.redisLoader.(loader.Lister); ok {
			list, er := lister.List(ctx)
			if er != nil {
				l.logger.Warnf("redis loader: %v", er)
			}
			patterns = append(patterns, list...)
		} else {
			r, er := l.options.redisLoader.Load(ctx)
			if er != nil {
				l.logger.Warnf("redis loader: %v", er)
			}
			if v, _ := l.parsePatterns(r); v != nil {
				patterns = append(patterns, v...)
			}
		}
	}
	if l.options.httpLoader != nil {
		r, er := l.options.httpLoader.Load(ctx)
		if er != nil {
			l.logger.Warnf("http loader: %v", er)
		}
		if v, _ := l.parsePatterns(r); v != nil {
			patterns = append(patterns, v...)
		}
	}

	l.logger.Debugf("load items %d", len(patterns))
	return
}

func (l *clientLimiter) parsePatterns(r io.Reader) (patterns []string, err error) {
	if r == nil {
		return
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if line := l.parseLine(scanner.Text()); line != "" {
			patterns = append(patterns, line)
		}
	}

	err = scanner.Err()
	return
}

func (l *clientLimiter) parseLine(s string) string {
	if n := strings.IndexByte(s, '#'); n >= 0 {
		s = s[:n]
	}
	return strings.TrimSpace(s)
}

func (l *clientLimiter) parseLimit(s string) (key string, limit int) {
	s = strings.Replace(s, "\t", " ", -1)
	s = strings.TrimSpace(s)
	var ss []string
	for _, v := range strings.Split(s, " ") {
		if v != "" {
			ss = append(ss, v)
		}
	}
	if len(ss) < 2 {
		return
	}

	key = ss[0]
	limit, _ = strconv.Atoi(ss[1])

	return
}

type ipTrackingLimiter struct {
	key    string
	parent *clientLimiter
}

func (l *ipTrackingLimiter) Allow(n int) bool {
	if n > 0 {
		l.parent.mu.Lock()
		if l.parent.maxClients > 0 && l.parent.ipConns[l.key] == 0 && len(l.parent.ipConns) >= l.parent.maxClients {
			l.parent.mu.Unlock()
			return false
		}
		l.parent.ipConns[l.key] += n
		l.parent.mu.Unlock()
		return true
	}

	// n <= 0: releasing connections
	l.parent.mu.Lock()
	l.parent.ipConns[l.key] += n
	if l.parent.ipConns[l.key] <= 0 {
		delete(l.parent.ipConns, l.key)
	}
	l.parent.mu.Unlock()
	return true
}

func (l *ipTrackingLimiter) Limit() int {
	return l.parent.maxClients
}

// compositeConnLimiter combines a regular ConnLimiter with a client (IP count) limiter.
type compositeConnLimiter struct {
	inner  limiter.ConnLimiter
	client limiter.ConnLimiter
}

// NewCompositeConnLimiter wraps inner and client limiters together.
// Either may be nil.
func NewCompositeConnLimiter(inner, client limiter.ConnLimiter) limiter.ConnLimiter {
	if inner == nil && client == nil {
		return nil
	}
	if inner == nil {
		return client
	}
	if client == nil {
		return inner
	}
	return &compositeConnLimiter{inner: inner, client: client}
}

func (l *compositeConnLimiter) Limiter(key string) limiter.Limiter {
	innerLim := l.inner.Limiter(key)
	clientLim := l.client.Limiter(key)

	if innerLim == nil {
		return clientLim
	}
	if clientLim == nil {
		return innerLim
	}

	return &compositeLimiter{inner: innerLim, client: clientLim}
}

type compositeLimiter struct {
	inner  limiter.Limiter
	client limiter.Limiter
}

func (l *compositeLimiter) Allow(n int) bool {
	if n > 0 {
		if !l.inner.Allow(n) {
			return false
		}
		if !l.client.Allow(n) {
			// Rollback inner
			l.inner.Allow(-n)
			return false
		}
		return true
	}

	// n <= 0: releasing
	l.inner.Allow(n)
	l.client.Allow(n)
	return true
}

func (l *compositeLimiter) Limit() int {
	il := l.inner.Limit()
	cl := l.client.Limit()
	if il <= 0 {
		return cl
	}
	if cl <= 0 {
		return il
	}
	if il < cl {
		return il
	}
	return cl
}

// GetClientCounter extracts a ClientCounter from a ConnLimiter, if available.
func GetClientCounter(cl limiter.ConnLimiter) ClientCounter {
	if cc, ok := cl.(ClientCounter); ok {
		return cc
	}
	if comp, ok := cl.(*compositeConnLimiter); ok {
		if cc, ok := comp.client.(ClientCounter); ok {
			return cc
		}
	}
	return nil
}

// ipToKey extracts the IP string from a host:port or plain IP.
func ipToKey(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
