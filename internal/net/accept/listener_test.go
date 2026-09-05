package accept

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func listen(t *testing.T, cfg Config) *Listener {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := NewListener(raw, cfg)
	t.Cleanup(func() { l.Close() })
	return l
}

func dial(t *testing.T, l net.Listener) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	return c
}

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for setup")
	}
}

func TestSlowSetupDoesNotBlockAccept(t *testing.T) {
	started := make(chan struct{}, 3)
	l := listen(t, Config{Timeout: 2 * time.Second, Prepare: func(ctx context.Context, c net.Conn) (net.Conn, error) {
		started <- struct{}{}
		b := make([]byte, 1)
		_, err := io.ReadFull(c, b)
		return c, err
	}})
	dial(t, l)
	wait(t, started)
	dial(t, l)
	wait(t, started)
	c := dial(t, l)
	c.Write([]byte{1})
	now := time.Now()
	accepted, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if time.Since(now) > time.Second {
		t.Fatal("healthy connection waited behind slow setup")
	}
}

func TestCloseCancelsSetupAndQueuedConnections(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "setup", true: "queued"}[ready], func(t *testing.T) {
			started := make(chan struct{})
			exited := make(chan struct{})
			l := listen(t, Config{Prepare: func(ctx context.Context, c net.Conn) (net.Conn, error) {
				defer close(exited)
				close(started)
				if !ready {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return c, nil
			}})
			c := dial(t, l)
			wait(t, started)
			l.Close()
			wait(t, exited)
			if _, err := c.Read(make([]byte, 1)); err == nil {
				t.Fatal("pending socket left open")
			}
			if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Accept: %v", err)
			}
		})
	}
}

func TestSetupLimitAndRelease(t *testing.T) {
	started := make(chan struct{}, 3)
	l := listen(t, Config{MaxPending: 2, Timeout: 150 * time.Millisecond, Prepare: func(ctx context.Context, c net.Conn) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	dial(t, l)
	wait(t, started)
	dial(t, l)
	wait(t, started)
	excess := dial(t, l)
	if _, err := excess.Read(make([]byte, 1)); err == nil {
		t.Fatal("excess setup was not rejected")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		l.mu.Lock()
		n := len(l.pending)
		l.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired setup retained slots")
		}
		time.Sleep(time.Millisecond)
	}
	dial(t, l)
	wait(t, started)
}

func TestAcceptedConnectionSurvivesSetupTimeoutAndListenerClose(t *testing.T) {
	l := listen(t, Config{Timeout: 30 * time.Millisecond, Prepare: func(_ context.Context, c net.Conn) (net.Conn, error) { return c, nil }})
	c := dial(t, l)
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l.Close()
	time.Sleep(60 * time.Millisecond)
	go c.Write([]byte("ok"))
	s.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 2)
	if _, err := io.ReadFull(s, b); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCloseAndDelivery(t *testing.T) {
	for range 100 {
		l := listen(t, Config{Timeout: time.Second, Prepare: func(_ context.Context, c net.Conn) (net.Conn, error) { return c, nil }})
		c := dial(t, l)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			conn, _ := l.Accept()
			if conn != nil {
				conn.Close()
			}
		}()
		go func() { defer wg.Done(); l.Close() }()
		wg.Wait()
		c.Close()
	}
}
