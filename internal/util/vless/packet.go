package vless

import (
	"encoding/binary"
	"io"
	"net"

	"github.com/go-gost/core/common/bufpool"
)

const maxPacketSize = 65535

type packetConn struct {
	net.Conn
	raddr net.Addr
}

// PacketConn converts a VLESS UDP stream into a net.PacketConn. Every packet
// is framed with a two byte big-endian length, and raddr, the target address
// carried by the request header, is the peer of every packet.
func PacketConn(c net.Conn, raddr net.Addr) net.PacketConn {
	return &packetConn{
		Conn:  c,
		raddr: raddr,
	}
}

func (c *packetConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.Conn, hdr[:]); err != nil {
		return
	}

	size := int(binary.BigEndian.Uint16(hdr[:]))
	if size <= len(b) {
		n, err = io.ReadFull(c.Conn, b[:size])
		return n, c.raddr, err
	}

	// The packet does not fit, truncate it and drop the remainder so that the
	// stream stays in sync.
	if n, err = io.ReadFull(c.Conn, b); err != nil {
		return n, c.raddr, err
	}
	_, err = io.CopyN(io.Discard, c.Conn, int64(size-len(b)))
	return n, c.raddr, err
}

func (c *packetConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	if len(b) > maxPacketSize {
		return 0, ErrBadPacket
	}

	buf := bufpool.Get(len(b) + 2)
	defer bufpool.Put(buf)

	binary.BigEndian.PutUint16(buf[:2], uint16(len(b)))
	copy(buf[2:], b)

	if _, err = c.Conn.Write(buf[:len(b)+2]); err != nil {
		return 0, err
	}
	return len(b), nil
}
