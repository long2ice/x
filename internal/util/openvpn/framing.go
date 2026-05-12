package openvpn

import (
	"encoding/binary"
	"errors"
	"io"
)

const MaxPacketSize = 65535

var ErrPacketTooLarge = errors.New("openvpn: packet too large for TCP framing")

// ReadFramedPacket reads one OpenVPN packet from a TCP stream: 2-byte
// big-endian length followed by that many bytes. A zero-length frame is
// valid (used by some keepalive paths) and returns a nil slice.
func ReadFramedPacket(r io.Reader) ([]byte, error) {
	var lb [2]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lb[:]))
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func WriteFramedPacket(w io.Writer, pkt []byte) error {
	if len(pkt) > MaxPacketSize {
		return ErrPacketTooLarge
	}
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(len(pkt)))
	if _, err := w.Write(lb[:]); err != nil {
		return err
	}
	if len(pkt) == 0 {
		return nil
	}
	_, err := w.Write(pkt)
	return err
}
