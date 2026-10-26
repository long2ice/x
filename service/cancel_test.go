package service

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/metadata"
	xlogger "github.com/go-gost/x/logger"
)

type cancellationListener struct{ net.Listener }

func (*cancellationListener) Init(metadata.Metadata) error { return nil }

type blockedHandler struct{ started, done chan struct{} }

func (*blockedHandler) Init(metadata.Metadata) error { return nil }
func (h *blockedHandler) Handle(_ context.Context, conn net.Conn, _ ...handler.HandleOption) error {
	close(h.started)
	var b [1]byte
	_, err := conn.Read(b[:])
	conn.Close()
	close(h.done)
	return err
}

func TestCloseUnblocksHandlerThatIsReadingHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &blockedHandler{started: make(chan struct{}), done: make(chan struct{})}
	svc := NewService(t.Name(), &cancellationListener{listener}, h, LoggerOption(xlogger.Nop()))
	defer svc.Close()
	go svc.Serve()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-h.started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	svc.Close()
	select {
	case <-h.done:
	case <-time.After(time.Second):
		t.Fatal("service shutdown retained its blocked handler")
	}
}
