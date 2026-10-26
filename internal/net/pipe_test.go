package net

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type fastPathConn struct {
	net.Conn
	readSize chan int
}

func (c *fastPathConn) Read(b []byte) (int, error) {
	select {
	case c.readSize <- len(b):
	default:
	}
	return c.Conn.Read(b)
}

func (c *fastPathConn) WriteTo(io.Writer) (int64, error) {
	return 0, errors.New("unexpected WriterTo bypassed the configured copy buffer")
}

func (c *fastPathConn) ReadFrom(io.Reader) (int64, error) {
	return 0, errors.New("unexpected ReadFrom bypassed the configured copy buffer")
}

func TestPipeIdleBufferHonorsSizeWithoutTimeout(t *testing.T) {
	leftClient, leftPipe := net.Pipe()
	rightPipe, rightServer := net.Pipe()
	t.Cleanup(func() { leftClient.Close(); rightServer.Close() })
	left := &fastPathConn{Conn: leftPipe, readSize: make(chan int, 1)}
	right := &fastPathConn{Conn: rightPipe, readSize: make(chan int, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- PipeIdleBuffer(ctx, left, right, 0, 1024) }()
	for _, conn := range []*fastPathConn{left, right} {
		select {
		case size := <-conn.readSize:
			if size != 1024 {
				t.Fatalf("copy read buffer = %d, want 1024", size)
			}
		case err := <-done:
			t.Fatalf("copy did not use the configured buffer: %v", err)
		case <-time.After(time.Second):
			t.Fatal("copy did not start reading")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled pipe did not exit")
	}
}

func TestPipeIdleBufferKeepsOneWayTrafficAlive(t *testing.T) {
	leftClient, leftPipe := net.Pipe()
	rightPipe, rightServer := net.Pipe()
	t.Cleanup(func() {
		leftClient.Close()
		rightServer.Close()
	})

	const idle = 50 * time.Millisecond
	errCh := make(chan error, 1)
	go func() {
		errCh <- PipeIdleBuffer(context.Background(), leftPipe, rightPipe, idle, 1024)
	}()

	// Keep only the client-to-server direction active for several complete idle
	// windows. Per-direction idle deadlines would close this connection.
	for i := range 8 {
		writeErr := make(chan error, 1)
		go func(b byte) {
			_, err := leftClient.Write([]byte{b})
			writeErr <- err
		}(byte(i))

		var b [1]byte
		if _, err := io.ReadFull(rightServer, b[:]); err != nil {
			t.Fatal(err)
		}
		if err := <-writeErr; err != nil {
			t.Fatal(err)
		}
		time.Sleep(idle / 2)
	}

	select {
	case err := <-errCh:
		t.Fatalf("pipe closed during one-way activity: %v", err)
	default:
	}

	leftClient.Close()
	rightServer.Close()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("pipe did not stop after its endpoints closed")
	}
}

func TestPipeIdleBufferClosesFullyIdleConnection(t *testing.T) {
	leftClient, leftPipe := net.Pipe()
	rightPipe, rightServer := net.Pipe()
	t.Cleanup(func() {
		leftClient.Close()
		rightServer.Close()
	})

	const idle = 40 * time.Millisecond
	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		errCh <- PipeIdleBuffer(context.Background(), leftPipe, rightPipe, idle, 1024)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("idle pipe returned an error: %v", err)
		}
		if elapsed := time.Since(start); elapsed < idle {
			t.Fatalf("pipe closed too early after %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("fully idle pipe was not closed")
	}
}

func TestPipeIdleKeepsOneWayTrafficAlive(t *testing.T) {
	client, left := net.Pipe()
	right, target := net.Pipe()
	t.Cleanup(func() { client.Close(); target.Close() })
	client.SetDeadline(time.Now().Add(2 * time.Second))
	target.SetDeadline(time.Now().Add(2 * time.Second))
	const idle = 250 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- PipeIdle(context.Background(), left, right, idle) }()
	for range 12 {
		write := make(chan error, 1)
		go func() { _, err := client.Write([]byte{1}); write <- err }()
		var b [1]byte
		if _, err := io.ReadFull(target, b[:]); err != nil {
			t.Fatalf("one-way transfer was interrupted: %v", err)
		}
		if err := <-write; err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("active connection closed: %v", err)
	default:
	}
	client.Close()
	target.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pipe did not stop")
	}
}
