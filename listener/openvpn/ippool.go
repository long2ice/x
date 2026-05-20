package openvpn

import (
	"errors"
	"net/netip"
	"sync"
)

var errPoolExhausted = errors.New("openvpn listener: address pool exhausted")

// ipPool hands out client tunnel addresses and peer ids. The first host
// in the subnet is reserved as the server gateway.
type ipPool struct {
	mu      sync.Mutex
	subnet  netip.Prefix
	gateway netip.Addr
	inUse   map[netip.Addr]struct{}
	peerSeq uint32
}

func newIPPool(subnet netip.Prefix) *ipPool {
	gw := subnet.Addr().Next() // .1
	return &ipPool{
		subnet:  subnet,
		gateway: gw,
		inUse:   map[netip.Addr]struct{}{gw: {}},
	}
}

// allocate returns a free address and a fresh peer id.
func (p *ipPool) allocate() (netip.Addr, uint32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for addr := p.gateway.Next(); p.subnet.Contains(addr); addr = addr.Next() {
		if _, used := p.inUse[addr]; used {
			continue
		}
		// Skip the broadcast-like all-ones host of the subnet.
		if !addr.Next().IsValid() || !p.subnet.Contains(addr.Next()) {
			break
		}
		p.inUse[addr] = struct{}{}
		p.peerSeq++
		return addr, p.peerSeq, nil
	}
	return netip.Addr{}, 0, errPoolExhausted
}

func (p *ipPool) release(addr netip.Addr) {
	p.mu.Lock()
	delete(p.inUse, addr)
	p.mu.Unlock()
}

// netmask returns the subnet mask as a netip.Addr (e.g. 255.255.255.0).
func (p *ipPool) netmask() netip.Addr {
	bits := p.subnet.Bits()
	var m [4]byte
	for i := 0; i < 4; i++ {
		b := bits - i*8
		switch {
		case b >= 8:
			m[i] = 0xff
		case b > 0:
			m[i] = byte(0xff << (8 - b))
		}
	}
	return netip.AddrFrom4(m)
}
