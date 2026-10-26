package net

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestIdleConnCancelsUpstreamContext(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	guard := NewIdleConn(context.Background(), client, 40*time.Millisecond)
	defer guard.Close()
	select {
	case <-guard.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("idle close did not cancel the upstream request context")
	}
	var b [1]byte
	if _, err := peer.Read(b[:]); err != io.EOF {
		t.Fatalf("idle socket remained open: %v", err)
	}
}

func TestIdleConnKeepsOneWayWritesAlive(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	guard := NewIdleConn(context.Background(), client, 100*time.Millisecond)
	defer guard.Close()
	peer.SetDeadline(time.Now().Add(2 * time.Second))
	for range 8 {
		done := make(chan error, 1)
		go func() { _, err := guard.Write([]byte{1}); done <- err }()
		var b [1]byte
		if _, err := io.ReadFull(peer, b[:]); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	select {
	case <-guard.Context().Done():
		t.Fatal("one-way traffic was treated as idle")
	default:
	}
}

func TestPipeIdleFuncCancelsBlockedCallbacks(t *testing.T) {
	client, left := net.Pipe()
	right, target := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); client.Close(); target.Close() })
	done := make(chan error, 1)
	go func() {
		done <- PipeIdleFunc(ctx, left, right, 40*time.Millisecond,
			func(ctx context.Context, _, _ io.ReadWriter, _ bool) error { <-ctx.Done(); return ctx.Err() })
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle cleanup did not cancel protocol workers")
	}
}
