// Package openvpn implements the OpenVPN protocol (TLS mode, legacy
// key-method-2 key derivation) well enough to interoperate with the stock
// `openvpn` binary as both a client and a server.
//
// Layering, bottom to top:
//
//	packet.go    opcodes, session ids, control-packet wire format
//	tlscrypt.go  --tls-crypt control-channel wrapping
//	control.go   reliable control channel + net.Conn adapter + packet IO
//	keymethod.go key method 2 exchange + OpenVPN PRF key derivation
//	data.go      AEAD data channel (AES-GCM)
//	push.go      PUSH_REQUEST / PUSH_REPLY option strings
//	client.go    client-side handshake driver (dialer)
//	server.go    server-side handshake driver (listener)
package openvpn

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// Opcode is the 5-bit OpenVPN packet opcode (the high bits of the first
// byte; the low 3 bits carry the key id).
type Opcode uint8

const (
	OpcodeShift = 3
	KeyIDMask   = 0x07

	PControlHardResetClientV1 Opcode = 1
	PControlHardResetServerV1 Opcode = 2
	PControlSoftResetV1       Opcode = 3
	PControlV1                Opcode = 4
	PAckV1                    Opcode = 5
	PDataV1                   Opcode = 6
	PControlHardResetClientV2 Opcode = 7
	PControlHardResetServerV2 Opcode = 8
	PDataV2                   Opcode = 9
	PControlHardResetClientV3 Opcode = 10
	PControlWKCV1             Opcode = 11

	SessionIDSize = 8
)

func (o Opcode) String() string {
	switch o {
	case PControlHardResetClientV1:
		return "P_CONTROL_HARD_RESET_CLIENT_V1"
	case PControlHardResetServerV1:
		return "P_CONTROL_HARD_RESET_SERVER_V1"
	case PControlSoftResetV1:
		return "P_CONTROL_SOFT_RESET_V1"
	case PControlV1:
		return "P_CONTROL_V1"
	case PAckV1:
		return "P_ACK_V1"
	case PDataV1:
		return "P_DATA_V1"
	case PControlHardResetClientV2:
		return "P_CONTROL_HARD_RESET_CLIENT_V2"
	case PControlHardResetServerV2:
		return "P_CONTROL_HARD_RESET_SERVER_V2"
	case PDataV2:
		return "P_DATA_V2"
	case PControlHardResetClientV3:
		return "P_CONTROL_HARD_RESET_CLIENT_V3"
	case PControlWKCV1:
		return "P_CONTROL_WKC_V1"
	default:
		return fmt.Sprintf("P_UNKNOWN(%d)", uint8(o))
	}
}

func (o Opcode) IsControl() bool {
	switch o {
	case PControlHardResetClientV1, PControlHardResetServerV1, PControlSoftResetV1, PControlV1,
		PAckV1, PControlHardResetClientV2, PControlHardResetServerV2, PControlHardResetClientV3,
		PControlWKCV1:
		return true
	default:
		return false
	}
}

func (o Opcode) IsData() bool { return o == PDataV1 || o == PDataV2 }

// HasMessageID reports whether a control opcode carries a reliable
// message id and payload (everything except the bare P_ACK_V1).
func (o Opcode) HasMessageID() bool { return o.IsControl() && o != PAckV1 }

func opcodeKeyID(opcode Opcode, keyID uint8) byte {
	return byte(opcode)<<OpcodeShift | (keyID & KeyIDMask)
}

func parseOpcodeKeyID(b byte) (Opcode, uint8) {
	return Opcode(b >> OpcodeShift), b & KeyIDMask
}

// SessionID is the random 8-byte identifier each side picks for the
// lifetime of a connection.
type SessionID [SessionIDSize]byte

func NewSessionID() (SessionID, error) {
	var id SessionID
	_, err := rand.Read(id[:])
	return id, err
}

func (s SessionID) IsZero() bool { return s == SessionID{} }

// ControlPacket is a decoded control-channel packet, without the
// tls-crypt wrapping (Encode/DecodeControlPacket apply that).
//
// Plain (post-tls-crypt) layout:
//
//	ack_count       1B
//	ack_ids         4B * ack_count
//	[remote_sid     8B]   (only if ack_count > 0)
//	[message_id     4B]   (omitted for P_ACK_V1)
//	payload         var
//
// The opcode|key_id byte and the 8-byte local session id live in the
// tls-crypt header, not the plain body.
type ControlPacket struct {
	Opcode       Opcode
	KeyID        uint8
	LocalSession SessionID

	AckIDs           []uint32
	AckRemoteSession SessionID

	MessageID uint32
	Payload   []byte
}

func (p ControlPacket) encodePlain() ([]byte, error) {
	if !p.Opcode.IsControl() {
		return nil, fmt.Errorf("openvpn: opcode %s is not a control opcode", p.Opcode)
	}
	if len(p.AckIDs) > 255 {
		return nil, fmt.Errorf("openvpn: too many ack ids: %d", len(p.AckIDs))
	}
	size := 1 + len(p.AckIDs)*4
	if len(p.AckIDs) > 0 {
		size += SessionIDSize
	}
	if p.Opcode.HasMessageID() {
		size += 4 + len(p.Payload)
	}
	out := make([]byte, 0, size)
	out = append(out, byte(len(p.AckIDs)))
	for _, id := range p.AckIDs {
		out = binary.BigEndian.AppendUint32(out, id)
	}
	if len(p.AckIDs) > 0 {
		out = append(out, p.AckRemoteSession[:]...)
	}
	if p.Opcode.HasMessageID() {
		out = binary.BigEndian.AppendUint32(out, p.MessageID)
		out = append(out, p.Payload...)
	}
	return out, nil
}

func decodeControlPlain(opcode Opcode, plain []byte) (ackIDs []uint32, ackRemote SessionID, messageID uint32, payload []byte, err error) {
	if len(plain) < 1 {
		return nil, SessionID{}, 0, nil, errors.New("openvpn: control payload too short")
	}
	ackLen := int(plain[0])
	offset := 1
	if len(plain) < offset+ackLen*4 {
		return nil, SessionID{}, 0, nil, errors.New("openvpn: control ack array truncated")
	}
	ackIDs = make([]uint32, ackLen)
	for i := 0; i < ackLen; i++ {
		ackIDs[i] = binary.BigEndian.Uint32(plain[offset : offset+4])
		offset += 4
	}
	if ackLen > 0 {
		if len(plain) < offset+SessionIDSize {
			return nil, SessionID{}, 0, nil, errors.New("openvpn: control ack remote session truncated")
		}
		copy(ackRemote[:], plain[offset:offset+SessionIDSize])
		offset += SessionIDSize
	}
	if opcode.HasMessageID() {
		if len(plain) < offset+4 {
			return nil, SessionID{}, 0, nil, errors.New("openvpn: control message id truncated")
		}
		messageID = binary.BigEndian.Uint32(plain[offset : offset+4])
		offset += 4
		payload = cloneBytes(plain[offset:])
	} else if len(plain) != offset {
		return nil, SessionID{}, 0, nil, errors.New("openvpn: ack packet has trailing payload")
	}
	return ackIDs, ackRemote, messageID, payload, nil
}

// Encode wraps the packet with tls-crypt and returns the on-wire bytes.
func (p ControlPacket) Encode(crypt *TLSCrypt, packetID, unixTime uint32) ([]byte, error) {
	if crypt == nil {
		return nil, errors.New("openvpn: tls-crypt is required")
	}
	plain, err := p.encodePlain()
	if err != nil {
		return nil, err
	}
	header := make([]byte, tlsCryptHeaderSize)
	header[0] = opcodeKeyID(p.Opcode, p.KeyID)
	copy(header[1:], p.LocalSession[:])
	return crypt.Wrap(header, packetID, unixTime, plain)
}

// DecodeControlPacket unwraps tls-crypt and decodes one control packet.
func DecodeControlPacket(crypt *TLSCrypt, packet []byte) (*ControlPacket, error) {
	if crypt == nil {
		return nil, errors.New("openvpn: tls-crypt is required")
	}
	header, _, _, plain, err := crypt.Unwrap(packet)
	if err != nil {
		return nil, err
	}
	if len(header) != tlsCryptHeaderSize {
		return nil, fmt.Errorf("openvpn: invalid control header length %d", len(header))
	}
	opcode, keyID := parseOpcodeKeyID(header[0])
	if !opcode.IsControl() {
		return nil, fmt.Errorf("openvpn: opcode %s is not a control opcode", opcode)
	}
	var local SessionID
	copy(local[:], header[1:])
	ackIDs, ackRemote, messageID, payload, err := decodeControlPlain(opcode, plain)
	if err != nil {
		return nil, err
	}
	return &ControlPacket{
		Opcode:           opcode,
		KeyID:            keyID,
		LocalSession:     local,
		AckIDs:           ackIDs,
		AckRemoteSession: ackRemote,
		MessageID:        messageID,
		Payload:          payload,
	}, nil
}

func cloneBytes(in []byte) []byte {
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
