package udp

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestRelayCancellationClosesBlockedEndpoints(t *testing.T) {
	left, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	right, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		left.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { left.Close(); right.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewRelay(left, right).Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancelled UDP relay retained its blocked readers")
	}
}
