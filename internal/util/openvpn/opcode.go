package openvpn

import "fmt"

type Opcode uint8

const (
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
	PControlWkcV1             Opcode = 11
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
	case PControlWkcV1:
		return "P_CONTROL_WKC_V1"
	}
	return fmt.Sprintf("P_UNKNOWN(%d)", uint8(o))
}

func (o Opcode) IsControl() bool {
	switch o {
	case PControlHardResetClientV1, PControlHardResetServerV1, PControlSoftResetV1,
		PControlV1, PControlHardResetClientV2, PControlHardResetServerV2,
		PControlHardResetClientV3, PControlWkcV1:
		return true
	}
	return false
}

func (o Opcode) IsAck() bool { return o == PAckV1 }

func (o Opcode) IsData() bool { return o == PDataV1 || o == PDataV2 }

// First byte of every OpenVPN packet is (opcode << 3) | key_id.
func EncodeHeader(op Opcode, keyID uint8) byte {
	return (byte(op) << 3) | (keyID & 0x07)
}

func DecodeHeader(b byte) (Opcode, uint8) {
	return Opcode(b >> 3), b & 0x07
}
