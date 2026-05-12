package openvpn

import (
	"bytes"
	"testing"
)

func runHandshakeStateMachines(t *testing.T, psk []byte) (*Handshake, *Handshake) {
	t.Helper()
	cli := NewClientHandshake(psk)
	srv := NewServerHandshake(psk)

	clientHello, err := cli.Initial()
	if err != nil {
		t.Fatal(err)
	}
	if len(clientHello) != HandshakeClientHelloSize {
		t.Fatalf("client hello size %d, want %d", len(clientHello), HandshakeClientHelloSize)
	}

	serverHello, done, err := srv.Receive(clientHello)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("server should not be done after ClientHello")
	}
	if len(serverHello) != HandshakeServerHelloSize {
		t.Fatalf("server hello size %d, want %d", len(serverHello), HandshakeServerHelloSize)
	}

	clientFinish, done, err := cli.Receive(serverHello)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("client should not be done after ServerHello")
	}
	if len(clientFinish) != HandshakeClientFinishSize {
		t.Fatalf("client finish size %d, want %d", len(clientFinish), HandshakeClientFinishSize)
	}

	serverFinish, done, err := srv.Receive(clientFinish)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("server should be done after ClientFinish")
	}
	if len(serverFinish) != HandshakeServerFinishSize {
		t.Fatalf("server finish size %d, want %d", len(serverFinish), HandshakeServerFinishSize)
	}

	_, done, err = cli.Receive(serverFinish)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("client should be done after ServerFinish")
	}
	return cli, srv
}

func TestHandshakeFull(t *testing.T) {
	cli, srv := runHandshakeStateMachines(t, []byte("shared psk for tests"))
	if !cli.Done() || !srv.Done() {
		t.Errorf("both ends should be done")
	}
	ckey := cli.SessionKey()
	skey := srv.SessionKey()
	if !bytes.Equal(ckey, skey) {
		t.Errorf("session keys differ")
	}
	if len(ckey) != HandshakeSessionKeySize {
		t.Errorf("session key size %d, want %d", len(ckey), HandshakeSessionKeySize)
	}
}

func TestHandshakeDifferentNoncesProduceDifferentKeys(t *testing.T) {
	cli1, _ := runHandshakeStateMachines(t, []byte("psk"))
	cli2, _ := runHandshakeStateMachines(t, []byte("psk"))
	if bytes.Equal(cli1.SessionKey(), cli2.SessionKey()) {
		t.Errorf("two handshakes with same PSK should yield different session keys")
	}
}

func TestHandshakeWrongPSKFailsAtFinish(t *testing.T) {
	cli := NewClientHandshake([]byte("alice-psk"))
	srv := NewServerHandshake([]byte("bob-psk"))

	clientHello, _ := cli.Initial()
	serverHello, _, err := srv.Receive(clientHello)
	if err != nil {
		t.Fatal(err)
	}
	clientFinish, _, err := cli.Receive(serverHello)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.Receive(clientFinish); err != ErrHandshakeBadFinish {
		t.Errorf("wrong PSK should fail with bad finish, got %v", err)
	}
}

func TestHandshakeWrongSizeRejected(t *testing.T) {
	srv := NewServerHandshake([]byte("psk"))
	if _, _, err := srv.Receive(make([]byte, HandshakeClientHelloSize-1)); err != ErrHandshakeWrongSize {
		t.Errorf("short ClientHello should be rejected, got %v", err)
	}
}

func TestHandshakeTamperedClientFinish(t *testing.T) {
	psk := []byte("psk")
	cli := NewClientHandshake(psk)
	srv := NewServerHandshake(psk)

	clientHello, _ := cli.Initial()
	serverHello, _, _ := srv.Receive(clientHello)
	clientFinish, _, _ := cli.Receive(serverHello)
	clientFinish[0] ^= 0x01

	if _, _, err := srv.Receive(clientFinish); err != ErrHandshakeBadFinish {
		t.Errorf("tampered finish should be rejected, got %v", err)
	}
}

func TestHandshakeInitialOnlyOnce(t *testing.T) {
	cli := NewClientHandshake([]byte("psk"))
	if _, err := cli.Initial(); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Initial(); err != ErrHandshakeWrongState {
		t.Errorf("second Initial should fail, got %v", err)
	}
}

func TestHandshakeServerCantCallInitial(t *testing.T) {
	srv := NewServerHandshake([]byte("psk"))
	if _, err := srv.Initial(); err != ErrHandshakeWrongState {
		t.Errorf("server Initial should fail, got %v", err)
	}
}

func TestSessionKeyNilUntilDone(t *testing.T) {
	cli := NewClientHandshake([]byte("psk"))
	if cli.SessionKey() != nil {
		t.Errorf("session key should be nil before Done()")
	}
	_, _ = cli.Initial()
	if cli.SessionKey() != nil {
		t.Errorf("session key should be nil after only Initial()")
	}
}
