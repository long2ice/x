package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-gost/core/connector"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/service"
	"github.com/go-gost/gosocks5"
	"github.com/go-gost/x/config"
	serviceparser "github.com/go-gost/x/config/parsing/service"
	httpconnector "github.com/go-gost/x/connector/http"
	socksconnector "github.com/go-gost/x/connector/socks/v5"
	ssconnector "github.com/go-gost/x/connector/ss"
	_ "github.com/go-gost/x/dialer/tcp"
	_ "github.com/go-gost/x/handler/forward/local"
	_ "github.com/go-gost/x/handler/forward/remote"
	_ "github.com/go-gost/x/handler/http"
	_ "github.com/go-gost/x/handler/socks/v5"
	_ "github.com/go-gost/x/handler/ss"
	"github.com/go-gost/x/internal/util/transport"
	_ "github.com/go-gost/x/listener/tcp"
	_ "github.com/go-gost/x/listener/udp"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"
)

func TestMain(m *testing.M) {
	if os.Getenv("TRANSPORT_AUDIT_DEBUG") != "" {
		logger.SetDefault(xlogger.NewLogger(xlogger.LevelOption(logger.DebugLevel)))
	} else {
		logger.SetDefault(xlogger.Nop())
	}
	os.Exit(m.Run())
}

func startService(t *testing.T, protocol, target string, md map[string]any) service.Service {
	t.Helper()
	cfg := &config.ServiceConfig{
		Name: t.Name(), Addr: "127.0.0.1:0",
		Listener: &config.ListenerConfig{Type: "tcp"},
		Handler:  &config.HandlerConfig{Type: protocol, Metadata: md},
	}
	if protocol == "tcp" || protocol == "rtcp" {
		cfg.Forwarder = &config.ForwarderConfig{Nodes: []*config.ForwardNodeConfig{{Name: "target", Addr: target}}}
	}
	if protocol == "ss" {
		cfg.Handler.Auth = &config.AuthConfig{Username: "chacha20-ietf-poly1305", Password: "integration-test-only"}
	}
	svc, err := serviceparser.ParseService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	go svc.Serve()
	t.Cleanup(func() { svc.Close() })
	return svc
}

func protocolClient(t *testing.T, protocol string, raw net.Conn, target string) net.Conn {
	t.Helper()
	var c connector.Connector
	options := []connector.Option{connector.LoggerOption(xlogger.Nop())}
	switch protocol {
	case "http":
		c = httpconnector.NewConnector(options...)
	case "socks5":
		c = socksconnector.NewConnector(options...)
	case "ss":
		c = ssconnector.NewConnector(append(options, connector.AuthOption(url.UserPassword("chacha20-ietf-poly1305", "integration-test-only")))...)
	default:
		return raw
	}
	if err := c.Init(xmd.NewMetadata(map[string]any{"notls": true})); err != nil {
		t.Fatal(err)
	}
	var err error
	if h, ok := c.(connector.Handshaker); ok {
		raw, err = h.Handshake(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	conn, err := c.Connect(context.Background(), raw, "tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func openStream(t *testing.T, protocol string, idle time.Duration) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	svc := startService(t, protocol, ln.Addr().String(), map[string]any{"idleTimeout": idle.String(), "tcp.bufferSize": 1024})
	proxyAddress := svc.Addr().String()
	if protocol == "ss" {
		proxyAddress = startSSClient(t, proxyAddress, ln.Addr().String(), false)
	}
	raw, err := net.DialTimeout("tcp", proxyAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	raw.SetDeadline(time.Now().Add(3 * time.Second))
	client := raw
	if protocol != "ss" {
		client = protocolClient(t, protocol, raw, ln.Addr().String())
	}
	t.Cleanup(func() { client.Close() })
	if _, err := client.Write([]byte{42}); err != nil {
		t.Fatal(err)
	}
	select {
	case target := <-accepted:
		t.Cleanup(func() { target.Close() })
		target.SetDeadline(time.Now().Add(3 * time.Second))
		var b [1]byte
		if _, err := io.ReadFull(target, b[:]); err != nil || b[0] != 42 {
			t.Fatalf("initial transfer: %v, %v", b, err)
		}
		return client, target
	case <-time.After(time.Second):
		t.Fatal("target was not connected")
		return nil, nil
	}
}

// AEAD salt replay detection is process-global. Keep the client in a separate
// process so a legitimate client salt is not mistaken for a server-side replay.
func startSSClient(t *testing.T, server, target string, noDelay bool) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSSClientProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "TRANSPORT_SS_SERVER="+server, "TRANSPORT_SS_TARGET="+target, fmt.Sprintf("TRANSPORT_SS_NODELAY=%t", noDelay))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	reader := bufio.NewReader(stdout)
	address, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("SS client startup: %v", err)
	}
	go io.Copy(io.Discard, reader)
	return strings.TrimSpace(address)
}

func TestSSClientProcess(t *testing.T) {
	server := os.Getenv("TRANSPORT_SS_SERVER")
	if server == "" {
		t.Skip("subprocess helper")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Fprintln(os.Stdout, listener.Addr().String())
	client, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := net.Dial("tcp", server)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	c := ssconnector.NewConnector(connector.LoggerOption(xlogger.Nop()), connector.AuthOption(url.UserPassword("chacha20-ietf-poly1305", "integration-test-only")))
	if err := c.Init(xmd.NewMetadata(map[string]any{"nodelay": os.Getenv("TRANSPORT_SS_NODELAY") == "true"})); err != nil {
		t.Fatal(err)
	}
	remote, err := c.Connect(context.Background(), raw, "tcp", os.Getenv("TRANSPORT_SS_TARGET"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(remote, client); done <- struct{}{} }()
	go func() { io.Copy(client, remote); done <- struct{}{} }()
	<-done
	client.Close()
	remote.Close()
	<-done
}

var protocols = []string{"http", "socks5", "ss", "tcp", "rtcp"}

func TestTCPHandlersReclaimIdleStream(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			client, target := openStream(t, protocol, 80*time.Millisecond)
			for _, conn := range []net.Conn{client, target} {
				conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				var b [1]byte
				if _, err := conn.Read(b[:]); err != io.EOF {
					t.Fatalf("idle endpoint was not reclaimed: %v", err)
				}
			}
		})
	}
}

func TestTCPHandlersKeepOneWayTrafficAlive(t *testing.T) {
	for _, protocol := range protocols {
		for _, direction := range []string{"upload", "download"} {
			t.Run(protocol+"/"+direction, func(t *testing.T) {
				const idle = 150 * time.Millisecond
				client, target := openStream(t, protocol, idle)
				src, dst := client, target
				if direction == "download" {
					src, dst = target, client
				}
				for i := range 8 {
					if _, err := src.Write([]byte{byte(i)}); err != nil {
						t.Fatal(err)
					}
					var b [1]byte
					if _, err := io.ReadFull(dst, b[:]); err != nil || b[0] != byte(i) {
						t.Fatalf("active transfer: %v, %v", b, err)
					}
					time.Sleep(idle / 4)
				}
			})
		}
	}
}

func TestTCPHandlersAllowDisablingIdleTimeout(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			client, target := openStream(t, protocol, 0)
			time.Sleep(100 * time.Millisecond)
			if _, err := target.Write([]byte{7}); err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			if _, err := io.ReadFull(client, b[:]); err != nil || b[0] != 7 {
				t.Fatalf("unbounded stream: %v, %v", b, err)
			}
		})
	}
}

func TestHTTPKeepaliveReclaimsIdleClient(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer target.Close()
	svc := startService(t, "http", "", map[string]any{"idleTimeout": "80ms", "http.keepalive": true})
	conn, err := net.Dial("tcp", svc.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	req.Header.Set("Connection", "keep-alive")
	if err := req.WriteProxy(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("proxy response: %q, %v", body, err)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("idle keepalive connection remained open: %v", err)
	}
}

func TestHTTPIdleBodyCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(target.Close)
	svc := startService(t, "http", "", map[string]any{"idleTimeout": "80ms"})
	conn, err := net.Dial("tcp", svc.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(time.Second))
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	if err := req.WriteProxy(conn); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(response.Body, b[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle client left its upstream body reader alive")
	}
	response.Body.Close()
}

func TestSSHandshakeTimeout(t *testing.T) {
	svc := startService(t, "ss", "", map[string]any{"readTimeout": "50ms"})
	conn, err := net.Dial("tcp", svc.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var b [1]byte
	if _, err := conn.Read(b[:]); err != io.EOF {
		t.Fatalf("SS authentication retained an idle client: %v", err)
	}
}

func TestSSNoDelaySupportsServerFirstTraffic(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := target.Accept(); err == nil {
			accepted <- conn
		}
	}()
	svc := startService(t, "ss", "", nil)
	clientAddress := startSSClient(t, svc.Addr().String(), target.Addr().String(), true)
	client, err := net.Dial("tcp", clientAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(time.Second))
	select {
	case conn := <-accepted:
		defer conn.Close()
		if _, err := conn.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		var b [5]byte
		if _, err := io.ReadFull(client, b[:]); err != nil || string(b[:]) != "hello" {
			t.Fatalf("server-first greeting: %q, %v", b, err)
		}
	case <-time.After(time.Second):
		t.Fatal("nodelay did not send the SS target header")
	}
}

func TestServiceCloseReleasesPendingHandshakes(t *testing.T) {
	for _, protocol := range []string{"http", "socks5", "ss"} {
		t.Run(protocol, func(t *testing.T) {
			svc := startService(t, protocol, "", nil)
			conn, err := net.Dial("tcp", svc.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(time.Second))
			svc.Close()
			var b [1]byte
			if _, err := conn.Read(b[:]); err == nil {
				t.Fatal("closed service still accepted handshake traffic")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("service cancellation retained a handshake reader")
			}
		})
	}
}

func TestServiceCloseReleasesSOCKSUDPControl(t *testing.T) {
	svc := startService(t, "socks5", "", map[string]any{"udp": true, "notls": true})
	raw, err := net.Dial("tcp", svc.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(time.Second))
	c := socksconnector.NewConnector(connector.LoggerOption(xlogger.Nop()))
	c.Init(xmd.NewMetadata(map[string]any{"notls": true}))
	conn, err := c.(connector.Handshaker).Handshake(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := gosocks5.NewRequest(gosocks5.CmdUdp, &gosocks5.Addr{Type: gosocks5.AddrIPv4, Host: "0.0.0.0"}).Write(conn); err != nil {
		t.Fatal(err)
	}
	response, err := gosocks5.ReadReply(conn)
	if err != nil || response.Rep != gosocks5.Succeeded {
		t.Fatalf("UDP associate failed: %v, %v", response, err)
	}
	conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	var b [1]byte
	if _, err := conn.Read(b[:]); err == io.EOF {
		t.Fatal("UDP control closed before service shutdown")
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	svc.Close()
	if _, err := conn.Read(b[:]); err != io.EOF {
		t.Fatalf("cancelled service retained UDP control connection: %v", err)
	}
}

func TestSOCKSBindTimesOutDataButNotWaitingControl(t *testing.T) {
	svc := startService(t, "socks5", "", map[string]any{"bind": true, "notls": true, "idleTimeout": "100ms"})
	raw, err := net.Dial("tcp", svc.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(2 * time.Second))
	c := socksconnector.NewConnector(connector.LoggerOption(xlogger.Nop()))
	c.Init(xmd.NewMetadata(map[string]any{"notls": true}))
	control, err := c.(connector.Handshaker).Handshake(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := gosocks5.NewRequest(gosocks5.CmdBind, &gosocks5.Addr{Type: gosocks5.AddrIPv4, Host: "127.0.0.1"}).Write(control); err != nil {
		t.Fatal(err)
	}
	response, err := gosocks5.ReadReply(control)
	if err != nil || response.Rep != gosocks5.Succeeded {
		t.Fatalf("bind: %v, %v", response, err)
	}
	time.Sleep(250 * time.Millisecond)
	incoming, err := net.Dial("tcp", response.Addr.String())
	if err != nil {
		t.Fatalf("waiting BIND control was treated as a data stream: %v", err)
	}
	defer incoming.Close()
	incoming.SetDeadline(time.Now().Add(time.Second))
	response, err = gosocks5.ReadReply(control)
	if err != nil || response.Rep != gosocks5.Succeeded {
		t.Fatalf("accepted bind: %v, %v", response, err)
	}
	var b [1]byte
	if _, err := incoming.Read(b[:]); err != io.EOF {
		t.Fatalf("idle BIND data stream was retained: %v", err)
	}
}

func TestTCPResourceMetadata(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			_, err := serviceparser.ParseService(&config.ServiceConfig{
				Name: t.Name(), Addr: "127.0.0.1:0", Listener: &config.ListenerConfig{Type: "tcp"},
				Handler: &config.HandlerConfig{Type: protocol, Metadata: map[string]any{"idleTimeout": "-1s"}},
			})
			if err == nil {
				t.Fatal("negative timeout must be rejected")
			}
		})
	}
	options, err := transport.Parse(nil, transport.DefaultIdleTimeout)
	if err != nil || options.IdleTimeout != 5*time.Minute || options.BufferSize != 16*1024 {
		t.Fatalf("defaults: %+v, %v", options, err)
	}
}

func TestUDPForwardPreservesDatagramsAndSession(t *testing.T) {
	target, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	peers := make(chan string, 2)
	go func() {
		buffer := make([]byte, 65536)
		for {
			n, peer, err := target.ReadFrom(buffer)
			if err != nil {
				return
			}
			peers <- peer.String()
			target.WriteTo(buffer[:n], peer)
		}
	}()
	svc, err := serviceparser.ParseService(&config.ServiceConfig{
		Name: t.Name(), Addr: "127.0.0.1:0",
		Listener:  &config.ListenerConfig{Type: "udp", Metadata: map[string]any{"keepalive": true, "ttl": "2s", "udp.bufferSize": 65536}},
		Handler:   &config.HandlerConfig{Type: "udp", Metadata: map[string]any{"idleTimeout": "30ms", "tcp.bufferSize": 1024}},
		Forwarder: &config.ForwarderConfig{Nodes: []*config.ForwardNodeConfig{{Name: "target", Addr: target.LocalAddr().String()}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	go svc.Serve()
	t.Cleanup(func() { svc.Close() })
	client, err := net.Dial("udp", svc.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	size := 24 * 1024
	if runtime.GOOS == "darwin" {
		size = 8 * 1024
	}
	payload := bytes.Repeat([]byte{42}, size)
	reply := make([]byte, 65536)
	var first string
	for i := range 2 {
		if _, err := client.Write(payload); err != nil {
			t.Fatal(err)
		}
		n, err := client.Read(reply)
		if err != nil || !bytes.Equal(reply[:n], payload) {
			t.Fatalf("UDP payload was truncated: %d/%d, %v", n, size, err)
		}
		peer := <-peers
		if i == 0 {
			first = peer
		} else if peer != first {
			t.Fatalf("TCP idle policy replaced an active UDP association: %s -> %s", first, peer)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("preserved %d-byte datagrams and the UDP association", size)
}

func TestInvalidHandlerMetadataReleasesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	_, err = serviceparser.ParseService(&config.ServiceConfig{
		Name: t.Name(), Addr: address, Listener: &config.ListenerConfig{Type: "tcp"},
		Handler: &config.HandlerConfig{Type: "http", Metadata: map[string]any{"idleTimeout": "-1s"}},
	})
	if err == nil {
		t.Fatal("invalid timeout was accepted")
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("failed handler initialization leaked its listening socket: %v", err)
	}
	listener.Close()
}
