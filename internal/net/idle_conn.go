package net

import (
	"context"
	"net"
	"sync"
	"time"
)

// NewIdleConn watches connection-wide traffic, including HTTP bodies and idle
// keepalive reads. Closing it releases the timer and cancellation callback.
func NewIdleConn(ctx context.Context, conn net.Conn, idle time.Duration) *idleConn {
	ctx, cancel := context.WithCancel(ctx)
	c := &idleConn{Conn: conn, ctx: ctx, cancel: cancel, activity: newPipeActivity(), idle: idle}
	c.mu.Lock()
	if idle > 0 {
		c.timer = time.AfterFunc(idle, c.checkIdle)
	}
	c.stop = context.AfterFunc(ctx, func() { c.Close() })
	c.mu.Unlock()
	return c
}

type idleConn struct {
	net.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	activity *pipeActivity
	idle     time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	stop     func() bool
	closed   bool
}

func (c *idleConn) Context() context.Context { return c.ctx }

func (c *idleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}

func (c *idleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}

func (c *idleConn) checkIdle() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if delay := c.activity.remaining(c.idle); delay > 0 {
		c.timer.Reset(delay)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.Close()
}

func (c *idleConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
	if c.stop != nil {
		c.stop()
	}
	c.mu.Unlock()
	c.cancel()
	return c.Conn.Close()
}
