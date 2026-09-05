package reality

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	xmd "github.com/go-gost/x/metadata"
	"github.com/pires/go-proxyproto"
	utls "github.com/refraction-networking/utls"
	"github.com/xtls/reality"
	"golang.org/x/crypto/hkdf"
)

// Authenticate a real TLS 1.3 REALITY session, then keep sending application
// data after the setup timeout and listener.Close. This catches accidentally
// retaining the cancellation callback or a deadline on handed-off sockets.
func TestAuthenticatedConnectionSurvivesListenerClose(t *testing.T) {
	for _, pp := range []int{0, 1, 2} {
		t.Run(strconv.Itoa(pp), func(t *testing.T) { testAuthenticatedConnection(t, pp) })
	}
}

func testAuthenticatedConnection(t *testing.T, pp int) {
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dest.TLS = &tls.Config{MinVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519}}
	dest.StartTLS()
	defer dest.Close()
	addr := dest.Listener.Addr().String()
	for alpn := range 3 {
		key := addr + " example.com " + strconv.Itoa(alpn)
		reality.GlobalPostHandshakeRecordsLens.Store(key, []int{})
		t.Cleanup(func() { reality.GlobalPostHandshakeRecordsLens.Delete(key) })
	}
	private, public, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	l := NewListener(listener.AddrOption("127.0.0.1:0"), listener.LoggerOption(logger.Default()), listener.ProxyProtocolOption(pp))
	if err := l.Init(xmd.NewMetadata(map[string]any{"privateKey": private, "dest": addr, "serverNames": "example.com", "handshakeTimeout": "1s"})); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// Exercise the complete PROXY -> admission -> REALITY wrapper chain,
	// with slow clients already occupying the first two accepted sockets.
	for range 2 {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); accepted <- c }()
	raw, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if pp != 0 {
		if _, err := proxyproto.HeaderProxyFromAddrs(byte(pp), raw.LocalAddr(), raw.RemoteAddr()).WriteTo(raw); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := authenticatedClient(t, raw, public)
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	var server net.Conn
	select {
	case server = <-accepted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if server == nil {
		t.Fatal("authenticated session was not accepted")
	}
	defer server.Close()
	l.Close()
	time.Sleep(1100 * time.Millisecond)
	client.SetDeadline(time.Now().Add(time.Second))
	server.SetDeadline(time.Now().Add(time.Second))
	go server.Write([]byte("alive"))
	b := make([]byte, 5)
	if _, err := io.ReadFull(client, b); err != nil || string(b) != "alive" {
		t.Fatalf("session was closed after setup: %q %v", b, err)
	}
	go client.Write([]byte("reply"))
	if _, err := io.ReadFull(server, b); err != nil || string(b) != "reply" {
		t.Fatalf("reverse direction failed: %q %v", b, err)
	}
}

// The client follows the REALITY session-ID authentication format used by
// Xray-core. Certificate verification is mandatory here: accepting the dest's
// real certificate would make this test pass through unauthenticated fallback.
func authenticatedClient(t *testing.T, raw net.Conn, public string) *utls.UConn {
	t.Helper()
	var authKey []byte
	c := utls.UClient(raw, &utls.Config{ServerName: "example.com", InsecureSkipVerify: true, SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("missing REALITY certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			pub, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok {
				return fmt.Errorf("not a REALITY certificate")
			}
			h := hmac.New(sha512.New, authKey)
			h.Write(pub)
			if !bytes.Equal(h.Sum(nil), cert.Signature) {
				return fmt.Errorf("invalid REALITY authentication")
			}
			return nil
		}}, utls.HelloChrome_Auto)
	if err := c.BuildHandshakeState(); err != nil {
		t.Fatal(err)
	}
	hello := c.HandshakeState.Hello
	hello.SessionId = make([]byte, 32)
	copy(hello.Raw[39:], hello.SessionId)
	hello.SessionId[0] = 1
	binary.BigEndian.PutUint32(hello.SessionId[4:], uint32(time.Now().Unix()))
	b, err := decodeKey(public)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		t.Fatal(err)
	}
	key := c.HandshakeState.State13.KeyShareKeys.Ecdhe
	if key == nil {
		key = c.HandshakeState.State13.KeyShareKeys.MlkemEcdhe
	}
	if key == nil {
		t.Fatal("missing X25519 key share")
	}
	authKey, err = key.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(hkdf.New(sha256.New, authKey, hello.Random[:20], []byte("REALITY")), authKey); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(authKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	aead.Seal(hello.SessionId[:0], hello.Random[20:], hello.SessionId[:16], hello.Raw)
	copy(hello.Raw[39:], hello.SessionId)
	return c
}
