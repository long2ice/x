// Package accept separates socket acceptance from bounded connection setup.
package accept

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// Shared across listeners: hundreds of ports must not each admit an unbounded
// number of slow handshakes. Established connections do not consume these slots.
var setupSlots = make(chan struct{}, 1024)

type Config struct {
	AcceptLoops int
	MaxPending  int
	Timeout     time.Duration
	Prepare     func(context.Context, net.Conn) (net.Conn, error)
}

type Listener struct {
	net.Listener
	cfg     Config
	done    chan struct{}
	ready   chan *pending
	mu      sync.Mutex
	err     error
	pending map[*pending]struct{}
}

type pending struct {
	mu     sync.Mutex
	raw    net.Conn
	conn   net.Conn
	ctx    context.Context
	cancel context.CancelFunc
	taken  chan struct{}
	// detached transfers ownership to the caller of Accept. A timeout racing
	// with that transfer must never close an established connection.
	detached bool
	closed   bool
}

func NewListener(ln net.Listener, cfg Config) *Listener {
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 128
	}
	l := &Listener{Listener: ln, cfg: cfg, done: make(chan struct{}), ready: make(chan *pending), pending: make(map[*pending]struct{})}
	for range max(1, cfg.AcceptLoops) {
		go l.run()
	}
	return l
}

func (p *pending) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.detached && !p.closed {
		p.closed = true
		p.raw.Close()
	}
}

func (l *Listener) run() {
	var delay time.Duration
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			var ne net.Error
			if !errors.Is(err, net.ErrClosed) && errors.As(err, &ne) && ne.Temporary() {
				if delay == 0 {
					delay = 100 * time.Millisecond
				} else {
					delay = min(2*delay, 5*time.Second)
				}
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-l.done:
					timer.Stop()
					return
				}
				continue
			}
			l.stop(err)
			return
		}
		delay = 0
		l.mu.Lock()
		if l.err != nil {
			l.mu.Unlock()
			conn.Close()
			return
		}
		if len(l.pending) >= l.cfg.MaxPending {
			l.mu.Unlock()
			conn.Close()
			continue
		}
		select {
		case setupSlots <- struct{}{}:
		default:
			l.mu.Unlock()
			conn.Close()
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		if l.cfg.Timeout > 0 {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), l.cfg.Timeout)
		}
		p := &pending{raw: conn, ctx: ctx, cancel: cancel, taken: make(chan struct{})}
		l.pending[p] = struct{}{}
		l.mu.Unlock()
		go l.prepare(p)
	}
}

func (l *Listener) prepare(p *pending) {
	defer func() {
		p.cancel()
		p.close()
		l.mu.Lock()
		delete(l.pending, p)
		l.mu.Unlock()
		<-setupSlots
	}()
	stop := context.AfterFunc(p.ctx, p.close)
	defer stop()
	if deadline, ok := p.ctx.Deadline(); ok {
		if err := p.raw.SetDeadline(deadline); err != nil {
			return
		}
	}
	conn, err := l.cfg.Prepare(p.ctx, p.raw)
	if err != nil {
		return
	}
	// The raw connection remains owned here until Accept takes it, including
	// while a successful handshake waits for a slow consumer.
	p.conn = conn
	select {
	case l.ready <- p:
		<-p.taken
	case <-p.ctx.Done():
	case <-l.done:
	}
}

func (l *Listener) Accept() (net.Conn, error) {
	for {
		select {
		case <-l.done:
			l.mu.Lock()
			err := l.err
			l.mu.Unlock()
			return nil, err
		case p := <-l.ready:
			// Serialize ownership transfer with Close and timeout cleanup.
			l.mu.Lock()
			p.mu.Lock()
			if l.err == nil && !p.closed && p.ctx.Err() == nil {
				if err := p.conn.SetDeadline(time.Time{}); err == nil {
					p.detached = true
					close(p.taken)
					p.mu.Unlock()
					l.mu.Unlock()
					return p.conn, nil
				}
			}
			p.mu.Unlock()
			l.mu.Unlock()
			close(p.taken)
		}
	}
}

func (l *Listener) stop(err error) {
	l.mu.Lock()
	if l.err != nil {
		l.mu.Unlock()
		return
	}
	l.err = err
	close(l.done)
	for p := range l.pending {
		p.cancel()
		p.close()
	}
	l.mu.Unlock()
	l.Listener.Close()
}

func (l *Listener) Close() error { l.stop(net.ErrClosed); return nil }
