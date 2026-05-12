package openvpn

import (
	"bytes"
	"testing"
)

func TestHeaderRoundtrip(t *testing.T) {
	cases := []struct {
		op    Opcode
		keyID uint8
	}{
		{PControlHardResetClientV2, 0},
		{PControlV1, 3},
		{PAckV1, 0},
		{PDataV2, 7},
		{PControlHardResetClientV3, 5},
	}
	for _, c := range cases {
		b := EncodeHeader(c.op, c.keyID)
		op, keyID := DecodeHeader(b)
		if op != c.op || keyID != c.keyID {
			t.Errorf("roundtrip %v/%d -> %v/%d", c.op, c.keyID, op, keyID)
		}
	}
}

func TestControlPacketRoundtrip(t *testing.T) {
	in := &ControlPacket{
		Opcode:    PControlV1,
		KeyID:     0,
		SessionID: SessionID{1, 2, 3, 4, 5, 6, 7, 8},
		Acks:      []uint32{10, 11, 12},
		RemoteID:  SessionID{9, 9, 9, 9, 9, 9, 9, 9},
		PacketID:  42,
		Payload:   []byte("hello tls"),
	}
	enc, err := in.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeControlPacket(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Opcode != in.Opcode || out.KeyID != in.KeyID {
		t.Errorf("opcode/keyid mismatch")
	}
	if out.SessionID != in.SessionID {
		t.Errorf("session id mismatch")
	}
	if len(out.Acks) != len(in.Acks) {
		t.Fatalf("ack count mismatch: %d vs %d", len(out.Acks), len(in.Acks))
	}
	for i := range in.Acks {
		if out.Acks[i] != in.Acks[i] {
			t.Errorf("ack[%d] mismatch", i)
		}
	}
	if out.RemoteID != in.RemoteID {
		t.Errorf("remote id mismatch")
	}
	if out.PacketID != in.PacketID {
		t.Errorf("packet id mismatch")
	}
	if !bytes.Equal(out.Payload, in.Payload) {
		t.Errorf("payload mismatch")
	}
}

func TestControlPacketNoAcks(t *testing.T) {
	in := &ControlPacket{
		Opcode:    PControlHardResetClientV2,
		SessionID: SessionID{1, 2, 3, 4, 5, 6, 7, 8},
		PacketID:  0,
	}
	enc, err := in.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	// 1 (header) + 8 (sid) + 1 (ack=0) + 4 (packet_id) = 14
	if len(enc) != 14 {
		t.Fatalf("hard reset size %d, want 14", len(enc))
	}
	out, err := DecodeControlPacket(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Opcode != in.Opcode || out.SessionID != in.SessionID || len(out.Acks) != 0 {
		t.Errorf("hard reset roundtrip failed")
	}
}

func TestAckPacketHasNoPacketID(t *testing.T) {
	in := &ControlPacket{
		Opcode:    PAckV1,
		SessionID: SessionID{1},
		Acks:      []uint32{5},
		RemoteID:  SessionID{2},
	}
	enc, err := in.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	// 1 (header) + 8 (sid) + 1 (ack=1) + 4 (ack id) + 8 (remote sid) = 22
	if len(enc) != 22 {
		t.Errorf("ack size %d, want 22", len(enc))
	}
	out, err := DecodeControlPacket(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.PacketID != 0 {
		t.Errorf("ack packet should not have packet id")
	}
}

func TestControlPacketRejectsTooManyAcks(t *testing.T) {
	p := &ControlPacket{
		Opcode:    PControlV1,
		SessionID: SessionID{1},
		Acks:      make([]uint32, MaxAckCount+1),
	}
	if _, err := p.Encode(nil); err == nil {
		t.Errorf("encode should reject ack count > max")
	}
}

func TestControlPacketShort(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{},
		{EncodeHeader(PControlV1, 0)}, // header only
		{EncodeHeader(PControlV1, 0), 1, 2, 3, 4, 5, 6, 7, 8}, // sid but no ack count
	} {
		if _, err := DecodeControlPacket(b); err == nil {
			t.Errorf("decode should reject short packet len=%d", len(b))
		}
	}
}

func TestDataV2Roundtrip(t *testing.T) {
	in := &DataPacket{
		Opcode:  PDataV2,
		KeyID:   2,
		PeerID:  0x123456,
		Payload: []byte("encrypted ip packet"),
	}
	enc, err := in.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeDataPacket(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Opcode != in.Opcode || out.KeyID != in.KeyID || out.PeerID != in.PeerID {
		t.Errorf("data v2 header mismatch: got op=%v key=%d peer=%x", out.Opcode, out.KeyID, out.PeerID)
	}
	if !bytes.Equal(out.Payload, in.Payload) {
		t.Errorf("data v2 payload mismatch")
	}
}

func TestDataV1NoPeerID(t *testing.T) {
	in := &DataPacket{
		Opcode:  PDataV1,
		KeyID:   1,
		Payload: []byte("xyz"),
	}
	enc, err := in.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != 4 {
		t.Errorf("data v1 size %d, want 4", len(enc))
	}
	out, err := DecodeDataPacket(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.PeerID != 0 {
		t.Errorf("data v1 should have no peer id")
	}
}

func TestFramingRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	pkts := [][]byte{
		[]byte("openvpn over tcp"),
		nil,
		{0x01, 0x02, 0x03},
	}
	for _, p := range pkts {
		if err := WriteFramedPacket(&buf, p); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range pkts {
		got, err := ReadFramedPacket(&buf)
		if err != nil {
			t.Fatalf("pkt %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("pkt %d mismatch: %v vs %v", i, got, want)
		}
	}
}

func TestFramingRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	big := make([]byte, MaxPacketSize+1)
	if err := WriteFramedPacket(&buf, big); err == nil {
		t.Errorf("expected oversize error")
	}
}
