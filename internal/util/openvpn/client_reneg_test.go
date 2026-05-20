package openvpn

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestClientRenegotiation drives a real TLS renegotiation: it connects to
// a stock `openvpn` server configured with a short reneg-sec, then checks
// the data channel switches to the new key_id and keeps interoperating.
func TestClientRenegotiation(t *testing.T) {
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

	// Stock server with a 30s renegotiation interval.
	const port = "11944"
	conf := "dev null\nproto udp\nlport " + port + "\n" +
		"ca " + filepath.Join(dir, "ca.crt") + "\n" +
		"cert " + filepath.Join(dir, "server.crt") + "\n" +
		"key " + filepath.Join(dir, "server.key") + "\n" +
		"dh none\ntls-crypt " + filepath.Join(dir, "tc.key") + "\n" +
		"topology subnet\nserver 10.8.0.0 255.255.255.0\n" +
		"data-ciphers AES-256-GCM\ndata-ciphers-fallback AES-256-GCM\n" +
		"auth SHA256\nkeepalive 10 60\nreneg-sec 30\nverb 3\n"
	confPath := filepath.Join(t.TempDir(), "server.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatalf("write server config: %v", err)
	}
	srv := exec.Command(openvpnBinary, "--config", confPath)
	srvLog := filepath.Join(t.TempDir(), "server.log")
	lf, _ := os.Create(srvLog)
	srv.Stdout, srv.Stderr = lf, lf
	if err := srv.Start(); err != nil {
		t.Fatalf("start openvpn server: %v", err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
	}()
	time.Sleep(2 * time.Second)

	conn, err := net.Dial("udp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cli, err := NewClient(&ClientConfig{
		Proto: "udp", Cipher: "AES-256-GCM", Auth: "SHA256",
		CA: read("ca.crt"), Cert: read("client.crt"), Key: read("client.key"), TLSCrypt: tlsCryptKey,
	}, NewDatagramPacketIO(conn))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer cli.Close()

	hctx, hcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer hcancel()
	if _, err := cli.Handshake(hctx); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if kid := cli.data.Load().keyID; kid != 0 {
		t.Fatalf("initial data channel key_id = %d, want 0", kid)
	}
	t.Log("handshake OK on key_id 0, waiting for renegotiation ...")

	// Watch the data channel: after renegotiation it switches to key_id
	// 1, and we must still decrypt the server's keepalive pings on it.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		dctx, dcancel := context.WithTimeout(context.Background(), 12*time.Second)
		wire, err := cli.mux.ReadDataPacket(dctx)
		dcancel()
		if err != nil {
			continue
		}
		d := cli.data.Load()
		plain, err := d.Decrypt(wire)
		if err != nil {
			continue // pre-reneg key_id, or transient
		}
		if d.keyID == 1 && IsKeepalive(plain) {
			t.Log("renegotiation OK: data channel switched to key_id 1 and decrypts server traffic")
			return
		}
	}
	t.Fatalf("no renegotiated (key_id 1) traffic decrypted; final key_id=%d", cli.data.Load().keyID)
}
