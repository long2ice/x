package proxyproto

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/x/ctx"
	pp "github.com/pires/go-proxyproto"
)

func TestSlowHeadersDoNotBlockOtherClients(t *testing.T) {
	for _, version := range []byte{0, 1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			l := WrapListener(1, raw, 2*time.Second)
			defer l.Close()
			for range 2 {
				c, err := net.Dial("tcp", l.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
			}
			client, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			client.SetDeadline(time.Now().Add(time.Second))
			src := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1234}
			dst := &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 443}
			if version != 0 {
				if _, err := pp.HeaderProxyFromAddrs(version, src, dst).WriteTo(client); err != nil {
					t.Fatal(err)
				}
			}
			client.Write([]byte("payload"))
			now := time.Now()
			c, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if time.Since(now) > time.Second {
				t.Fatal("blocked behind silent clients")
			}
			c.SetReadDeadline(time.Now().Add(time.Second))
			b := make([]byte, 7)
			if _, err := io.ReadFull(c, b); err != nil || string(b) != "payload" {
				t.Fatalf("payload %q: %v", b, err)
			}
			if c.RemoteAddr().String() != client.LocalAddr().String() {
				t.Fatal("raw peer address changed")
			}
			if version != 0 {
				cc := c.(ctx.Context).Context()
				if ctx.SrcAddrFromContext(cc).String() != src.String() || ctx.DstAddrFromContext(cc).String() != dst.String() {
					t.Fatal("PROXY addresses not preserved")
				}
			}
			if err := c.(*serverConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHeaderOnlyDoesNotReadApplicationData(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	go c.Write([]byte("PROXY TCP4 192.0.2.1 192.0.2.2 1234 443\r\n"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := prepareConn(ctx, s, 100*time.Millisecond); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("header parser read application payload")
	}
}

func TestOptionalHeaderSupportsServerFirst(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := WrapListener(1, raw, 100*time.Millisecond)
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go s.Write([]byte("hello"))
	c.SetDeadline(time.Now().Add(time.Second))
	b := make([]byte, 5)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "hello" {
		t.Fatalf("server greeting: %q %v", b, err)
	}
	go c.Write([]byte("reply"))
	s.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(s, b); err != nil || string(b) != "reply" {
		t.Fatalf("client reply: %q %v", b, err)
	}
}

func TestMalformedHeaderRejected(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	go c.Write([]byte("PROXY TCP4 invalid 192.0.2.2 1234 443\r\n"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := prepareConn(ctx, s, time.Second); err == nil {
		t.Fatal("malformed header accepted")
	}
}

func TestConcurrentBufferedReads(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	go c.Write([]byte("PROXY UNKNOWN\r\nabcdefgh"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := prepareConn(ctx, s, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
