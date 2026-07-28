package vless

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/logger"
	xchain "github.com/go-gost/x/chain"
	xvless "github.com/go-gost/x/internal/util/vless"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"
)

const testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

func TestMain(m *testing.M) {
	logger.SetDefault(xlogger.NewLogger(xlogger.LevelOption(logger.ErrorLevel)))
	os.Exit(m.Run())
}

func echoServer(t *testing.T) net.Addr {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()

	return ln.Addr()
}

func udpEchoServer(t *testing.T) net.Addr {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })

	go func() {
		b := make([]byte, 4096)
		for {
			n, addr, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(b[:n], addr)
		}
	}()

	return pc.LocalAddr()
}

func newTestHandler(t *testing.T, md map[string]any) handler.Handler {
	t.Helper()

	if md == nil {
		md = map[string]any{}
	}
	if _, ok := md["users"]; !ok {
		md["users"] = map[string]any{"alice": testUUID}
	}

	h := NewHandler(
		handler.RouterOption(xchain.NewRouter(chain.LoggerRouterOption(logger.Default()))),
		handler.LoggerOption(logger.Default()),
		handler.ServiceOption("test"),
	)
	if err := h.Init(xmd.NewMetadata(md)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := h.(io.Closer); ok {
			c.Close()
		}
	})

	return h
}

// dialHandler runs the handler on one end of a pipe and returns the other end,
// with the VLESS request already sent.
func dialHandler(t *testing.T, h handler.Handler, req *xvless.Request) net.Conn {
	t.Helper()

	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close() })

	go h.Handle(context.Background(), c2)

	c1.SetDeadline(time.Now().Add(10 * time.Second))
	if err := xvless.WriteRequest(c1, req); err != nil {
		t.Fatal(err)
	}

	return c1
}

func TestHandleTCP(t *testing.T) {
	id, _ := xvless.ParseUUID(testUUID)
	addr := echoServer(t).(*net.TCPAddr)

	conn := dialHandler(t, newTestHandler(t, nil), &xvless.Request{
		ID:      id,
		Command: xvless.CmdTCP,
		Host:    addr.IP.String(),
		Port:    addr.Port,
	})

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	if err := xvless.ReadResponse(conn); err != nil {
		t.Fatal(err)
	}

	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "ping" {
		t.Errorf("got %q, want ping", b)
	}
}

func TestHandleUDP(t *testing.T) {
	id, _ := xvless.ParseUUID(testUUID)
	addr := udpEchoServer(t).(*net.UDPAddr)

	conn := dialHandler(t, newTestHandler(t, nil), &xvless.Request{
		ID:      id,
		Command: xvless.CmdUDP,
		Host:    addr.IP.String(),
		Port:    addr.Port,
	})

	pc := xvless.PacketConn(conn, addr)
	if _, err := pc.WriteTo([]byte("ping"), addr); err != nil {
		t.Fatal(err)
	}

	if err := xvless.ReadResponse(conn); err != nil {
		t.Fatal(err)
	}

	b := make([]byte, 1024)
	n, _, err := pc.ReadFrom(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:n]) != "ping" {
		t.Errorf("got %q, want ping", b[:n])
	}
}

func TestHandleUnknownUser(t *testing.T) {
	other, _ := xvless.ParseUUID("00000000-0000-0000-0000-000000000000")
	addr := echoServer(t).(*net.TCPAddr)

	h := newTestHandler(t, nil)
	c1, c2 := net.Pipe()
	defer c1.Close()

	errc := make(chan error, 1)
	go func() { errc <- h.Handle(context.Background(), c2) }()

	c1.SetDeadline(time.Now().Add(5 * time.Second))
	xvless.WriteRequest(c1, &xvless.Request{
		ID:      other,
		Command: xvless.CmdTCP,
		Host:    addr.IP.String(),
		Port:    addr.Port,
	})

	if err := <-errc; err != ErrUnauthorized {
		t.Errorf("got %v, want %v", err, ErrUnauthorized)
	}
}

func TestHandleVisionWithoutRawConn(t *testing.T) {
	id, _ := xvless.ParseUUID(testUUID)
	addr := echoServer(t).(*net.TCPAddr)

	h := newTestHandler(t, nil)
	c1, c2 := net.Pipe()
	defer c1.Close()

	errc := make(chan error, 1)
	go func() { errc <- h.Handle(context.Background(), c2) }()

	c1.SetDeadline(time.Now().Add(5 * time.Second))
	xvless.WriteRequest(c1, &xvless.Request{
		ID:      id,
		Command: xvless.CmdTCP,
		Flow:    xvless.FlowVision,
		Host:    addr.IP.String(),
		Port:    addr.Port,
	})

	// A plain connection cannot hand its transport over, the flow is refused
	// instead of breaking halfway through.
	err := <-errc
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("reality")) {
		t.Errorf("got %v, want a flow error", err)
	}
}

func TestUsersFromAuth(t *testing.T) {
	h := NewHandler(
		handler.AuthOption(url.UserPassword("alice", testUUID)),
		handler.LoggerOption(logger.Default()),
	)
	if err := h.Init(xmd.NewMetadata(nil)); err != nil {
		t.Fatal(err)
	}

	id, _ := xvless.ParseUUID(testUUID)
	if name := h.(*vlessHandler).users[id]; name != "alice" {
		t.Errorf("got %q, want alice", name)
	}
}

func TestNoUsers(t *testing.T) {
	h := NewHandler(handler.LoggerOption(logger.Default()))
	if err := h.Init(xmd.NewMetadata(nil)); err != ErrNoUser {
		t.Errorf("got %v, want %v", err, ErrNoUser)
	}
}
