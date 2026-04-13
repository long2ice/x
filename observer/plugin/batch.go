package observer

import (
	"context"
	"io"
	"math"
	"sync"
	"time"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/observer"
)

// BatchObserver wraps an underlying Observer and batches events before sending.
// Multiple services/handlers submit events independently; the batch observer
// aggregates them and flushes periodically in a single call, reducing the
// number of HTTP/gRPC round-trips from O(N) to O(1) per flush interval.
type BatchObserver struct {
	observer observer.Observer

	mu     sync.Mutex
	events []observer.Event

	flushInterval time.Duration
	maxBatchSize  int
	maxQueueSize  int

	// exponential backoff state
	backoff      time.Duration
	maxBackoff   time.Duration
	flushTimeout time.Duration

	cancel context.CancelFunc
	done   chan struct{}
	log    logger.Logger
}

type BatchOption func(*BatchObserver)

func BatchFlushIntervalOption(d time.Duration) BatchOption {
	return func(b *BatchObserver) {
		if d > 0 {
			b.flushInterval = d
		}
	}
}

func BatchMaxSizeOption(n int) BatchOption {
	return func(b *BatchObserver) {
		if n > 0 {
			b.maxBatchSize = n
		}
	}
}

func BatchMaxQueueOption(n int) BatchOption {
	return func(b *BatchObserver) {
		if n > 0 {
			b.maxQueueSize = n
		}
	}
}

func BatchFlushTimeoutOption(d time.Duration) BatchOption {
	return func(b *BatchObserver) {
		if d > 0 {
			b.flushTimeout = d
		}
	}
}

func BatchLoggerOption(log logger.Logger) BatchOption {
	return func(b *BatchObserver) {
		b.log = log
	}
}

// NewBatchObserver creates a BatchObserver that wraps the given observer.
// It starts a background goroutine to flush accumulated events periodically.
func NewBatchObserver(obs observer.Observer, opts ...BatchOption) *BatchObserver {
	b := &BatchObserver{
		observer:      obs,
		flushInterval: 5 * time.Second,
		maxBatchSize:  1000,
		maxQueueSize:  10000,
		maxBackoff:    60 * time.Second,
		flushTimeout:  5 * time.Second,
		done:          make(chan struct{}),
		log:           logger.Default(),
	}
	for _, opt := range opts {
		opt(b)
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	go b.run(ctx)
	return b
}

// Observe queues events for batched sending. This is safe for concurrent use.
func (b *BatchObserver) Observe(ctx context.Context, events []observer.Event, opts ...observer.Option) error {
	b.mu.Lock()
	b.events = append(b.events, events...)
	b.trimQueueLocked()
	b.mu.Unlock()
	return nil
}

func (b *BatchObserver) run(ctx context.Context) {
	defer close(b.done)

	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.flush(ctx)
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), b.flushTimeout)
			b.flush(flushCtx)
			cancel()
			return
		}
	}
}

func (b *BatchObserver) flush(ctx context.Context) {
	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return
	}
	events := b.events
	b.events = nil
	b.mu.Unlock()

	// Send in chunks to avoid overly large payloads
	for i := 0; i < len(events); i += b.maxBatchSize {
		end := i + b.maxBatchSize
		if end > len(events) {
			end = len(events)
		}
		chunk := events[i:end]

		if err := b.observer.Observe(ctx, chunk); err != nil {
			if b.log != nil {
				b.log.Warnf("batch observer flush failed (%d events): %v", len(chunk), err)
			}

			// Put unsent events back and apply backoff
			b.mu.Lock()
			remaining := make([]observer.Event, 0, len(events[i:])+len(b.events))
			remaining = append(remaining, events[i:]...)
			remaining = append(remaining, b.events...)
			b.events = remaining
			b.trimQueueLocked()
			b.mu.Unlock()

			b.applyBackoff(ctx)
			return
		}
	}

	// Reset backoff on success
	b.backoff = 0
}

func (b *BatchObserver) trimQueueLocked() {
	if b.maxQueueSize <= 0 || len(b.events) <= b.maxQueueSize {
		return
	}

	dropped := len(b.events) - b.maxQueueSize
	b.events = append([]observer.Event(nil), b.events[dropped:]...)
	if b.log != nil {
		b.log.Warnf("batch observer queue full, dropped %d oldest events", dropped)
	}
}

func (b *BatchObserver) applyBackoff(ctx context.Context) {
	if b.backoff == 0 {
		b.backoff = time.Second
	} else {
		b.backoff = time.Duration(math.Min(
			float64(b.backoff*2),
			float64(b.maxBackoff),
		))
	}

	if b.log != nil {
		b.log.Warnf("batch observer backing off for %v", b.backoff)
	}
	timer := time.NewTimer(b.backoff)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Close stops the background flush goroutine and performs a final flush.
func (b *BatchObserver) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	<-b.done
	if closer, ok := b.observer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
