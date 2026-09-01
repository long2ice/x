package wrapper

import (
	"net"
	"sync/atomic"
	"testing"
)

type countingLimiter struct {
	current atomic.Int64
}

func (l *countingLimiter) Allow(n int) bool {
	l.current.Add(int64(n))
	return true
}

func (l *countingLimiter) Limit() int { return 1 }

func TestServerConnCloseReleasesOnce(t *testing.T) {
	lim := &countingLimiter{}
	lim.current.Store(1)

	c1, c2 := net.Pipe()
	t.Cleanup(func() { c2.Close() })
	c := WrapConn(lim, c1)

	_ = c.Close()
	_ = c.Close()

	if got := lim.current.Load(); got != 0 {
		t.Fatalf("limiter count = %d, want 0", got)
	}
}
