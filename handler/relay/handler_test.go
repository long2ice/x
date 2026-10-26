package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"runtime"
	"strconv"
	"testing"
	"time"

	corechain "github.com/go-gost/core/chain"
	"github.com/go-gost/core/handler"
	protocol "github.com/go-gost/relay"
	xchain "github.com/go-gost/x/chain"
	"github.com/go-gost/x/internal/util/mux"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"
)

func TestMetadataTCPResources(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values map[string]any
		idle   time.Duration
		buffer int
	}{
		{"defaults", nil, 5 * time.Minute, 16 * 1024},
		{"custom", map[string]any{"idleTimeout": "10m", "tcp.bufferSize": 24 * 1024}, 10 * time.Minute, 24 * 1024},
		{"disabled", map[string]any{"idleTimeout": 0, "bufferSize": 8192}, 0, 8192},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &relayHandler{}
			if err := h.parseMetadata(xmd.NewMetadata(tt.values)); err != nil {
				t.Fatal(err)
			}
			if h.md.idleTimeout != tt.idle || h.md.bufferSize != tt.buffer {
				t.Fatalf("idle=%v buffer=%d, want %v and %d", h.md.idleTimeout, h.md.bufferSize, tt.idle, tt.buffer)
			}
		})
	}
	h := &relayHandler{}
	if err := h.parseMetadata(xmd.NewMetadata(map[string]any{"idleTimeout": "-1s"})); err == nil {
		t.Fatal("negative idle timeout must be rejected")
	}
}

// Exercise a real relay request over smux, with a real TCP/UDP destination.
func dialRelay(t *testing.T, network, address string, idle time.Duration) (net.Conn, <-chan error) {
	t.Helper()
	log := xlogger.Nop()
	h := NewHandler(
		handler.LoggerOption(log),
		handler.RouterOption(xchain.NewRouter(corechain.LoggerRouterOption(log))),
	)
	if err := h.Init(xmd.NewMetadata(map[string]any{"idleTimeout": idle.String(), "nodelay": true})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.(*relayHandler).Close() })
	clientBase, serverBase := net.Pipe()
	t.Cleanup(func() { clientBase.Close(); serverBase.Close() })
	clientSession, err := mux.ClientSession(clientBase, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clientSession.Close() })
	serverSession, err := mux.ServerSession(serverBase, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client, err := clientSession.GetConn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server, err := serverSession.Accept()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.Handle(context.Background(), server) }()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	portNumber, _ := strconv.Atoi(port)
	req := &protocol.Request{
		Version: protocol.Version1, Cmd: protocol.CmdConnect,
		Features: []protocol.Feature{&protocol.AddrFeature{Host: host, Port: uint16(portNumber)}},
	}
	if network == "udp" {
		req.Cmd |= protocol.FUDP
	}
	if _, err := req.WriteTo(client); err != nil {
		t.Fatal(err)
	}
	response := &protocol.Response{}
	if _, err := response.ReadFrom(client); err != nil {
		t.Fatal(err)
	}
	if response.Status != protocol.StatusOK {
		t.Fatalf("relay status=%d", response.Status)
	}
	return client, done
}

func tcpRelay(t *testing.T, idle time.Duration) (net.Conn, net.Conn, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	client, done := dialRelay(t, "tcp", listener.Addr().String(), idle)
	select {
	case target := <-accepted:
		t.Cleanup(func() { target.Close() })
		target.SetDeadline(time.Now().Add(5 * time.Second))
		return client, target, done
	case <-time.After(5 * time.Second):
		t.Fatal("destination connection was not established")
		return nil, nil, nil
	}
}

func TestHandleTCPReclaimsIdleStream(t *testing.T) {
	client, target, done := tcpRelay(t, 100*time.Millisecond)
	for name, conn := range map[string]net.Conn{"client": client, "target": target} {
		var b [1]byte
		if _, err := conn.Read(b[:]); err != io.EOF {
			t.Fatalf("%s: expected EOF after idle reclamation, got %v", name, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle relay handler did not exit")
	}
}

func TestHandleTCPKeepsOneWayTrafficAlive(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			const idle = 250 * time.Millisecond
			client, target, done := tcpRelay(t, idle)
			source, destination := client, target
			if direction == "download" {
				source, destination = target, client
			}
			for i := 0; i < 12; i++ {
				if _, err := source.Write([]byte{byte(i)}); err != nil {
					t.Fatal(err)
				}
				var b [1]byte
				if _, err := io.ReadFull(destination, b[:]); err != nil {
					t.Fatal(err)
				}
				if b[0] != byte(i) {
					t.Fatal("forwarded data changed")
				}
				time.Sleep(idle / 5)
			}
			select {
			case err := <-done:
				t.Fatalf("active %s stream closed: %v", direction, err)
			default:
			}
		})
	}
}

func TestHandleUDPPreservesLargeDatagrams(t *testing.T) {
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { packetConn.Close() })
	go func() {
		buffer := make([]byte, 65536)
		for {
			n, peer, err := packetConn.ReadFrom(buffer)
			if err != nil {
				return
			}
			packetConn.WriteTo(buffer[:n], peer)
		}
	}()
	client, done := dialRelay(t, "udp", packetConn.LocalAddr().String(), 40*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	payloadSize := 24 * 1024
	if runtime.GOOS == "darwin" {
		// macOS defaults to a 9216-byte UDP datagram limit. The Linux test
		// binary exercises the larger-than-TCP-buffer case on the target host.
		payloadSize = 8 * 1024
	}
	t.Logf("UDP payload: %d bytes", payloadSize)
	payload := bytes.Repeat([]byte{'x'}, payloadSize)
	packet := make([]byte, len(payload)+2)
	binary.BigEndian.PutUint16(packet[:2], uint16(len(payload)))
	copy(packet[2:], payload)
	if _, err := client.Write(packet); err != nil {
		t.Fatal(err)
	}
	var header [2]byte
	if _, err := io.ReadFull(client, header[:]); err != nil {
		select {
		case handlerErr := <-done:
			t.Fatalf("read UDP response: %v; relay handler: %v", err, handlerErr)
		default:
		}
		t.Fatal(err)
	}
	response := make([]byte, int(binary.BigEndian.Uint16(header[:])))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatalf("UDP datagram changed: got %d bytes, want %d", len(response), len(payload))
	}
}

func TestBindReclaimsStreamsWithoutClosingControl(t *testing.T) {
	log := xlogger.Nop()
	h := NewHandler(handler.LoggerOption(log))
	if err := h.Init(xmd.NewMetadata(map[string]any{"bind": true, "idleTimeout": "100ms"})); err != nil {
		t.Fatal(err)
	}
	defer h.(*relayHandler).Close()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go h.Handle(context.Background(), server)
	client.SetDeadline(time.Now().Add(3 * time.Second))
	req := &protocol.Request{Version: protocol.Version1, Cmd: protocol.CmdBind, Features: []protocol.Feature{&protocol.AddrFeature{Host: "127.0.0.1"}}}
	if _, err := req.WriteTo(client); err != nil {
		t.Fatal(err)
	}
	response := &protocol.Response{}
	if _, err := response.ReadFrom(client); err != nil || response.Status != protocol.StatusOK {
		t.Fatalf("bind: %v, %v", response, err)
	}
	var address string
	for _, feature := range response.Features {
		if addr, ok := feature.(*protocol.AddrFeature); ok {
			address = net.JoinHostPort(addr.Host, strconv.Itoa(int(addr.Port)))
		}
	}
	if address == "" {
		t.Fatal("missing bind address")
	}
	client.SetDeadline(time.Time{})
	session, err := mux.ServerSession(client, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	time.Sleep(250 * time.Millisecond)
	for range 2 {
		incoming, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatalf("idle control connection lost its listener: %v", err)
		}
		t.Cleanup(func() { incoming.Close() })
		accepted := make(chan net.Conn, 1)
		go func() {
			if conn, err := session.Accept(); err == nil {
				accepted <- conn
			}
		}()
		var stream net.Conn
		select {
		case stream = <-accepted:
		case <-time.After(time.Second):
			t.Fatal("bind stream was not opened")
		}
		t.Cleanup(func() { stream.Close() })
		stream.SetDeadline(time.Now().Add(time.Second))
		incoming.SetDeadline(time.Now().Add(time.Second))
		response := &protocol.Response{}
		if _, err := response.ReadFrom(stream); err != nil || response.Status != protocol.StatusOK {
			t.Fatalf("bind stream response: %v, %v", response, err)
		}
		var b [1]byte
		if _, err := stream.Read(b[:]); err != io.EOF {
			t.Fatalf("idle bind stream was not reclaimed: %v", err)
		}
		if _, err := incoming.Read(b[:]); err != io.EOF {
			t.Fatalf("idle bind TCP peer was not reclaimed: %v", err)
		}
	}
}
