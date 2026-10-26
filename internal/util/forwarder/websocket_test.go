package forwarder

import (
	"bytes"
	"context"
	xlogger "github.com/go-gost/x/logger"
	xrecorder "github.com/go-gost/x/recorder"
	"io"
	"net"
	"testing"
	"time"
)

func TestWebsocketRecordingReclaimsIdle(t *testing.T) {
	client, left := net.Pipe()
	right, target := net.Pipe()
	t.Cleanup(func() { client.Close(); target.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Sniffer{IdleTimeout: 50 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		done <- h.sniffingWebsocketFrame(ctx, left, right, &xrecorder.HandlerRecorderObject{}, xlogger.Nop())
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("idle WebSocket recording retained its frame readers")
	}
}

func TestWebsocketRecordingKeepsOneWayTrafficAndCancels(t *testing.T) {
	client, left := net.Pipe()
	right, target := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); client.Close(); target.Close() })
	client.SetDeadline(time.Now().Add(3 * time.Second))
	target.SetDeadline(time.Now().Add(3 * time.Second))
	h := &Sniffer{IdleTimeout: 100 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		done <- h.sniffingWebsocketFrame(ctx, left, right, &xrecorder.HandlerRecorderObject{}, xlogger.Nop())
	}()
	for i := range 8 {
		frame := []byte{0x82, 1, byte(i)}
		write := make(chan error, 1)
		go func() { _, err := target.Write(frame); write <- err }()
		received := make([]byte, len(frame))
		if _, err := io.ReadFull(client, received); err != nil {
			t.Fatal(err)
		}
		if err := <-write; err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(received, frame) {
			t.Fatalf("frame changed: %v", received)
		}
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WebSocket readers survived cancellation")
	}
}
