package traffic

import (
	"sync"
	"time"
)

// evictRegistry runs a single goroutine that periodically evicts expired
// entries from every registered trafficLimiter.  Before this existed each
// limiter had its own janitor/eviction goroutine; with 20k services that's
// 20k extra scheduler wakeups every cleanupInterval.
type evictRegistry struct {
	mu        sync.Mutex
	limiters  map[*trafficLimiter]struct{}
	ticker    *time.Ticker
	stopOnce  sync.Once
	startOnce sync.Once
}

var globalEvictRegistry = &evictRegistry{limiters: make(map[*trafficLimiter]struct{})}

func (r *evictRegistry) add(l *trafficLimiter) {
	r.mu.Lock()
	r.limiters[l] = struct{}{}
	r.mu.Unlock()
	r.startOnce.Do(r.start)
}

func (r *evictRegistry) remove(l *trafficLimiter) {
	r.mu.Lock()
	delete(r.limiters, l)
	r.mu.Unlock()
}

func (r *evictRegistry) start() {
	r.ticker = time.NewTicker(cleanupInterval)
	go func() {
		for range r.ticker.C {
			r.sweep()
		}
	}()
}

func (r *evictRegistry) sweep() {
	r.mu.Lock()
	limiters := make([]*trafficLimiter, 0, len(r.limiters))
	for l := range r.limiters {
		limiters = append(limiters, l)
	}
	r.mu.Unlock()
	for _, l := range limiters {
		l.evictExpired()
	}
}
