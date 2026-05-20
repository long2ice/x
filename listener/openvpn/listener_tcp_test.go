package openvpn

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-gost/core/listener"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"
)

// TestListenerInteropTCP runs a stock `openvpn` client over TCP against
// the go-gost openvpn listener in TCP mode.
func TestListenerInteropTCP(t *testing.T) {
	dir := "/tmp/ovpn-test"
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Skip("openvpn interop rig not present at", dir)
	}
	if _, err := os.Stat(openvpnBinary); err != nil {
		t.Skip("stock openvpn binary not found")
	}

	ln := NewListener(
		listener.AddrOption("127.0.0.1:0"),
		listener.LoggerOption(xlogger.Nop()),
	)
	if err := ln.Init(xmd.NewMetadata(map[string]any{
		"udp":      false, // TCP mode
		"ca":       filepath.Join(dir, "ca.crt"),
		"cert":     filepath.Join(dir, "server.crt"),
		"key":      filepath.Join(dir, "server.key"),
		"tlsCrypt": filepath.Join(dir, "tc.key"),
		"server":   "10.8.0.0/24",
	})); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port

	clientCfg := filepath.Join(t.TempDir(), "client.conf")
	conf := "client\ndev null\nproto tcp-client\n" +
		"remote 127.0.0.1 " + itoa(port) + "\n" +
		"ca " + filepath.Join(dir, "ca.crt") + "\n" +
		"cert " + filepath.Join(dir, "client.crt") + "\n" +
		"key " + filepath.Join(dir, "client.key") + "\n" +
		"tls-crypt " + filepath.Join(dir, "tc.key") + "\n" +
		"remote-cert-tls server\n" +
		"data-ciphers AES-256-GCM\ndata-ciphers-fallback AES-256-GCM\n" +
		"auth SHA256\nverb 3\n"
	if err := os.WriteFile(clientCfg, []byte(conf), 0o600); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	logFile := filepath.Join(t.TempDir(), "client.log")
	lf, _ := os.Create(logFile)
	cmd := exec.Command(openvpnBinary, "--config", clientCfg)
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

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		_ = conn.Close()
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listener accept: %v", err)
		}
		t.Log("TCP listener interop OK: stock client completed handshake over TCP")
	case <-time.After(25 * time.Second):
		t.Fatal("timed out waiting for stock TCP client")
	}
}
