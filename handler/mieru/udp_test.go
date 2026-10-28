package mieru

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/model"
)

func TestParseSocks5UDPDatagram(t *testing.T) {
	t.Parallel()

	spec := model.AddrSpec{IP: net.ParseIP("127.0.0.1"), Port: 53}
	var addrBuf bytes.Buffer
	if err := spec.WriteToSocks5(&addrBuf); err != nil {
		t.Fatal(err)
	}

	raw := append([]byte{0, 0, 0}, addrBuf.Bytes()...)
	raw = append(raw, []byte("ping")...)

	dg, err := parseSocks5UDPDatagram(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(dg.Payload) != "ping" {
		t.Fatalf("payload = %q", dg.Payload)
	}
	if dg.Addr.String() != "127.0.0.1:53" {
		t.Fatalf("addr = %s", dg.Addr.String())
	}
}

func TestRunUDPAssociateLoopStopsOnEitherError(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		stop func(net.Conn, net.PacketConn) error
		want error
	}{
		{
			name: "client disconnects",
			stop: func(client net.Conn, _ net.PacketConn) error { return client.Close() },
			want: io.EOF,
		},
		{
			name: "upstream closes",
			stop: func(_ net.Conn, upstream net.PacketConn) error { return upstream.Close() },
			want: net.ErrClosed,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server, client := net.Pipe()
			t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
			upstream, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = upstream.Close() })

			done := make(chan error, 1)
			go func() {
				done <- runUDPAssociateLoop(context.Background(), apicommon.NewPacketOverStreamTunnel(server), upstream, nil, udpAssociateOptions{})
			}()

			if err := tt.stop(client, upstream); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, tt.want) {
					t.Fatalf("loop returned %v; want %v", err, tt.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("loop did not stop after one side closed")
			}

			_ = upstream.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, _, err := upstream.ReadFrom(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("upstream still open: %v", err)
			}
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("client stream still open: %v", err)
			}
		})
	}
}

func TestRunUDPAssociateLoopForwardsDatagrams(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	upstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	target, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })

	done := make(chan error, 1)
	go func() {
		done <- runUDPAssociateLoop(context.Background(), apicommon.NewPacketOverStreamTunnel(server), upstream, nil, udpAssociateOptions{})
	}()
	clientTunnel := apicommon.NewPacketOverStreamTunnel(client)
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := target.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}

	header, err := udpAddrHeader(target.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientTunnel.Write(append(header, []byte("ping")...)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	n, sender, err := target.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("target received %q, want ping", buf[:n])
	}
	if _, err := target.WriteTo([]byte("pong"), sender); err != nil {
		t.Fatal(err)
	}
	n, err = clientTunnel.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	datagram, err := parseSocks5UDPDatagram(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if string(datagram.Payload) != "pong" || datagram.Addr.String() != target.LocalAddr().String() {
		t.Fatalf("client received payload %q from %s", datagram.Payload, datagram.Addr.String())
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("loop returned %v; want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after client disconnect")
	}
}
