// Package vless implements the VLESS protocol used by Xray/V2Ray.
//
// The request sent by the client is:
//
//	+---------+----------+--------------+---------+---------+------+--------+
//	| version | uuid(16) | addons len:1 | addons  | command | port | addr   |
//	+---------+----------+--------------+---------+---------+------+--------+
//
// port and addr are omitted for the mux command. The server replies with a
// two byte header (version, addons length) prepended to the first response.
package vless

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
)

const (
	// Version is the only VLESS version in use.
	Version byte = 0

	CmdTCP byte = 1
	CmdUDP byte = 2
	CmdMux byte = 3

	AddrIPv4   byte = 1
	AddrDomain byte = 2
	AddrIPv6   byte = 3

	// FlowVision is the XTLS Vision flow, not supported by the server yet.
	FlowVision = "xtls-rprx-vision"
)

var (
	ErrBadVersion = errors.New("vless: unsupported protocol version")
	ErrBadAddr    = errors.New("vless: unsupported address type")
	ErrBadAddons  = errors.New("vless: malformed addons")
	ErrBadPacket  = errors.New("vless: packet too large")
)

// UUID is the 16 bytes user identity.
type UUID [16]byte

func (u UUID) String() string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

// ParseUUID parses the canonical UUID representation. Following Xray, a short
// string that is not a UUID is mapped to one by hashing it, so that arbitrary
// passwords can be used as user IDs.
func ParseUUID(s string) (uuid UUID, err error) {
	if len(s) < 32 {
		if len(s) == 0 || len(s) > 30 {
			return uuid, fmt.Errorf("vless: invalid uuid %q", s)
		}
		// UUID v5 with the nil namespace, as Xray does for non-UUID ids.
		h := sha1.New()
		h.Write(uuid[:])
		h.Write([]byte(s))
		sum := h.Sum(nil)
		sum[6] = (sum[6] & 0x0f) | (5 << 4)
		sum[8] = (sum[8] & 0x3f) | 0x80
		copy(uuid[:], sum)
		return uuid, nil
	}

	b := make([]byte, 0, 16)
	for _, c := range []byte(s) {
		if c == '-' {
			continue
		}
		b = append(b, c)
	}
	if len(b) != 32 {
		return uuid, fmt.Errorf("vless: invalid uuid %q", s)
	}
	if _, err := hex.Decode(uuid[:], b); err != nil {
		return uuid, fmt.Errorf("vless: invalid uuid %q: %w", s, err)
	}
	return uuid, nil
}

// Request is the VLESS request header sent by the client.
type Request struct {
	Version byte
	ID      UUID
	Flow    string
	Command byte
	Host    string
	Port    int
}

// Addr returns the target address of the request.
func (r *Request) Addr() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}

// ReadRequest reads a VLESS request header from r.
func ReadRequest(r io.Reader) (*Request, error) {
	var b [256]byte

	if _, err := io.ReadFull(r, b[:18]); err != nil {
		return nil, err
	}

	req := &Request{Version: b[0]}
	if req.Version != Version {
		return nil, ErrBadVersion
	}
	copy(req.ID[:], b[1:17])

	if n := int(b[17]); n > 0 {
		if _, err := io.ReadFull(r, b[:n]); err != nil {
			return nil, err
		}
		flow, err := parseAddons(b[:n])
		if err != nil {
			return nil, err
		}
		req.Flow = flow
	}

	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return nil, err
	}
	req.Command = b[0]
	if req.Command == CmdMux {
		// A mux request carries no address, it always goes to the same one.
		req.Host, req.Port = MuxHost, MuxPort
		return req, nil
	}

	if _, err := io.ReadFull(r, b[:3]); err != nil {
		return nil, err
	}
	req.Port = int(binary.BigEndian.Uint16(b[:2]))

	switch b[2] {
	case AddrIPv4:
		if _, err := io.ReadFull(r, b[:4]); err != nil {
			return nil, err
		}
		req.Host = net.IP(b[:4]).String()
	case AddrIPv6:
		if _, err := io.ReadFull(r, b[:16]); err != nil {
			return nil, err
		}
		req.Host = net.IP(b[:16]).String()
	case AddrDomain:
		if _, err := io.ReadFull(r, b[:1]); err != nil {
			return nil, err
		}
		n := int(b[0])
		if n == 0 {
			return nil, ErrBadAddr
		}
		if _, err := io.ReadFull(r, b[:n]); err != nil {
			return nil, err
		}
		req.Host = string(b[:n])
	default:
		return nil, ErrBadAddr
	}

	return req, nil
}

// WriteRequest writes a VLESS request header to w. It is only used by tests
// and by clients.
func WriteRequest(w io.Writer, req *Request) error {
	b := make([]byte, 0, 64)
	b = append(b, req.Version)
	b = append(b, req.ID[:]...)
	if req.Flow == "" {
		b = append(b, 0)
	} else {
		addons := make([]byte, 0, len(req.Flow)+2)
		addons = append(addons, 0x0a, byte(len(req.Flow)))
		addons = append(addons, req.Flow...)
		b = append(b, byte(len(addons)))
		b = append(b, addons...)
	}
	b = append(b, req.Command)

	if req.Command != CmdMux {
		b = binary.BigEndian.AppendUint16(b, uint16(req.Port))
		if ip := net.ParseIP(req.Host); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				b = append(b, AddrIPv4)
				b = append(b, ip4...)
			} else {
				b = append(b, AddrIPv6)
				b = append(b, ip.To16()...)
			}
		} else {
			if len(req.Host) == 0 || len(req.Host) > 255 {
				return ErrBadAddr
			}
			b = append(b, AddrDomain, byte(len(req.Host)))
			b = append(b, req.Host...)
		}
	}

	_, err := w.Write(b)
	return err
}

// parseAddons extracts the flow from the protobuf encoded addons.
// Only the flow (field 1) is of interest, other fields are skipped.
func parseAddons(b []byte) (flow string, err error) {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return "", ErrBadAddons
		}
		b = b[n:]

		switch field, wire := tag>>3, tag&7; wire {
		case 0: // varint
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return "", ErrBadAddons
			}
			b = b[n:]
		case 1: // 64-bit
			if len(b) < 8 {
				return "", ErrBadAddons
			}
			b = b[8:]
		case 2: // length delimited
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return "", ErrBadAddons
			}
			b = b[n:]
			if field == 1 {
				flow = string(b[:l])
			}
			b = b[l:]
		case 5: // 32-bit
			if len(b) < 4 {
				return "", ErrBadAddons
			}
			b = b[4:]
		default:
			return "", ErrBadAddons
		}
	}
	return flow, nil
}

type serverConn struct {
	net.Conn
	replied atomic.Bool
}

// ServerConn wraps c so that the VLESS response header is prepended to the
// first write, the same way Xray does it. Nothing is sent if the server never
// writes anything.
func ServerConn(c net.Conn) net.Conn {
	return &serverConn{Conn: c}
}

func (c *serverConn) Write(b []byte) (n int, err error) {
	if !c.replied.CompareAndSwap(false, true) {
		return c.Conn.Write(b)
	}

	buf := make([]byte, 0, len(b)+2)
	buf = append(buf, Version, 0)
	buf = append(buf, b...)

	n, err = c.Conn.Write(buf)
	if n -= 2; n < 0 {
		n = 0
	}
	return
}

// ReadResponse reads the VLESS response header. It is only used by tests and
// by clients.
func ReadResponse(r io.Reader) error {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	if b[0] != Version {
		return ErrBadVersion
	}
	if n := int(b[1]); n > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
			return err
		}
	}
	return nil
}
