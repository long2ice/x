package wrapper

import (
	"sync"

	limiter "github.com/go-gost/core/limiter/conn"
)

// ClientCounter can report the number of unique client IPs with active connections.
type ClientCounter interface {
	ClientCount() int
}

// ipCountConnLimiter wraps a ConnLimiter to add a limit on the number of
// unique client IPs that may hold active connections simultaneously.
type ipCountConnLimiter struct {
	inner      limiter.ConnLimiter
	maxClients int
	mu         sync.Mutex
	ipConns    map[string]int // IP -> active connection count
}

// NewIPCountConnLimiter wraps inner (which may be nil) with a max unique IP limit.
// If maxClients <= 0, the inner limiter is returned unchanged.
func NewIPCountConnLimiter(inner limiter.ConnLimiter, maxClients int) limiter.ConnLimiter {
	return &ipCountConnLimiter{
		inner:      inner,
		maxClients: maxClients,
		ipConns:    make(map[string]int),
	}
}

func (l *ipCountConnLimiter) ClientCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ipConns)
}

func (l *ipCountConnLimiter) ClientIPs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ips := make([]string, 0, len(l.ipConns))
	for ip := range l.ipConns {
		ips = append(ips, ip)
	}
	return ips
}

func (l *ipCountConnLimiter) Limiter(key string) limiter.Limiter {
	var innerLim limiter.Limiter
	if l.inner != nil {
		innerLim = l.inner.Limiter(key)
	}

	return &ipTrackingLimiter{
		inner:  innerLim,
		key:    key,
		parent: l,
	}
}

type ipTrackingLimiter struct {
	inner  limiter.Limiter
	key    string
	parent *ipCountConnLimiter
}

func (l *ipTrackingLimiter) Allow(n int) bool {
	if n > 0 {
		// Check inner limiter first
		if l.inner != nil && !l.inner.Allow(n) {
			return false
		}

		l.parent.mu.Lock()
		if l.parent.maxClients > 0 && l.parent.ipConns[l.key] == 0 && len(l.parent.ipConns) >= l.parent.maxClients {
			// New IP and we've hit the limit
			l.parent.mu.Unlock()
			// Rollback inner limiter
			if l.inner != nil {
				l.inner.Allow(-n)
			}
			return false
		}
		l.parent.ipConns[l.key] += n
		l.parent.mu.Unlock()
		return true
	}

	// n <= 0: releasing connections
	if l.inner != nil {
		l.inner.Allow(n)
	}

	l.parent.mu.Lock()
	l.parent.ipConns[l.key] += n
	if l.parent.ipConns[l.key] <= 0 {
		delete(l.parent.ipConns, l.key)
	}
	l.parent.mu.Unlock()
	return true
}

func (l *ipTrackingLimiter) Limit() int {
	if l.inner != nil {
		return l.inner.Limit()
	}
	return l.parent.maxClients
}
