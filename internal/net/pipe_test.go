package net

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

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
