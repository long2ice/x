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
	udpPeerInboxSize = 64
)

// udpPeerConn is the per-remote-addr net.Conn the listener feeds to the
// shared OpenVPN tunnel driver. Inbound datagrams arrive through inbox
// (filled by the listener's demux goroutine). Outbound writes go
// directly to the shared UDP socket addressed to the peer.
//
// SetDeadline is sampled at the start of each Read; deadlines set while
// a Read is already blocked do not take effect until the next Read.
type udpPeerConn struct {
	pc    net.PacketConn
	addr  net.Addr
	inbox chan []byte

	deadlineMu sync.Mutex
	deadline   time.Time

	activityMu sync.Mutex
	lastActive time.Time

	doneMu        sync.Mutex
	handshakeDone bool

	closeOnce sync.Once
	closed    chan struct{}

	onClose func()
}

// markHandshakeDone signals that this peer has finished its OpenVPN-shape
// handshake (including the post-handshake decoy). Until then, the idle
// reaper skips it: the post-handshake sleep produces no inbound packets
// for up to ~1.4s, which a short idle window would otherwise mistake for
// an abandoned peer.
func (p *udpPeerConn) markHandshakeDone() {
	p.doneMu.Lock()
	p.handshakeDone = true
	p.doneMu.Unlock()
	p.activityMu.Lock()
	p.lastActive = time.Now()
	p.activityMu.Unlock()
}

func (p *udpPeerConn) isHandshakeDone() bool {
	p.doneMu.Lock()
	defer p.doneMu.Unlock()
	return p.handshakeDone
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
		// inbox full; drop. The reliability layer will retransmit if it
		// was a control packet, and data-channel loss is tolerated.
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
		n := copy(b, pkt)
		return n, nil
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
