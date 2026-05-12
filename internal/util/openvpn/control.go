package openvpn

import (
	"encoding/binary"
	"errors"
)

var (
	ErrShortPacket  = errors.New("openvpn: short packet")
	ErrTooManyAcks  = errors.New("openvpn: ack count exceeds max")
	ErrWrongOpcode  = errors.New("openvpn: wrong opcode for decoder")
	ErrEncodeBuffer = errors.New("openvpn: encode buffer too small")
)

// OpenVPN allows up to 8 ACK IDs per packet (ack_count is a single byte but
// the C reference uses RELIABLE_ACK_SIZE = 8).
const MaxAckCount = 8

// ControlPacket is the on-the-wire form of a control-channel packet WITHOUT
// any tls-auth / tls-crypt wrapping (that layer is applied separately).
//
// Wire layout:
//
//	opcode|key_id   1B
//	session_id      8B
//	ack_count       1B
//	ack_ids         4B * ack_count
//	[remote_sid     8B]   (only if ack_count > 0)
//	[packet_id      4B]   (omitted for P_ACK_V1)
//	payload         var
type ControlPacket struct {
	Opcode    Opcode
	KeyID     uint8
	SessionID SessionID
	Acks      []uint32
	RemoteID  SessionID
	PacketID  uint32
	Payload   []byte
}

func (p *ControlPacket) encodedLen() int {
	n := 1 + 8 + 1 + 4*len(p.Acks)
	if len(p.Acks) > 0 {
		n += 8
	}
	if !p.Opcode.IsAck() {
		n += 4
	}
	n += len(p.Payload)
	return n
}

func (p *ControlPacket) Encode(buf []byte) ([]byte, error) {
	if !p.Opcode.IsControl() && !p.Opcode.IsAck() {
		return nil, ErrWrongOpcode
	}
	if len(p.Acks) > MaxAckCount {
		return nil, ErrTooManyAcks
	}
	n := p.encodedLen()
	if cap(buf) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	i := 0
	buf[i] = EncodeHeader(p.Opcode, p.KeyID)
	i++
	copy(buf[i:], p.SessionID[:])
	i += 8
	buf[i] = byte(len(p.Acks))
	i++
	for _, a := range p.Acks {
		binary.BigEndian.PutUint32(buf[i:], a)
		i += 4
	}
	if len(p.Acks) > 0 {
		copy(buf[i:], p.RemoteID[:])
		i += 8
	}
	if !p.Opcode.IsAck() {
		binary.BigEndian.PutUint32(buf[i:], p.PacketID)
		i += 4
	}
	copy(buf[i:], p.Payload)
	return buf, nil
}

func DecodeControlPacket(b []byte) (*ControlPacket, error) {
	if len(b) < 1 {
		return nil, ErrShortPacket
	}
	op, keyID := DecodeHeader(b[0])
	if !op.IsControl() && !op.IsAck() {
		return nil, ErrWrongOpcode
	}
	p := &ControlPacket{Opcode: op, KeyID: keyID}
	i := 1
	if len(b) < i+9 {
		return nil, ErrShortPacket
	}
	copy(p.SessionID[:], b[i:i+8])
	i += 8
	ackCount := int(b[i])
	i++
	if ackCount > MaxAckCount {
		return nil, ErrTooManyAcks
	}
	need := 4 * ackCount
	if ackCount > 0 {
		need += 8
	}
	if !op.IsAck() {
		need += 4
	}
	if len(b) < i+need {
		return nil, ErrShortPacket
	}
	if ackCount > 0 {
		p.Acks = make([]uint32, ackCount)
		for k := 0; k < ackCount; k++ {
			p.Acks[k] = binary.BigEndian.Uint32(b[i:])
			i += 4
		}
		copy(p.RemoteID[:], b[i:i+8])
		i += 8
	}
	if !op.IsAck() {
		p.PacketID = binary.BigEndian.Uint32(b[i:])
		i += 4
	}
	if i < len(b) {
		p.Payload = append([]byte(nil), b[i:]...)
	}
	return p, nil
}
