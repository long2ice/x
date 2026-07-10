package traffic

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	limiter "github.com/go-gost/core/limiter/traffic"
	"golang.org/x/time/rate"
)

// maxBurst caps the token-bucket capacity so idle periods cannot accumulate a
// large dump. 4KiB is ~0.3ms on a 100Mbps link — invisible on typical graphs
// while still avoiding per-byte Wait overhead.
const maxBurst = 4 << 10

type llimiter struct {
	limiter *rate.Limiter
}

func NewLimiter(r int) limiter.Limiter {
	return &llimiter{
		limiter: rate.NewLimiter(rate.Limit(r), burstFor(r)),
	}
}

// burstFor returns a minimal burst (~1ms of traffic, capped) so the limiter
// paces instead of allowing a full-second dump.
func burstFor(r int) int {
	if r <= 0 {
		return 1
	}
	b := r / 1000
	if b < 1 {
		b = 1
	}
	if b > maxBurst {
		b = maxBurst
	}
	return b
}

// Wait grants at most one burst worth of bytes, blocking until those tokens
// are available. Callers that need a larger transfer (stream Write, UDP
// datagrams) must loop. Capping each grant keeps instantaneous spikes tiny.
func (l *llimiter) Wait(ctx context.Context, n int) int {
	if n <= 0 {
		return 0
	}
	if burst := l.limiter.Burst(); n > burst {
		n = burst
	}
	if err := l.limiter.WaitN(ctx, n); err != nil {
		return 0
	}
	return n
}

func (l *llimiter) Limit() int {
	return int(l.limiter.Limit())
}

func (l *llimiter) Set(n int) {
	l.limiter.SetLimit(rate.Limit(n))
	l.limiter.SetBurst(burstFor(n))
}

func (l *llimiter) String() string {
	return strconv.Itoa(int(l.limiter.Limit()))
}

type limiterGroup struct {
	limiters []limiter.Limiter
}

func newLimiterGroup(limiters ...limiter.Limiter) *limiterGroup {
	sort.Slice(limiters, func(i, j int) bool {
		return limiters[i].Limit() < limiters[j].Limit()
	})
	return &limiterGroup{limiters: limiters}
}

func (l *limiterGroup) Wait(ctx context.Context, n int) int {
	for i := range l.limiters {
		if v := l.limiters[i].Wait(ctx, n); v < n {
			n = v
		}
	}
	return n
}

func (l *limiterGroup) Limit() int {
	if len(l.limiters) == 0 {
		return 0
	}

	return l.limiters[0].Limit()
}

func (l *limiterGroup) Set(n int) {}

func (l *limiterGroup) String() string {
	return fmt.Sprintf("%v", l.limiters)
}
