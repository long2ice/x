package wireguard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/go-gost/core/dialer"
	corelistener "github.com/go-gost/core/listener"
	net_dialer "github.com/go-gost/x/internal/net/dialer"
	xlogger "github.com/go-gost/x/logger"
	listenerpkg "github.com/go-gost/x/listener/wireguard"
	xmd "github.com/go-gost/x/metadata"
	"golang.org/x/crypto/curve25519"
)

type keypair struct {
	priv string // base64
	pub  string // base64
}

func newKeypair(t *testing.T) keypair {
	t.Helper()
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Per RFC 7748 §5: clamp before deriving the public key.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	return keypair{
		priv: base64.StdEncoding.EncodeToString(priv[:]),
		pub:  base64.StdEncoding.EncodeToString(pub),
	}
}

// TestDialListenerHandshakeAndIO brings up a real wireguard listener and a
// real wireguard dialer, then exchanges a handcrafted IPv4 UDP datagram each
// way to prove the encrypted tunnel actually carries packets end to end.
func TestDialListenerHandshakeAndIO(t *testing.T) {
	testHandshakeAndIO(t)
}

// TestDialListenerHandshakeAndIOWithNetDialer runs the same exchange with a
// NetDialer that has an interface bound, which forces the DialBind transport
// path (the one rule-mode routing relies on to keep the tunnel's own UDP
// packets out of the tunnel).
func TestDialListenerHandshakeAndIOWithNetDialer(t *testing.T) {
	testHandshakeAndIO(t, dialer.NetDialerDialOption(&net_dialer.Dialer{
		Interface: "127.0.0.1",
	}))
}

func testHandshakeAndIO(t *testing.T, dialOpts ...dialer.DialOption) {
	server := newKeypair(t)
	client := newKeypair(t)

	const (
		clientTunIP = "10.99.0.2"
		serverTunIP = "10.99.0.1"
	)

	// The listener's metadata parser requires a fixed listen port (it does
	// not surface the kernel-assigned one back via Addr), so pre-pick a free
	// UDP port and pass it in explicitly.
	port := freeUDPPort(t)

	l := listenerpkg.NewListener(
		corelistener.AddrOption(addrWithPort("127.0.0.1", port)),
		corelistener.LoggerOption(xlogger.Nop()),
	)
	if err := l.Init(xmd.NewMetadata(map[string]any{
		"privateKey": server.priv,
		"peers": []any{
			map[string]any{
				"publicKey":  client.pub,
				"allowedIPs": clientTunIP + "/32",
			},
		},
	})); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	defer l.Close()

	srvConn, err := l.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer srvConn.Close()

	d := NewDialer(dialer.LoggerOption(xlogger.Nop()))
	if err := d.Init(xmd.NewMetadata(map[string]any{
		"privateKey":          client.priv,
		"publicKey":           server.pub,
		"allowedIPs":          serverTunIP + "/32",
		"persistentKeepalive": 1,
	})); err != nil {
		t.Fatalf("dialer init: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cli, err := d.Dial(ctx, addrWithPort("127.0.0.1", port), dialOpts...)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	// Client -> server: a 0-payload IPv4 UDP datagram src=client, dst=server.
	clientPkt := buildUDPv4(t, clientTunIP, serverTunIP, 1111, 2222, nil)
	serverPkt := buildUDPv4(t, serverTunIP, clientTunIP, 2222, 1111, []byte("pong"))

	// Send periodically until the server receives one; the handshake adds
	// initial latency and some early packets may be dropped while it is
	// still in progress.
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 2048)
		_ = srvConn.SetReadDeadline(time.Now().Add(8 * time.Second))
		n, err := srvConn.Read(buf)
		if err != nil {
			return
		}
		done <- append([]byte(nil), buf[:n]...)
	}()

	send := time.NewTicker(200 * time.Millisecond)
	defer send.Stop()
	deadline := time.After(8 * time.Second)
loop:
	for {
		if _, err := cli.Write(clientPkt); err != nil {
			t.Fatalf("client write: %v", err)
		}
		select {
		case got := <-done:
			if !ipv4HeaderMatches(got, clientTunIP, serverTunIP) {
				t.Fatalf("server got packet that does not match expected src/dst: % x", got)
			}
			break loop
		case <-send.C:
		case <-deadline:
			t.Fatal("server did not receive a packet within 8s")
		}
	}

	// Server -> client.
	if _, err := srvConn.Write(serverPkt); err != nil {
		t.Fatalf("server write: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	// cli.SetReadDeadline returns an error since Conn does not support
	// deadlines, but blocking with a goroutine-based timeout works.
	gotCh := make(chan []byte, 1)
	go func() {
		n, err := cli.Read(buf)
		if err != nil {
			return
		}
		gotCh <- append([]byte(nil), buf[:n]...)
	}()
	select {
	case got := <-gotCh:
		if !ipv4HeaderMatches(got, serverTunIP, clientTunIP) {
			t.Fatalf("client got packet that does not match expected src/dst: % x", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not receive server packet within 5s")
	}
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	p := c.LocalAddr().(*net.UDPAddr).Port
	_ = c.Close()
	return p
}

func addrWithPort(host string, port int) string {
	return net.JoinHostPort(host, itoa(port))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [11]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// buildUDPv4 hand-assembles a minimal IPv4+UDP packet (no fragmentation, no
// IPv4 options, no UDP checksum) suitable for being sent across a wireguard
// tunnel where the receiving side only inspects src/dst addresses.
func buildUDPv4(t *testing.T, src, dst string, sport, dport uint16, payload []byte) []byte {
	t.Helper()
	srcIP := net.ParseIP(src).To4()
	dstIP := net.ParseIP(dst).To4()
	if srcIP == nil || dstIP == nil {
		t.Fatalf("invalid src/dst: %s %s", src, dst)
	}

	udpLen := 8 + len(payload)
	totalLen := 20 + udpLen

	pkt := make([]byte, totalLen)
	pkt[0] = 0x45               // IPv4, IHL=5
	pkt[1] = 0x00               // DSCP/ECN
	binary.BigEndian.PutUint16(pkt[2:4], uint16(totalLen))
	binary.BigEndian.PutUint16(pkt[4:6], 0) // ID
	binary.BigEndian.PutUint16(pkt[6:8], 0) // flags+frag
	pkt[8] = 64                             // TTL
	pkt[9] = 17                             // UDP
	// header checksum (offset 10..12) left zero for now
	copy(pkt[12:16], srcIP)
	copy(pkt[16:20], dstIP)
	// IPv4 header checksum
	binary.BigEndian.PutUint16(pkt[10:12], ipv4Checksum(pkt[:20]))

	binary.BigEndian.PutUint16(pkt[20:22], sport)
	binary.BigEndian.PutUint16(pkt[22:24], dport)
	binary.BigEndian.PutUint16(pkt[24:26], uint16(udpLen))
	binary.BigEndian.PutUint16(pkt[26:28], 0) // UDP checksum=0 (allowed for IPv4)
	copy(pkt[28:], payload)
	return pkt
}

func ipv4Checksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i : i+2]))
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

func ipv4HeaderMatches(pkt []byte, src, dst string) bool {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return false
	}
	gotSrc := net.IPv4(pkt[12], pkt[13], pkt[14], pkt[15]).String()
	gotDst := net.IPv4(pkt[16], pkt[17], pkt[18], pkt[19]).String()
	return gotSrc == src && gotDst == dst
}
