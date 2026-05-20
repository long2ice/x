package openvpn

import (
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const (
	udpReadBuffer    = 2048
	udpPeerInboxSize = 128
)

// udpPeerConn is the per-remote-address net.Conn the listener feeds to
// the OpenVPN protocol driver. Inbound datagrams arrive through inbox
// (filled by the listener demux); outbound writes go straight to the
// shared UDP socket addressed to this peer.
type udpPeerConn struct {
	pc    net.PacketConn
	addr  net.Addr
	inbox chan []byte

	deadlineMu sync.Mutex
	deadline   time.Time

	activityMu sync.Mutex
	lastActive time.Time

	closeOnce sync.Once
	closed    chan struct{}
	onClose   func()
}

func newUDPPeerConn(pc net.PacketConn, addr net.Addr, onClose func()) *udpPeerConn {
	return &udpPeerConn{
		pc:         pc,
		addr:       addr,
		inbox:      make(chan []byte, udpPeerInboxSize),
		closed:     make(chan struct{}),
		onClose:    onClose,
		lastActive: time.Now(),
	}
}

func (p *udpPeerConn) deliver(pkt []byte) {
	p.activityMu.Lock()
	p.lastActive = time.Now()
	p.activityMu.Unlock()
	select {
	case p.inbox <- pkt:
	case <-p.closed:
	default:
		// inbox full; drop. The reliability layer retransmits control
		// packets, and data-channel loss is tolerated.
	}
}

func (p *udpPeerConn) lastActiveAt() time.Time {
	p.activityMu.Lock()
	defer p.activityMu.Unlock()
	return p.lastActive
}

func (p *udpPeerConn) Read(b []byte) (int, error) {
	p.deadlineMu.Lock()
	dl := p.deadline
	p.deadlineMu.Unlock()

	var deadlineCh <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		deadlineCh = timer.C
	}
	select {
	case pkt, ok := <-p.inbox:
		if !ok {
			return 0, io.EOF
		}
		return copy(b, pkt), nil
	case <-deadlineCh:
		return 0, os.ErrDeadlineExceeded
	case <-p.closed:
		return 0, io.EOF
	}
}

func (p *udpPeerConn) Write(b []byte) (int, error) {
	return p.pc.WriteTo(b, p.addr)
}

func (p *udpPeerConn) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		if p.onClose != nil {
			p.onClose()
		}
	})
	return nil
}

func (p *udpPeerConn) LocalAddr() net.Addr  { return p.pc.LocalAddr() }
func (p *udpPeerConn) RemoteAddr() net.Addr { return p.addr }

func (p *udpPeerConn) SetDeadline(t time.Time) error {
	p.deadlineMu.Lock()
	p.deadline = t
	p.deadlineMu.Unlock()
	return nil
}
func (p *udpPeerConn) SetReadDeadline(t time.Time) error  { return p.SetDeadline(t) }
func (p *udpPeerConn) SetWriteDeadline(t time.Time) error { return nil }
