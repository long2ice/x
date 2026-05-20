package openvpn

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	xchain "github.com/go-gost/x/chain"
	xctx "github.com/go-gost/x/ctx"
	tungo "github.com/go-gost/x/handler/tungo"
	ovpn "github.com/go-gost/x/internal/util/openvpn"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"
)

// TestEndToEndProxy wires the real go-gost openvpn listener to the real
// tungo handler and proxies a UDP datagram through the tunnel:
//
//	test client (ovpn.Client) --crafted UDP/IP packet-->
//	openvpn listener --> tungo (gVisor netstack) --> dials echo server
//	--> reply travels all the way back.
func TestEndToEndProxy(t *testing.T) {
	dir := "/tmp/ovpn-test"
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Skip("openvpn interop rig not present at", dir)
	}
	logger.SetDefault(xlogger.Nop()) // chain.Router defaults to logger.Default()

	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return b
	}
	tlsCryptKey, err := ovpn.DecodeStaticKey(read("tc.key"))
	if err != nil {
		t.Fatalf("decode tls-crypt key: %v", err)
	}

	// --- target: a UDP echo server tungo will dial ---
	// gVisor drops packets with a loopback destination on a non-loopback
	// NIC ("martian"), so the echo server must live on a real local IP.
	echoIP := localIPv4(t)
	echo, err := net.ListenPacket("udp4", net.JoinHostPort(echoIP.String(), "0"))
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echo.Close()
	echoPort := uint16(echo.LocalAddr().(*net.UDPAddr).Port)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(buf[:n], addr)
		}
	}()

	// --- server: openvpn listener + tungo handler ---
	ln := NewListener(
		listener.AddrOption("127.0.0.1:0"),
		listener.LoggerOption(xlogger.Nop()),
	)
	if err := ln.Init(xmd.NewMetadata(map[string]any{
		"udp":      true,
		"ca":       filepath.Join(dir, "ca.crt"),
		"cert":     filepath.Join(dir, "server.crt"),
		"key":      filepath.Join(dir, "server.key"),
		"tlsCrypt": filepath.Join(dir, "tc.key"),
		"server":   "10.8.0.0/24",
	})); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	defer ln.Close()

	h := tungo.NewHandler(
		handler.RouterOption(xchain.NewRouter()),
		handler.LoggerOption(xlogger.Nop()),
	)
	if err := h.Init(xmd.NewMetadata(map[string]any{})); err != nil {
		t.Fatalf("tungo init: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			ctx := context.Background()
			if cv, ok := conn.(xctx.Context); ok {
				if v := cv.Context(); v != nil {
					ctx = v
				}
			}
			go h.Handle(ctx, conn)
		}
	}()

	// --- client: ovpn.Client straight onto the listener ---
	raw, err := net.Dial("udp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	cli, err := ovpn.NewClient(&ovpn.ClientConfig{
		Proto: "udp", Cipher: "AES-256-GCM", Auth: "SHA256",
		CA: read("ca.crt"), Cert: read("client.crt"), Key: read("client.key"),
		TLSCrypt: tlsCryptKey,
	}, ovpn.NewDatagramPacketIO(raw))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	push, err := cli.Handshake(ctx)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	clientIP := push.Prefixes[0].Addr()
	t.Logf("tunnel up, client IP %s", clientIP)

	// --- craft a UDP/IPv4 datagram destined for the echo server ---
	payload := []byte("ping-through-openvpn-tunnel")
	const srcPort = 40000
	pkt := buildUDP4(clientIP, echoIP, srcPort, echoPort, payload)

	got := make(chan []byte, 1)
	go func() {
		for {
			ip, err := cli.ReadIPPacket(ctx)
			if err != nil {
				return
			}
			if pl, sp, dp, ok := parseUDP4(ip); ok && sp == echoPort && dp == srcPort {
				got <- pl
				return
			}
		}
	}()

	// Retransmit a few times; the gVisor stack may still be coming up.
	for i := 0; i < 5; i++ {
		if err := cli.WriteIPPacket(ctx, pkt); err != nil {
			t.Fatalf("write ip packet: %v", err)
		}
		select {
		case pl := <-got:
			if string(pl) != string(payload) {
				t.Fatalf("echo mismatch: got %q want %q", pl, payload)
			}
			t.Logf("end-to-end OK: %q proxied through openvpn tunnel + tungo to echo server", pl)
			return
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatal("no echo reply received through the tunnel")
}

// buildUDP4 builds a UDP-over-IPv4 datagram with valid IP and UDP checksums.
func buildUDP4(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	s, d := src.As4(), dst.As4()
	total := 20 + 8 + len(payload)
	p := make([]byte, total)
	// IPv4 header.
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:], uint16(total))
	p[8] = 64 // TTL
	p[9] = 17 // UDP
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	binary.BigEndian.PutUint16(p[10:], onesComplement(p[:20]))
	// UDP header.
	udp := p[20:]
	binary.BigEndian.PutUint16(udp[0:], sport)
	binary.BigEndian.PutUint16(udp[2:], dport)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(payload)))
	copy(udp[8:], payload)
	// UDP checksum over the pseudo-header + UDP segment.
	pseudo := make([]byte, 12+len(udp))
	copy(pseudo[0:4], s[:])
	copy(pseudo[4:8], d[:])
	pseudo[9] = 17
	binary.BigEndian.PutUint16(pseudo[10:], uint16(len(udp)))
	copy(pseudo[12:], udp)
	csum := onesComplement(pseudo)
	if csum == 0 {
		csum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:], csum)
	return p
}

func parseUDP4(pkt []byte) (payload []byte, sport, dport uint16, ok bool) {
	if len(pkt) < 28 || pkt[0]>>4 != 4 || pkt[9] != 17 {
		return nil, 0, 0, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+8 {
		return nil, 0, 0, false
	}
	udp := pkt[ihl:]
	ulen := int(binary.BigEndian.Uint16(udp[4:]))
	if ulen < 8 || ulen > len(udp) {
		return nil, 0, 0, false
	}
	return udp[8:ulen], binary.BigEndian.Uint16(udp[0:]), binary.BigEndian.Uint16(udp[2:]), true
}

// localIPv4 returns a non-loopback IPv4 address of this host (used so the
// proxied destination is not a martian address from gVisor's view).
func localIPv4(t *testing.T) netip.Addr {
	c, err := net.Dial("udp", "8.8.8.8:80") // no packet sent; just route lookup
	if err != nil {
		t.Skip("no outbound route for a local IPv4:", err)
	}
	defer c.Close()
	addr := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr()
	if !addr.Is4() || addr.IsLoopback() {
		t.Skip("no usable non-loopback IPv4 address")
	}
	return addr
}

func onesComplement(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
