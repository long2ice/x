package openvpn

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClientInterop drives a full handshake against a stock `openvpn`
// server. It is skipped unless the test rig under /tmp/ovpn-test exists
// (see the project setup); start the server with:
//
//	openvpn --config /tmp/ovpn-test/server.conf
func TestClientInterop(t *testing.T) {
	dir := "/tmp/ovpn-test"
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Skip("openvpn interop rig not present at", dir)
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

	conn, err := net.Dial("udp", "127.0.0.1:1194")
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}

	client, err := NewClient(&ClientConfig{
		Proto:    "udp",
		Cipher:   "AES-256-GCM",
		Auth:     "SHA256",
		CA:       read("ca.crt"),
		Cert:     read("client.crt"),
		Key:      read("client.key"),
		TLSCrypt: tlsCryptKey,
	}, NewDatagramPacketIO(conn))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	push, err := client.Handshake(ctx)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if len(push.Prefixes) == 0 {
		t.Fatalf("push reply has no assigned address")
	}
	t.Logf("handshake OK: assigned=%v peer-id=%d cipher=%s", push.Prefixes, push.PeerID, push.Cipher)

	// Data-channel interop: send our keepalive ping (verifies the send
	// key + AEAD wire format), then read+decrypt the server's keepalive
	// ping straight off the mux (ReadIPPacket would filter it out). The
	// server config sets `keepalive 10 60` so it emits one within ~10s.
	if err := client.WriteIPPacket(ctx, KeepalivePing); err != nil {
		t.Fatalf("write data-channel ping: %v", err)
	}

	dataCtx, dataCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer dataCancel()
	wire, err := client.mux.ReadDataPacket(dataCtx)
	if err != nil {
		t.Fatalf("read data packet from server: %v", err)
	}
	plain, err := client.data.Load().Decrypt(wire)
	if err != nil {
		t.Fatalf("decrypt server data packet: %v", err)
	}
	if !IsKeepalive(plain) {
		t.Fatalf("expected keepalive ping, got %x", plain)
	}
	t.Logf("data channel OK: decrypted server keepalive ping %x", plain)
}
