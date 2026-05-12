package openvpn

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	xnet "github.com/go-gost/core/common/net"
	"github.com/go-gost/core/dialer"
	corelistener "github.com/go-gost/core/listener"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"

	dialerpkg "github.com/go-gost/x/dialer/openvpn"
)

type stdDialer struct{}

func (stdDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func TestListenerDialerEndToEnd(t *testing.T) {
	const psk = "integration psk"

	l := NewListener(
		corelistener.AddrOption("127.0.0.1:0"),
		corelistener.LoggerOption(xlogger.Nop()),
	)
	if err := l.Init(xmd.NewMetadata(map[string]any{"key": psk})); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	// Echo server
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

	d := dialerpkg.NewDialer(
		dialer.LoggerOption(xlogger.Nop()),
	)
	if err := d.Init(xmd.NewMetadata(map[string]any{"key": psk})); err != nil {
		t.Fatalf("dialer init: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cli, err := d.Dial(ctx, addr, dialer.NetDialerDialOption(stdDialer{}))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	msgs := [][]byte{
		[]byte("hello"),
		bytes.Repeat([]byte("chunked-"), 300), // ~2400 bytes, exercises chunking
	}
	for i, m := range msgs {
		var wg sync.WaitGroup
		wg.Add(1)
		go func(m []byte) {
			defer wg.Done()
			_, _ = cli.Write(m)
		}(m)

		got := make([]byte, len(m))
		if _, err := io.ReadFull(cli, got); err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		wg.Wait()
		if !bytes.Equal(got, m) {
			t.Errorf("msg %d mismatch (len got=%d want=%d)", i, len(got), len(m))
		}
	}

	cli.Close()
	select {
	case <-srvDone:
	case <-time.After(time.Second):
		t.Errorf("server goroutine did not exit after client close")
	}
}

func TestListenerRejectsWrongKey(t *testing.T) {
	l := NewListener(
		corelistener.AddrOption("127.0.0.1:0"),
		corelistener.LoggerOption(xlogger.Nop()),
	)
	if err := l.Init(xmd.NewMetadata(map[string]any{"key": "right"})); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	defer l.Close()

	d := dialerpkg.NewDialer(dialer.LoggerOption(xlogger.Nop()))
	if err := d.Init(xmd.NewMetadata(map[string]any{"key": "wrong"})); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.Dial(ctx, l.Addr().String(), dialer.NetDialerDialOption(stdDialer{})); err == nil {
		t.Errorf("dial with wrong key should fail")
	}
}

// ensure unused import xnet is referenced
var _ xnet.Dialer = stdDialer{}
