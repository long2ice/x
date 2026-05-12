package openvpn

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/core/dialer"
	corelistener "github.com/go-gost/core/listener"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"

	dialerpkg "github.com/go-gost/x/dialer/openvpn"
)

func TestUDPListenerDialerEndToEnd(t *testing.T) {
	const psk = "udp integration psk"

	l := NewListener(
		corelistener.AddrOption("127.0.0.1:0"),
		corelistener.LoggerOption(xlogger.Nop()),
	)
	if err := l.Init(xmd.NewMetadata(map[string]any{"key": psk, "udp": true, "idleTimeout": "500ms"})); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			if _, err := c.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	d := dialerpkg.NewDialer(dialer.LoggerOption(xlogger.Nop()))
	if err := d.Init(xmd.NewMetadata(map[string]any{"key": psk, "udp": true})); err != nil {
		t.Fatalf("dialer init: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := d.Dial(ctx, addr, dialer.NetDialerDialOption(stdDialer{}))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	msgs := [][]byte{
		[]byte("ping"),
		[]byte("second message"),
		bytes.Repeat([]byte("payload-"), 100), // ~800B, single chunk
	}
	for i, m := range msgs {
		var wg sync.WaitGroup
		wg.Add(1)
		go func(m []byte) {
			defer wg.Done()
			_, _ = cli.Write(m)
		}(m)

		got := make([]byte, len(m))
		if err := readFullWithTimeout(cli, got, 5*time.Second); err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		wg.Wait()
		if !bytes.Equal(got, m) {
			t.Errorf("msg %d mismatch (len got=%d want=%d)", i, len(got), len(m))
		}
	}

	cli.Close()
	// UDP has no FIN — server must self-evict via idleTimeout (500ms).
	select {
	case <-srvDone:
	case <-time.After(3 * time.Second):
		t.Errorf("server goroutine did not exit after idleTimeout")
	}
}

func TestUDPListenerRejectsWrongKey(t *testing.T) {
	l := NewListener(
		corelistener.AddrOption("127.0.0.1:0"),
		corelistener.LoggerOption(xlogger.Nop()),
	)
	if err := l.Init(xmd.NewMetadata(map[string]any{"key": "right", "udp": true, "handshakeTimeout": "1s"})); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	defer l.Close()

	d := dialerpkg.NewDialer(dialer.LoggerOption(xlogger.Nop()))
	if err := d.Init(xmd.NewMetadata(map[string]any{"key": "wrong", "udp": true, "handshakeTimeout": "2s"})); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := d.Dial(ctx, l.Addr().String(), dialer.NetDialerDialOption(stdDialer{})); err == nil {
		t.Errorf("dial with wrong key should fail")
	}
}

func TestUDPListenerIgnoresGarbagePackets(t *testing.T) {
	const psk = "garbage drop psk"
	l := NewListener(
		corelistener.AddrOption("127.0.0.1:0"),
		corelistener.LoggerOption(xlogger.Nop()),
	)
	if err := l.Init(xmd.NewMetadata(map[string]any{"key": psk, "udp": true})); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().String()

	// Send a few garbage packets at the listener from a separate socket.
	noise, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, _ = noise.Write([]byte("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"))
	}
	noise.Close()

	// Real client should still succeed.
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		_, _ = c.Write(buf[:n])
	}()

	d := dialerpkg.NewDialer(dialer.LoggerOption(xlogger.Nop()))
	if err := d.Init(xmd.NewMetadata(map[string]any{"key": psk, "udp": true})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := d.Dial(ctx, addr, dialer.NetDialerDialOption(stdDialer{}))
	if err != nil {
		t.Fatalf("legit dial after garbage: %v", err)
	}
	defer cli.Close()

	go cli.Write([]byte("hi"))
	got := make([]byte, 2)
	if err := readFullWithTimeout(cli, got, 5*time.Second); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, []byte("hi")) {
		t.Errorf("got %q want %q", got, "hi")
	}
}

func readFullWithTimeout(r net.Conn, b []byte, timeout time.Duration) error {
	_ = r.SetReadDeadline(time.Now().Add(timeout))
	defer r.SetReadDeadline(time.Time{})
	_, err := io.ReadFull(r, b)
	return err
}
