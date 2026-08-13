package wireguard

import (
	"net"
	"net/netip"
	"sync"

	wgconn "golang.zx2c4.com/wireguard/conn"
)

// DialBind is a wgconn.Bind whose UDP socket is created by the caller's
// dial function instead of wgconn's default bind. This lets the encrypted
// WireGuard transport inherit the node's socket options (interface binding,
// SO_MARK, netns) the same way every other dialer does via the chain's
// NetDialer. Without them, rule-based routing on the host can capture the
// tunnel's own UDP packets and feed them back into the tunnel.
type DialBind struct {
	dial func() (*net.UDPConn, error)
	mu   sync.RWMutex
	conn *net.UDPConn
}

func NewDialBind(dial func() (*net.UDPConn, error)) *DialBind {
	return &DialBind{dial: dial}
}

// Open ignores the requested port: the dialer owns socket creation, and the
// device side (a client) never requests a fixed listen port.
func (b *DialBind) Open(_ uint16) ([]wgconn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return nil, 0, wgconn.ErrBindAlreadyOpen
	}
	conn, err := b.dial()
	if err != nil {
		return nil, 0, err
	}
	b.conn = conn
	var port uint16
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		port = uint16(ua.Port)
	}
	recv := func(packets [][]byte, sizes []int, eps []wgconn.Endpoint) (int, error) {
		n, addr, err := conn.ReadFromUDPAddrPort(packets[0])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		eps[0] = &wgconn.StdNetEndpoint{AddrPort: unmap(addr)}
		return 1, nil
	}
	return []wgconn.ReceiveFunc{recv}, port, nil
}

func (b *DialBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return nil
	}
	err := b.conn.Close()
	b.conn = nil
	return err
}

// SetMark is a no-op: the mark, if any, is applied by the dial function when
// the socket is created.
func (b *DialBind) SetMark(uint32) error { return nil }

func (b *DialBind) Send(bufs [][]byte, ep wgconn.Endpoint) error {
	se, ok := ep.(*wgconn.StdNetEndpoint)
	if !ok {
		return wgconn.ErrWrongEndpointType
	}
	b.mu.RLock()
	conn := b.conn
	b.mu.RUnlock()
	if conn == nil {
		return net.ErrClosed
	}
	for _, buf := range bufs {
		if _, err := conn.WriteToUDPAddrPort(buf, se.AddrPort); err != nil {
			return err
		}
	}
	return nil
}

func (b *DialBind) ParseEndpoint(s string) (wgconn.Endpoint, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return &wgconn.StdNetEndpoint{AddrPort: unmap(ap)}, nil
	}
	// Not a numeric address; resolve the hostname.
	ua, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		return nil, err
	}
	return &wgconn.StdNetEndpoint{AddrPort: unmap(ua.AddrPort())}, nil
}

func (b *DialBind) BatchSize() int { return 1 }

// unmap normalizes IPv4-mapped IPv6 addresses so endpoint comparisons inside
// wireguard-go (roaming detection) don't see ::ffff:a.b.c.d and a.b.c.d as
// different peers.
func unmap(ap netip.AddrPort) netip.AddrPort {
	if ap.Addr().Is4In6() {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return ap
}
