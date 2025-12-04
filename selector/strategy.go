package selector

import (
	"context"
	"hash/crc32"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/metadata"
	"github.com/go-gost/core/selector"
	xctx "github.com/go-gost/x/ctx"
	mdutil "github.com/go-gost/x/metadata/util"
)

type Connectable interface {
	ActiveConns() int64
}

type LatencyStater interface {
	Latency() time.Duration
}

type roundRobinStrategy[T any] struct {
	counter uint64
}

// RoundRobinStrategy is a strategy for node selector.
// The node will be selected by round-robin algorithm.
func RoundRobinStrategy[T any]() selector.Strategy[T] {
	return &roundRobinStrategy[T]{}
}

func (s *roundRobinStrategy[T]) Apply(ctx context.Context, vs ...T) (v T) {
	if len(vs) == 0 {
		return
	}

	n := atomic.AddUint64(&s.counter, 1) - 1
	return vs[int(n%uint64(len(vs)))]
}

type randomStrategy[T any] struct {
	rw *RandomWeighted[T]
	mu sync.Mutex
}

// RandomStrategy is a strategy for node selector.
// The node will be selected randomly.
func RandomStrategy[T any]() selector.Strategy[T] {
	return &randomStrategy[T]{
		rw: NewRandomWeighted[T](),
	}
}

func (s *randomStrategy[T]) Apply(ctx context.Context, vs ...T) (v T) {
	if len(vs) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rw.Reset()
	for i := range vs {
		weight := 0
		if md, _ := any(vs[i]).(metadata.Metadatable); md != nil {
			weight = mdutil.GetInt(md.Metadata(), labelWeight)
		}
		if weight <= 0 {
			weight = 1
		}
		s.rw.Add(vs[i], weight)
	}

	return s.rw.Next()
}

type fifoStrategy[T any] struct{}

// FIFOStrategy is a strategy for node selector.
// The node will be selected from first to last,
// and will stick to the selected node until it is failed.
func FIFOStrategy[T any]() selector.Strategy[T] {
	return &fifoStrategy[T]{}
}

// Apply applies the fifo strategy for the nodes.
func (s *fifoStrategy[T]) Apply(ctx context.Context, vs ...T) (v T) {
	if len(vs) == 0 {
		return
	}
	return vs[0]
}

type hashStrategy[T any] struct {
	r  *rand.Rand
	mu sync.Mutex
}

func HashStrategy[T any]() selector.Strategy[T] {
	return &hashStrategy[T]{
		r: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *hashStrategy[T]) Apply(ctx context.Context, vs ...T) (v T) {
	if len(vs) == 0 {
		return
	}
	if h := xctx.HashFromContext(ctx); h != nil {
		value := uint64(crc32.ChecksumIEEE([]byte(h.Source)))
		logger.Default().Tracef("hash %s %d", h.Source, value)
		return vs[value%uint64(len(vs))]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return vs[s.r.Intn(len(vs))]
}

type leastConnStrategy[T any] struct {
	r  *rand.Rand
	mu sync.Mutex
}

func LeastConnStrategy[T any]() selector.Strategy[T] {
	return &leastConnStrategy[T]{
		r: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *leastConnStrategy[T]) Apply(ctx context.Context, vs ...T) (v T) {
	if len(vs) == 0 {
		return
	}

	var minConns int64 = math.MaxInt64
	var candidates []T

	for _, item := range vs {
		var conns int64
		if c, ok := any(item).(Connectable); ok {
			conns = c.ActiveConns()
		}

		if conns < minConns {
			minConns = conns
			candidates = []T{item}
		} else if conns == minConns {
			candidates = append(candidates, item)
		}
	}

	if len(candidates) == 1 {
		return candidates[0]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return candidates[s.r.Intn(len(candidates))]
}

type leastLatencyStrategy[T any] struct {
	r  *rand.Rand
	mu sync.Mutex
}

func LeastLatencyStrategy[T any]() selector.Strategy[T] {
	return &leastLatencyStrategy[T]{
		r: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (s *leastLatencyStrategy[T]) Apply(ctx context.Context, vs ...T) (v T) {
	if len(vs) == 0 {
		return
	}

	var minLatency time.Duration = math.MaxInt64
	var candidates []T

	for _, item := range vs {
		var latency time.Duration = math.MaxInt64

		if ls, ok := any(item).(LatencyStater); ok {
			latency = ls.Latency()
			if latency <= 0 {
				latency = math.MaxInt64
			}
		}

		if latency < minLatency {
			minLatency = latency
			candidates = []T{item}
		} else if latency == minLatency {
			candidates = append(candidates, item)
		}
	}

	if len(candidates) == 1 {
		return candidates[0]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return candidates[s.r.Intn(len(candidates))]
}
