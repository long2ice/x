package openvpn

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func tunnelPair(t *testing.T, psk []byte) (client, server *Tunnel) {
	t.Helper()
	cc, sc := net.Pipe()

	type result struct {
		t   *Tunnel
		err error
	}
	srvCh := make(chan result, 1)
	cliCh := make(chan result, 1)
	go func() {
		tn, err := ServerHandshake(sc, psk)
		srvCh <- result{tn, err}
	}()
	go func() {
		tn, err := ClientHandshake(cc, psk)
		cliCh <- result{tn, err}
	}()

	timeout := time.After(3 * time.Second)
	var s, c result
	for i := 0; i < 2; i++ {
		select {
		case s = <-srvCh:
		case c = <-cliCh:
		case <-timeout:
			t.Fatal("handshake timed out")
		}
	}
	if s.err != nil {
		t.Fatalf("server handshake: %v", s.err)
	}
	if c.err != nil {
		t.Fatalf("client handshake: %v", c.err)
	}
	return c.t, s.t
}

func TestTunnelHandshakeAndEcho(t *testing.T) {
	cli, srv := tunnelPair(t, []byte("integration test psk"))
	defer cli.Close()
	defer srv.Close()

	// Echo server: read N bytes, write them back.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1024)
		for {
			n, err := srv.Read(buf)
			if err != nil {
				return
			}
			if _, err := srv.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	msgs := [][]byte{
		[]byte("hello"),
		[]byte("world"),
		bytes.Repeat([]byte("payload-"), 200), // forces chunking
	}
	for i, m := range msgs {
		writeErr := make(chan error, 1)
		go func(m []byte) {
			_, err := cli.Write(m)
			writeErr <- err
		}(m)

		got := make([]byte, len(m))
		if _, err := io.ReadFull(cli, got); err != nil {
			t.Fatalf("read[%d]: %v", i, err)
		}
		if err := <-writeErr; err != nil {
			t.Fatalf("write[%d]: %v", i, err)
		}
		if !bytes.Equal(got, m) {
			t.Errorf("msg %d mismatch: got %q want %q", i, got, m)
		}
	}

	cli.Close()
	srv.Close()
	wg.Wait()
}

func TestTunnelWrongPSKFailsHandshake(t *testing.T) {
	cc, sc := net.Pipe()
	srvErr := make(chan error, 1)
	cliErr := make(chan error, 1)
	go func() {
		_, err := ServerHandshake(sc, []byte("server-psk"))
		if err != nil {
			sc.Close()
		}
		srvErr <- err
	}()
	go func() {
		_, err := ClientHandshake(cc, []byte("client-psk"))
		if err != nil {
			cc.Close()
		}
		cliErr <- err
	}()

	timeout := time.After(3 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case err := <-srvErr:
			if err == nil {
				t.Errorf("server should have failed with wrong PSK")
			}
		case err := <-cliErr:
			if err == nil {
				t.Errorf("client should have failed with wrong PSK")
			}
		case <-timeout:
			t.Fatal("wrong-PSK handshake timed out (both sides should error fast)")
		}
	}
}

func TestTunnelLargeStream(t *testing.T) {
	cli, srv := tunnelPair(t, []byte("psk"))
	defer cli.Close()
	defer srv.Close()

	payload := bytes.Repeat([]byte("ABCDEFGH"), 8*1024) // 64 KiB → many chunks

	done := make(chan error, 1)
	go func() {
		_, err := cli.Write(payload)
		done <- err
	}()

	got := make([]byte, 0, len(payload))
	for len(got) < len(payload) {
		buf := make([]byte, 4096)
		n, err := srv.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if err := <-done; err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("large payload mismatch (%d vs %d bytes)", len(got), len(payload))
	}
}
