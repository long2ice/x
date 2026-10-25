package sniffing

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	xlogger "github.com/go-gost/x/logger"
	xrecorder "github.com/go-gost/x/recorder"
)

func TestUpgradeKeepsOneWayTrafficAliveThenReclaimsIdle(t *testing.T) {
	client, clientPipe := net.Pipe()
	targetPipe, target := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); client.Close(); target.Close() })
	client.SetDeadline(time.Now().Add(5 * time.Second))
	target.SetDeadline(time.Now().Add(5 * time.Second))
	const idle = 250 * time.Millisecond
	sniffer := &Sniffer{IdleTimeout: idle, BufferSize: 1024}
	header := http.Header{"Connection": {"Upgrade"}, "Upgrade": {"test"}}
	req := &http.Request{Method: http.MethodGet, Header: header}
	res := &http.Response{StatusCode: http.StatusSwitchingProtocols, ProtoMajor: 1, ProtoMinor: 1, Header: header}
	done := make(chan error, 1)
	go func() {
		done <- sniffer.handleUpgradeResponse(ctx, clientPipe, targetPipe, req, res, &xrecorder.HandlerRecorderObject{}, xlogger.Nop())
	}()
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	for i := 0; i < 12; i++ {
		if _, err := target.Write([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			t.Fatal(err)
		}
		if b[0] != byte(i) {
			t.Fatal("upgraded response payload changed")
		}
		time.Sleep(idle / 5)
	}
	select {
	case err := <-done:
		t.Fatalf("active one-way upgrade closed: %v", err)
	default:
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fully idle upgrade was not reclaimed")
	}
}
