package openvpn

// PeerIDBroadcast is the all-ones 24-bit peer-id used in early P_DATA_V2
// packets before the server assigns a real one.
const PeerIDBroadcast uint32 = 0xFFFFFF

// DataPacket is the framed but still-encrypted data-channel packet. The
// AEAD layer (Phase 6) consumes/produces Payload.
//
// Wire layout:
//
//	opcode|key_id   1B
//	[peer_id        3B]   (only for P_DATA_V2)
//	payload         var
type DataPacket struct {
	Opcode  Opcode
	KeyID   uint8
	PeerID  uint32
	Payload []byte
}

func (p *DataPacket) Encode(buf []byte) ([]byte, error) {
	if !p.Opcode.IsData() {
		return nil, ErrWrongOpcode
	}
	n := 1 + len(p.Payload)
	if p.Opcode == PDataV2 {
		n += 3
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	i := 0
	buf[i] = EncodeHeader(p.Opcode, p.KeyID)
	i++
	if p.Opcode == PDataV2 {
		buf[i] = byte(p.PeerID >> 16)
		buf[i+1] = byte(p.PeerID >> 8)
		buf[i+2] = byte(p.PeerID)
		i += 3
	}
	copy(buf[i:], p.Payload)
	return buf, nil
}

func DecodeDataPacket(b []byte) (*DataPacket, error) {
	if len(b) < 1 {
		return nil, ErrShortPacket
	}
	op, keyID := DecodeHeader(b[0])
	if !op.IsData() {
		return nil, ErrWrongOpcode
	}
	p := &DataPacket{Opcode: op, KeyID: keyID}
	i := 1
	if op == PDataV2 {
		if len(b) < i+3 {
			return nil, ErrShortPacket
		}
		p.PeerID = uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
		i += 3
	}
	if i < len(b) {
		p.Payload = append([]byte(nil), b[i:]...)
	}
	return p, nil
}
