package openvpn

import (
	"context"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const openvpnBinary = "/opt/homebrew/opt/openvpn/sbin/openvpn"

// testServerPIO is a one-client UDP PacketIO for tests: it replays the
// first datagram (already consumed to learn the client address), then
// reads from the shared socket and writes back to that address.
type testServerPIO struct {
	pc    net.PacketConn
	raddr net.Addr

	mu    sync.Mutex
	first []byte
	sent  [][]byte
	recvd [][]byte
}

func (p *testServerPIO) ReadPacket(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	if p.first != nil {
		pkt := p.first
		p.first = nil
		p.recvd = append(p.recvd, cloneBytes(pkt))
		p.mu.Unlock()
		return pkt, nil
	}
	p.mu.Unlock()
	buf := make([]byte, 64*1024)
	n, _, err := p.pc.ReadFrom(buf)
	if err != nil {
		return nil, err
	}
	pkt := cloneBytes(buf[:n])
	p.mu.Lock()
	p.recvd = append(p.recvd, cloneBytes(pkt))
	p.mu.Unlock()
	return pkt, nil
}

func (p *testServerPIO) WritePacket(ctx context.Context, packet []byte) error {
	p.mu.Lock()
	p.sent = append(p.sent, cloneBytes(packet))
	p.mu.Unlock()
	_, err := p.pc.WriteTo(packet, p.raddr)
	return err
}

func (p *testServerPIO) Close() error        { return p.pc.Close() }
func (p *testServerPIO) LocalAddr() net.Addr { return p.pc.LocalAddr() }
func (p *testServerPIO) RemoteAddr() net.Addr {
	return p.raddr
}

// TestServerInterop runs our Go server against a stock `openvpn` client.
func TestServerInterop(t *testing.T) {
	dir := "/tmp/ovpn-test"
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Skip("openvpn interop rig not present at", dir)
	}
	if _, err := os.Stat(openvpnBinary); err != nil {
		t.Skip("stock openvpn binary not found")
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return b
	}
	tlsCryptKey, err := DecodeStaticKey(read("tc.key"))
	if err != nil {
		t.Fatalf("decode tls-crypt key: %v", err)
	}

	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer pc.Close()
	port := pc.LocalAddr().(*net.UDPAddr).Port

	cfg := &ServerConfig{
		Proto:    "udp",
		Cipher:   "AES-256-GCM",
		Auth:     "SHA256",
		CA:       read("ca.crt"),
		Cert:     read("server.crt"),
		Key:      read("server.key"),
		TLSCrypt: tlsCryptKey,
		Gateway:  netip.MustParseAddr("10.9.0.1"),
		Netmask:  netip.MustParseAddr("255.255.255.0"),
		TunMTU:   1500,
	}

	type result struct {
		pkt []byte
		err error
	}
	done := make(chan result, 1)
	pioCh := make(chan *testServerPIO, 1)
	go func() {
		buf := make([]byte, 64*1024)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			done <- result{err: err}
			return
		}
		pio := &testServerPIO{pc: pc, raddr: addr, first: cloneBytes(buf[:n])}
		pioCh <- pio
		sess, err := Accept(pio, cfg, netip.MustParseAddr("10.9.0.2"), 1)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer sess.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Read+decrypt a data packet straight off the mux: the stock
		// client (dev null) only emits keepalive pings, which
		// ReadIPPacket would filter out.
		wire, err := sess.mux.ReadDataPacket(ctx)
		if err != nil {
			done <- result{err: err}
			return
		}
		pkt, err := sess.data.Decrypt(wire)
		done <- result{pkt: pkt, err: err}
	}()

	// Stock openvpn client config pointed at our Go server.
	clientCfg := filepath.Join(t.TempDir(), "client.conf")
	if err := os.WriteFile(clientCfg, fmtClientConf(dir, port), 0o600); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	logFile := filepath.Join(t.TempDir(), "client.log")
	cmd := exec.Command(openvpnBinary, "--config", clientCfg)
	lf, _ := os.Create(logFile)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start openvpn client: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if log, err := os.ReadFile(logFile); err == nil {
			t.Logf("stock client log:\n%s", log)
		}
	}()

	dumpSent := func() {
		select {
		case pio := <-pioCh:
			pio.mu.Lock()
			defer pio.mu.Unlock()
			srvCrypt, _ := NewTLSCrypt(tlsCryptKey, false)
			for i, pkt := range pio.recvd {
				op, _ := parseOpcodeKeyID(pkt[0])
				note := ""
				if _, err := DecodeControlPacket(srvCrypt, pkt); err != nil && op.IsControl() {
					note = "  DECODE-FAIL: " + err.Error()
				}
				t.Logf("recv[%d] %-30s %d bytes%s", i, op, len(pkt), note)
			}
			for i, pkt := range pio.sent {
				op, _ := parseOpcodeKeyID(pkt[0])
				t.Logf("sent[%d] %-30s %d bytes", i, op, len(pkt))
			}
		default:
			t.Log("no pio captured")
		}
	}

	select {
	case r := <-done:
		if r.err != nil {
			dumpSent()
			t.Fatalf("server session: %v", r.err)
		}
		t.Logf("server interop OK: decrypted %d bytes from stock client: %x", len(r.pkt), r.pkt)
	case <-time.After(25 * time.Second):
		dumpSent()
		t.Fatal("timed out waiting for stock client handshake")
	}
}

func fmtClientConf(dir string, port int) []byte {
	return []byte("client\ndev null\nproto udp\n" +
		"remote 127.0.0.1 " + itoa(port) + "\n" +
		"ca " + filepath.Join(dir, "ca.crt") + "\n" +
		"cert " + filepath.Join(dir, "client.crt") + "\n" +
		"key " + filepath.Join(dir, "client.key") + "\n" +
		"tls-crypt " + filepath.Join(dir, "tc.key") + "\n" +
		"remote-cert-tls server\n" +
		"data-ciphers AES-256-GCM\ndata-ciphers-fallback AES-256-GCM\n" +
		"auth SHA256\nverb 4\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
