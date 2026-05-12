package openvpn

import (
	"bytes"
	"testing"
)

func newCipherPair(t *testing.T) (*ControlCipher, *ControlCipher) {
	t.Helper()
	secret := []byte("test shared secret abcdefghijklmnop")
	a, err := NewControlCipher(secret)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewControlCipher(secret)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func mustEncode(t *testing.T, p *ControlPacket) []byte {
	t.Helper()
	b, err := p.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTLSCryptRoundtrip(t *testing.T) {
	send, recv := newCipherPair(t)

	in := &ControlPacket{
		Opcode:    PControlV1,
		KeyID:     0,
		SessionID: SessionID{1, 2, 3, 4, 5, 6, 7, 8},
		Acks:      []uint32{5, 6},
		RemoteID:  SessionID{9, 9, 9, 9, 9, 9, 9, 9},
		PacketID:  17,
		Payload:   []byte("fake tls handshake bytes"),
	}
	plain := mustEncode(t, in)

	wire, err := send.Wrap(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != tlsCryptHeaderLen+len(plain)-9 {
		t.Errorf("wire length %d, want %d", len(wire), tlsCryptHeaderLen+len(plain)-9)
	}
	if !bytes.Equal(wire[:9], plain[:9]) {
		t.Errorf("opcode|sid prefix should be cleartext on the wire")
	}

	got, err := recv.Unwrap(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("roundtrip mismatch:\n got=%x\nwant=%x", got, plain)
	}

	out, err := DecodeControlPacket(got)
	if err != nil {
		t.Fatal(err)
	}
	if out.PacketID != in.PacketID || !bytes.Equal(out.Payload, in.Payload) {
		t.Errorf("decoded packet mismatch")
	}
}

func TestTLSCryptRejectsTamperedCiphertext(t *testing.T) {
	send, recv := newCipherPair(t)
	in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, Payload: []byte("xyz")}
	wire, err := send.Wrap(mustEncode(t, in))
	if err != nil {
		t.Fatal(err)
	}
	wire[len(wire)-1] ^= 0x01
	if _, err := recv.Unwrap(wire); err != ErrTLSCryptHMAC {
		t.Errorf("expected HMAC mismatch, got %v", err)
	}
}

func TestTLSCryptRejectsTamperedHMAC(t *testing.T) {
	send, recv := newCipherPair(t)
	in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, Payload: []byte("xyz")}
	wire, _ := send.Wrap(mustEncode(t, in))
	wire[17] ^= 0x01
	if _, err := recv.Unwrap(wire); err != ErrTLSCryptHMAC {
		t.Errorf("expected HMAC mismatch on tag flip, got %v", err)
	}
}

func TestTLSCryptRejectsTamperedSession(t *testing.T) {
	send, recv := newCipherPair(t)
	in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, Payload: []byte("xyz")}
	wire, _ := send.Wrap(mustEncode(t, in))
	wire[1] ^= 0x01 // flip a bit in session id (covered by HMAC)
	if _, err := recv.Unwrap(wire); err != ErrTLSCryptHMAC {
		t.Errorf("expected HMAC mismatch on sid flip, got %v", err)
	}
}

func TestTLSCryptWrongKey(t *testing.T) {
	send, _ := newCipherPair(t)
	wrong, err := NewControlCipher([]byte("wrong secret"))
	if err != nil {
		t.Fatal(err)
	}
	in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, Payload: []byte("xyz")}
	wire, _ := send.Wrap(mustEncode(t, in))
	if _, err := wrong.Unwrap(wire); err != ErrTLSCryptHMAC {
		t.Errorf("wrong key should fail HMAC, got %v", err)
	}
}

func TestTLSCryptReplayRejected(t *testing.T) {
	send, recv := newCipherPair(t)
	in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, Payload: []byte("xyz")}
	wire, _ := send.Wrap(mustEncode(t, in))

	if _, err := recv.Unwrap(wire); err != nil {
		t.Fatal(err)
	}
	if _, err := recv.Unwrap(wire); err != ErrTLSCryptReplay {
		t.Errorf("second unwrap of same packet should be ErrTLSCryptReplay, got %v", err)
	}
}

func TestTLSCryptOutOfOrderWithinWindow(t *testing.T) {
	send, recv := newCipherPair(t)
	var wires [][]byte
	for i := 0; i < 10; i++ {
		in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, PacketID: uint32(i), Payload: []byte{byte(i)}}
		w, err := send.Wrap(mustEncode(t, in))
		if err != nil {
			t.Fatal(err)
		}
		wires = append(wires, w)
	}
	// Receive reverse.
	for i := len(wires) - 1; i >= 0; i-- {
		if _, err := recv.Unwrap(wires[i]); err != nil {
			t.Errorf("out-of-order unwrap [%d]: %v", i, err)
		}
	}
}

func TestTLSCryptTooOldRejected(t *testing.T) {
	send, recv := newCipherPair(t)
	// Burn 100 send ids.
	var oldWire []byte
	for i := 0; i < 100; i++ {
		in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, Payload: []byte{byte(i)}}
		w, _ := send.Wrap(mustEncode(t, in))
		if i == 0 {
			oldWire = w
		}
	}
	// Deliver the latest first to set the high-water mark.
	in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}, Payload: []byte("now")}
	current, _ := send.Wrap(mustEncode(t, in))
	if _, err := recv.Unwrap(current); err != nil {
		t.Fatal(err)
	}
	// Now the old one is way outside the 64-slot window.
	if _, err := recv.Unwrap(oldWire); err != ErrTLSCryptReplay {
		t.Errorf("old packet should be replay-rejected, got %v", err)
	}
}

func TestTLSCryptShortFrame(t *testing.T) {
	_, recv := newCipherPair(t)
	if _, err := recv.Unwrap(make([]byte, tlsCryptHeaderLen-1)); err != ErrTLSCryptShort {
		t.Errorf("expected short frame error, got %v", err)
	}
}

func TestTLSCryptSendIDMonotonic(t *testing.T) {
	send, _ := newCipherPair(t)
	in := &ControlPacket{Opcode: PControlV1, SessionID: SessionID{1}}
	plain := mustEncode(t, in)
	for want := uint32(1); want <= 5; want++ {
		w, err := send.Wrap(plain)
		if err != nil {
			t.Fatal(err)
		}
		gotID := uint32(w[9])<<24 | uint32(w[10])<<16 | uint32(w[11])<<8 | uint32(w[12])
		if gotID != want {
			t.Errorf("send id %d, want %d", gotID, want)
		}
	}
}

func TestReplayWindowBasics(t *testing.T) {
	w := NewReplayWindow()
	if w.Accept(0) {
		t.Errorf("id 0 should be rejected")
	}
	if !w.Accept(1) {
		t.Errorf("id 1 should be accepted")
	}
	if w.Accept(1) {
		t.Errorf("duplicate id 1 should be rejected")
	}
	if !w.Accept(2) {
		t.Errorf("id 2 should be accepted")
	}
	if !w.Accept(100) {
		t.Errorf("id 100 (jump) should be accepted")
	}
	if w.Accept(35) {
		t.Errorf("id 35 (100-65) should be too old")
	}
	if !w.Accept(99) {
		t.Errorf("id 99 (within window) should be accepted")
	}
	if w.Accept(99) {
		t.Errorf("duplicate 99 should be rejected")
	}
}
