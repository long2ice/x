package service

import (
	"context"
	"sync"
	"time"

	"github.com/go-gost/core/observer"
	"github.com/go-gost/core/observer/stats"
	xstats "github.com/go-gost/x/observer/stats"
)

// observeReg centralizes per-service stats emission.  A single goroutine
// ticks at 1s resolution and fires each service's observeStats callback
// when its configured period has elapsed.  This replaces the one-goroutine-
// per-service pattern that otherwise runs 20k tickers at 20k services.
type observeReg struct {
	mu      sync.Mutex
	entries map[*defaultService]*observeEntry
	once    sync.Once
}

type observeEntry struct {
	svc      *defaultService
	period   time.Duration
	nextFire time.Time
	ctx      context.Context
}

var globalObserveRegistry = &observeReg{entries: make(map[*defaultService]*observeEntry)}

func (r *observeReg) add(s *defaultService, ctx context.Context) {
	d := s.options.observerPeriod
	if d == 0 {
		d = 5 * time.Second
	}
	if d < time.Second {
		d = 1 * time.Second
	}
	entry := &observeEntry{
		svc:      s,
		period:   d,
		nextFire: time.Now().Add(d),
		ctx:      ctx,
	}
	r.mu.Lock()
	r.entries[s] = entry
	r.mu.Unlock()
	r.once.Do(r.start)
}

func (r *observeReg) remove(s *defaultService) {
	r.mu.Lock()
	delete(r.entries, s)
	r.mu.Unlock()
}

func (r *observeReg) start() {
	ticker := time.NewTicker(time.Second)
	go func() {
		for now := range ticker.C {
			r.tick(now)
		}
	}()
}

func (r *observeReg) tick(now time.Time) {
	r.mu.Lock()
	due := make([]*observeEntry, 0, len(r.entries))
	for _, e := range r.entries {
		if !now.Before(e.nextFire) {
			due = append(due, e)
			e.nextFire = now.Add(e.period)
		}
	}
	r.mu.Unlock()

	for _, e := range due {
		if e.ctx.Err() != nil {
			r.remove(e.svc)
			continue
		}
		emitStats(e.ctx, e.svc)
	}
}

func emitStats(ctx context.Context, s *defaultService) {
	if s.options.observer == nil {
		return
	}
	st := s.status.Stats()
	if st == nil || !st.IsUpdated() {
		return
	}

	ev := xstats.StatsEvent{
		Kind:         "service",
		Service:      s.name,
		TotalConns:   st.Get(stats.KindTotalConns),
		CurrentConns: st.Get(stats.KindCurrentConns),
		InputBytes:   st.Get(stats.KindInputBytes),
		OutputBytes:  st.Get(stats.KindOutputBytes),
		TotalErrs:    st.Get(stats.KindTotalErrs),
	}
	if s.options.clientCounter != nil {
		ev.CurrentClients = s.options.clientCounter.ClientIPs()
	}
	s.options.observer.Observe(ctx, []observer.Event{ev})
}
