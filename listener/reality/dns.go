package reality

import (
	"context"
	"net"
	"sync"
	"time"
)

// destDNSTTL is how long a resolved dest address is trusted before a refresh
// is triggered. Refreshes happen in the background, so this only bounds how
// stale the cached address may get, never how long a dial waits.
const destDNSTTL = 30 * time.Second

// cachedResolver keeps the dest host's addresses cached so the REALITY
// handshake never pays for a DNS lookup on the hot path. reality.Server dials
// the dest for every single inbound connection; under a connection storm the
// per-connection lookups of a domain dest (e.g. www.apple.com) saturate the
// local resolver, the lookups start queuing, and handshakes stall for seconds
// even though the dest itself is reachable. Resolving at most once per TTL and
// serving the cached value in between keeps that off the handshake path.
type cachedResolver struct {
	base *net.Resolver
	ttl  time.Duration

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	addrs      []string
	expires    time.Time
	refreshing bool
}

func newCachedResolver(ttl time.Duration) *cachedResolver {
	return &cachedResolver{
		base:    net.DefaultResolver,
		ttl:     ttl,
		entries: make(map[string]*cacheEntry),
	}
}

// lookupHost returns the cached addresses for host, resolving synchronously
// only on the very first request (a cold cache). Once an entry exists every
// caller gets it immediately; an expired entry is served as-is while a single
// background goroutine refreshes it.
func (r *cachedResolver) lookupHost(ctx context.Context, host string) ([]string, error) {
	now := time.Now()

	r.mu.Lock()
	if e, ok := r.entries[host]; ok {
		if now.After(e.expires) && !e.refreshing {
			e.refreshing = true
			go r.refresh(host)
		}
		addrs := e.addrs
		r.mu.Unlock()
		return addrs, nil
	}
	r.mu.Unlock()

	addrs, err := r.base.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.entries[host] = &cacheEntry{addrs: addrs, expires: now.Add(r.ttl)}
	r.mu.Unlock()
	return addrs, nil
}

func (r *cachedResolver) refresh(host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := r.base.LookupHost(ctx, host)

	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[host]
	if e == nil {
		return
	}
	e.refreshing = false
	// On failure keep serving the previous addresses; just back off so the
	// next request retries after another TTL instead of on every dial.
	e.expires = time.Now().Add(r.ttl)
	if err == nil && len(addrs) > 0 {
		e.addrs = addrs
	}
}

// dial resolves addr through the cache (when its host is a domain) and dials
// the resulting IPs in order, falling back to a direct dial if anything about
// the cache path does not apply.
func (r *cachedResolver) dial(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.DialContext(ctx, network, addr)
	}
	if net.ParseIP(host) != nil {
		return d.DialContext(ctx, network, addr)
	}

	addrs, err := r.lookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return d.DialContext(ctx, network, addr)
	}

	var lastErr error
	for _, ip := range addrs {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
