package openvpn

import (
	"bytes"
	"testing"
)

func endToEndSessionKey(t *testing.T) []byte {
	t.Helper()
	cli, _ := runHandshakeStateMachines(t, []byte("data-channel-psk"))
	return cli.SessionKey()
}

func newDataPair(t *testing.T) (clientSend, clientRecv, serverSend, serverRecv *DataCipher) {
	t.Helper()
	key := endToEndSessionKey(t)
	cs, cr, err := NewDataCipherPair(key, false)
	if err != nil {
		t.Fatal(err)
	}
	ss, sr, err := NewDataCipherPair(key, true)
	if err != nil {
		t.Fatal(err)
	}
	return cs, cr, ss, sr
}

func TestDataCipherRoundtripBothDirections(t *testing.T) {
	cs, cr, ss, sr := newDataPair(t)

	// Client → Server
	pt := []byte("hello from client")
	wire, err := cs.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sr.Open(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("c→s plaintext mismatch")
	}

	// Server → Client
	pt2 := []byte("ack from server")
	wire2, err := ss.Seal(pt2)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := cr.Open(wire2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, pt2) {
		t.Errorf("s→c plaintext mismatch")
	}
}

func TestDataCipherWireLayout(t *testing.T) {
	cs, _, _, _ := newDataPair(t)
	pt := []byte("x")
	wire, err := cs.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	wantLen := DataPacketIDSize + len(pt) + DataAEADTagSize
	if len(wire) != wantLen {
		t.Errorf("wire len %d, want %d", len(wire), wantLen)
	}
	gotID := uint32(wire[0])<<24 | uint32(wire[1])<<16 | uint32(wire[2])<<8 | uint32(wire[3])
	if gotID != 1 {
		t.Errorf("first packet id %d, want 1", gotID)
	}
}

func TestDataCipherTamperedCiphertext(t *testing.T) {
	cs, _, _, sr := newDataPair(t)
	wire, _ := cs.Seal([]byte("payload"))
	wire[len(wire)-1] ^= 0x01
	if _, err := sr.Open(wire); err != ErrDataAEAD {
		t.Errorf("tampered ct should fail AEAD, got %v", err)
	}
}

func TestDataCipherTamperedPacketIDDetected(t *testing.T) {
	cs, _, _, sr := newDataPair(t)
	wire, _ := cs.Seal([]byte("payload"))
	wire[0] ^= 0x01
	if _, err := sr.Open(wire); err != ErrDataAEAD {
		t.Errorf("tampered packet id should fail AEAD (AAD), got %v", err)
	}
}

func TestDataCipherWrongDirectionFails(t *testing.T) {
	cs, cr, _, _ := newDataPair(t)
	wire, _ := cs.Seal([]byte("payload"))
	// Try to open client-send output with client-recv (should fail — different keys)
	if _, err := cr.Open(wire); err != ErrDataAEAD {
		t.Errorf("wrong-direction open should fail, got %v", err)
	}
}

func TestDataCipherReplayRejected(t *testing.T) {
	cs, _, _, sr := newDataPair(t)
	wire, _ := cs.Seal([]byte("payload"))
	if _, err := sr.Open(wire); err != nil {
		t.Fatal(err)
	}
	if _, err := sr.Open(wire); err != ErrDataReplay {
		t.Errorf("replay should be rejected, got %v", err)
	}
}

func TestDataCipherOutOfOrderWithinWindow(t *testing.T) {
	cs, _, _, sr := newDataPair(t)
	var wires [][]byte
	for i := 0; i < 16; i++ {
		w, err := cs.Seal([]byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		wires = append(wires, w)
	}
	// Reverse delivery
	for i := len(wires) - 1; i >= 0; i-- {
		pt, err := sr.Open(wires[i])
		if err != nil {
			t.Errorf("reverse[%d]: %v", i, err)
			continue
		}
		if pt[0] != byte(i) {
			t.Errorf("reverse[%d] payload byte %d, want %d", i, pt[0], i)
		}
	}
}

func TestDataCipherTooOldRejected(t *testing.T) {
	cs, _, _, sr := newDataPair(t)
	// Save first packet, then advance well past the 64-slot window.
	first, _ := cs.Seal([]byte("old"))
	for i := 0; i < 80; i++ {
		_, _ = cs.Seal([]byte{byte(i)})
	}
	// Deliver the latest from cs to advance sr's high-water mark.
	current, _ := cs.Seal([]byte("now"))
	if _, err := sr.Open(current); err != nil {
		t.Fatal(err)
	}
	if _, err := sr.Open(first); err != ErrDataReplay {
		t.Errorf("very old packet should be replay-rejected, got %v", err)
	}
}

func TestDataCipherDifferentSessionsIncompatible(t *testing.T) {
	cs1, _, _, sr1 := newDataPair(t)
	_, _, _, sr2 := newDataPair(t)
	wire, _ := cs1.Seal([]byte("for session 1"))
	if _, err := sr1.Open(wire); err != nil {
		t.Fatal(err)
	}
	if _, err := sr2.Open(wire); err != ErrDataAEAD {
		t.Errorf("different-session open should fail, got %v", err)
	}
}

func TestDataCipherShortFrame(t *testing.T) {
	_, _, _, sr := newDataPair(t)
	if _, err := sr.Open(make([]byte, DataPacketIDSize+DataAEADTagSize-1)); err != ErrDataShort {
		t.Errorf("short frame should fail, got %v", err)
	}
}
