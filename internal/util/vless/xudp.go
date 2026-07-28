package vless

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/go-gost/core/common/bufpool"
)

// XUDP carries UDP inside a VLESS stream with the frame format of mux.cool.
// Clients switch to it whenever they need more than one destination on a
// connection, and always when the xtls-rprx-vision flow is on. A request asks
// for it with the mux command and this address:
const (
	MuxHost = "v1.mux.cool"
	MuxPort = 666
)

const (
	muxStatusNew       byte = 1
	muxStatusKeep      byte = 2
	muxStatusEnd       byte = 3
	muxStatusKeepAlive byte = 4

	muxOptionData byte = 1

	muxNetworkUDP byte = 2

	maxMuxMetaSize = 512
)

var (
	ErrBadMuxFrame = errors.New("vless: malformed mux frame")
	ErrMuxNotUDP   = errors.New("vless: only the UDP part of mux is supported")
)

// IsMuxAddr reports whether a mux request is the XUDP marker.
func IsMuxAddr(host string, port int) bool {
	return host == MuxHost && port == MuxPort
}

type xudpConn struct {
	net.Conn
	resolve func(string) (*net.UDPAddr, error)

	sessionID [2]byte
	target    string

	// requested keeps the address the client asked for, so that replies from
	// a resolved address are reported back under the name it used.
	mu        sync.Mutex
	requested map[string]net.Addr
}

// XUDPConn converts an XUDP stream into a net.PacketConn. Unlike a plain VLESS
// UDP stream it carries the destination of every packet, so one connection
// reaches any number of them.
//
// resolve turns the destination of a packet into a routable address, it may be
// nil to use the system resolver.
func XUDPConn(c net.Conn, resolve func(string) (*net.UDPAddr, error)) net.PacketConn {
	if resolve == nil {
		resolve = func(addr string) (*net.UDPAddr, error) {
			return net.ResolveUDPAddr("udp", addr)
		}
	}
	return &xudpConn{
		Conn:      c,
		resolve:   resolve,
		requested: make(map[string]net.Addr),
	}
}

func (c *xudpConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		meta, err := c.readMeta()
		if err != nil {
			return 0, nil, err
		}

		target, hasData, err := c.parseMeta(meta)
		if err != nil {
			return 0, nil, err
		}

		if !hasData {
			continue
		}

		n, err := c.readData(b)
		if err != nil {
			return 0, nil, err
		}
		if target == "" {
			// A keep alive frame, its data is not a packet.
			continue
		}

		addr, err := c.resolve(target)
		if err != nil {
			// One unresolvable destination must not tear the session down.
			continue
		}

		c.mu.Lock()
		if addr.String() != target {
			c.requested[addr.String()] = &muxAddr{target}
		}
		c.mu.Unlock()

		return n, addr, nil
	}
}

func (c *xudpConn) readMeta() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
		return nil, err
	}

	size := int(binary.BigEndian.Uint16(hdr[:]))
	if size < 4 || size > maxMuxMetaSize {
		return nil, ErrBadMuxFrame
	}

	meta := make([]byte, size)
	if _, err := io.ReadFull(c.Conn, meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// parseMeta returns the destination of the frame, an empty one if its data is
// to be discarded, and whether data follows.
func (c *xudpConn) parseMeta(meta []byte) (target string, hasData bool, err error) {
	hasData = meta[3]&muxOptionData != 0
	rest := meta[4:]

	switch meta[2] {
	case muxStatusNew:
		// The replies of a relay are written from another goroutine and carry
		// the session id back, so it is shared state.
		c.mu.Lock()
		copy(c.sessionID[:], meta[:2])
		c.mu.Unlock()

		if len(rest) < 1 {
			return "", false, ErrBadMuxFrame
		}
		if rest[0] != muxNetworkUDP {
			return "", false, ErrMuxNotUDP
		}
		// Anything left after the address is the global id of the client,
		// which only matters to a cone NAT it keeps on its own side.
		if c.target, _, err = readAddrPort(rest[1:]); err != nil {
			return "", false, err
		}
		return c.target, hasData, nil

	case muxStatusKeep:
		// A destination is only repeated when it changes.
		if len(rest) >= 1 && rest[0] == muxNetworkUDP {
			if target, _, err = readAddrPort(rest[1:]); err != nil {
				return "", false, err
			}
			return target, hasData, nil
		}
		return c.target, hasData, nil

	case muxStatusEnd:
		return "", false, io.EOF

	case muxStatusKeepAlive:
		return "", hasData, nil

	default:
		return "", false, ErrBadMuxFrame
	}
}

func (c *xudpConn) readData(b []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
		return 0, err
	}

	size := int(binary.BigEndian.Uint16(hdr[:]))
	if size <= len(b) {
		return io.ReadFull(c.Conn, b[:size])
	}

	n, err := io.ReadFull(c.Conn, b)
	if err != nil {
		return n, err
	}
	_, err = io.CopyN(io.Discard, c.Conn, int64(size-len(b)))
	return n, err
}

func (c *xudpConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if len(b) > maxPacketSize {
		return 0, ErrBadPacket
	}

	c.mu.Lock()
	if requested, ok := c.requested[addr.String()]; ok {
		addr = requested
	}
	sessionID := c.sessionID
	c.mu.Unlock()

	host, port, err := splitAddr(addr)
	if err != nil {
		return 0, err
	}

	buf := bufpool.Get(maxMuxMetaSize + len(b) + 4)
	defer bufpool.Put(buf)

	frame := buf[:0]
	frame = append(frame, 0, 0) // metadata length, filled in below
	frame = append(frame, sessionID[0], sessionID[1], muxStatusKeep, muxOptionData, muxNetworkUDP)
	frame, err = appendAddrPort(frame, host, port)
	if err != nil {
		return 0, err
	}
	binary.BigEndian.PutUint16(frame[:2], uint16(len(frame)-2))

	frame = binary.BigEndian.AppendUint16(frame, uint16(len(b)))
	frame = append(frame, b...)

	if _, err := c.Conn.Write(frame); err != nil {
		return 0, err
	}
	return len(b), nil
}

// muxAddr is a destination as the client wrote it, which may be a name.
type muxAddr struct {
	addr string
}

func (a *muxAddr) Network() string { return "udp" }
func (a *muxAddr) String() string  { return a.addr }

// readAddrPort reads the port first address form that VLESS and mux share.
func readAddrPort(b []byte) (addr string, n int, err error) {
	if len(b) < 4 {
		return "", 0, ErrBadAddr
	}

	port := binary.BigEndian.Uint16(b[:2])
	var host string

	switch b[2] {
	case AddrIPv4:
		if len(b) < 7 {
			return "", 0, ErrBadAddr
		}
		host, n = net.IP(b[3:7]).String(), 7
	case AddrIPv6:
		if len(b) < 19 {
			return "", 0, ErrBadAddr
		}
		host, n = net.IP(b[3:19]).String(), 19
	case AddrDomain:
		size := int(b[3])
		if size == 0 || len(b) < 4+size {
			return "", 0, ErrBadAddr
		}
		host, n = string(b[4:4+size]), 4+size
	default:
		return "", 0, ErrBadAddr
	}

	return net.JoinHostPort(host, strconv.Itoa(int(port))), n, nil
}

func appendAddrPort(b []byte, host string, port int) ([]byte, error) {
	b = binary.BigEndian.AppendUint16(b, uint16(port))

	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return append(append(b, AddrIPv4), ip4...), nil
		}
		return append(append(b, AddrIPv6), ip.To16()...), nil
	}

	if len(host) == 0 || len(host) > 255 {
		return nil, ErrBadAddr
	}
	b = append(b, AddrDomain, byte(len(host)))
	return append(b, host...), nil
}

func splitAddr(addr net.Addr) (host string, port int, err error) {
	if a, ok := addr.(*net.UDPAddr); ok {
		return a.IP.String(), a.Port, nil
	}

	h, p, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", 0, err
	}
	port, err = strconv.Atoi(p)
	return h, port, err
}
