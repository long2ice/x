package registry

import (
	"context"

	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/limiter/conn"
	"github.com/go-gost/core/limiter/rate"
	"github.com/go-gost/core/limiter/traffic"
)

type trafficLimiterRegistry struct {
	registry[traffic.TrafficLimiter]
}

func (r *trafficLimiterRegistry) Register(name string, v traffic.TrafficLimiter) error {
	return r.registry.Register(name, v)
}

func (r *trafficLimiterRegistry) Get(name string) traffic.TrafficLimiter {
	if name != "" {
		return &trafficLimiterWrapper{name: name, r: r}
	}
	return nil
}

func (r *trafficLimiterRegistry) get(name string) traffic.TrafficLimiter {
	return r.registry.Get(name)
}

type trafficLimiterWrapper struct {
	name string
	r    *trafficLimiterRegistry
}

func (w *trafficLimiterWrapper) In(ctx context.Context, key string, opts ...limiter.Option) traffic.Limiter {
	v := w.r.get(w.name)
	if v == nil {
		return nil
	}
	return v.In(ctx, key, opts...)
}

func (w *trafficLimiterWrapper) Out(ctx context.Context, key string, opts ...limiter.Option) traffic.Limiter {
	v := w.r.get(w.name)
	if v == nil {
		return nil
	}
	return v.Out(ctx, key, opts...)
}

type connLimiterRegistry struct {
	registry[conn.ConnLimiter]
}

func (r *connLimiterRegistry) Register(name string, v conn.ConnLimiter) error {
	return r.registry.Register(name, v)
}

func (r *connLimiterRegistry) Get(name string) conn.ConnLimiter {
	if name != "" {
		return &connLimiterWrapper{name: name, r: r}
	}
	return nil
}

func (r *connLimiterRegistry) get(name string) conn.ConnLimiter {
	return r.registry.Get(name)
}

type connLimiterWrapper struct {
	name string
	r    *connLimiterRegistry
}

func (w *connLimiterWrapper) Limiter(key string) conn.Limiter {
	v := w.r.get(w.name)
	if v == nil {
		return nil
	}
	return v.Limiter(key)
}

type clientLimiterRegistry struct {
	registry[conn.ConnLimiter]
}

func (r *clientLimiterRegistry) Register(name string, v conn.ConnLimiter) error {
	return r.registry.Register(name, v)
}

func (r *clientLimiterRegistry) Get(name string) conn.ConnLimiter {
	if name != "" {
		return &clientLimiterWrapper{name: name, r: r}
	}
	return nil
}

func (r *clientLimiterRegistry) get(name string) conn.ConnLimiter {
	return r.registry.Get(name)
}

type clientLimiterWrapper struct {
	name string
	r    *clientLimiterRegistry
}

func (w *clientLimiterWrapper) Limiter(key string) conn.Limiter {
	v := w.r.get(w.name)
	if v == nil {
		return nil
	}
	return v.Limiter(key)
}

func (w *clientLimiterWrapper) ClientCount() int {
	v := w.r.get(w.name)
	if v == nil {
		return 0
	}
	type clientCounter interface {
		ClientCount() int
	}
	if cc, ok := v.(clientCounter); ok {
		return cc.ClientCount()
	}
	return 0
}

func (w *clientLimiterWrapper) ClientIPs() []string {
	v := w.r.get(w.name)
	if v == nil {
		return nil
	}
	type clientIPer interface {
		ClientIPs() []string
	}
	if cc, ok := v.(clientIPer); ok {
		return cc.ClientIPs()
	}
	return nil
}

type rateLimiterRegistry struct {
	registry[rate.RateLimiter]
}

func (r *rateLimiterRegistry) Register(name string, v rate.RateLimiter) error {
	return r.registry.Register(name, v)
}

func (r *rateLimiterRegistry) Get(name string) rate.RateLimiter {
	if name != "" {
		return &rateLimiterWrapper{name: name, r: r}
	}
	return nil
}

func (r *rateLimiterRegistry) get(name string) rate.RateLimiter {
	return r.registry.Get(name)
}

type rateLimiterWrapper struct {
	name string
	r    *rateLimiterRegistry
}

func (w *rateLimiterWrapper) Limiter(key string) rate.Limiter {
	v := w.r.get(w.name)
	if v == nil {
		return nil
	}
	return v.Limiter(key)
}
