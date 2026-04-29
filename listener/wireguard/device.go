package wireguard

import (
	"io"
	"os"
	"sync"

	"golang.zx2c4.com/wireguard/tun"
)

// wgDevice is an in-process tun.Device used by wireguard-go.
//
// Direction:
//   - Inbound  (peer -> gost): wireguard-go decrypts a packet and calls Write.
//     The packet is queued on inbound and surfaced through (*conn).Read so the
//     downstream handler (e.g. tungo) can feed it into its TCP/IP stack.
//   - Outbound (gost -> peer): the handler emits an IP packet via (*conn).Write,
//     it is queued on outbound and consumed by Read so wireguard-go can encrypt
//     and send it to the peer.
type wgDevice struct {
	name      string
	mtu       int
	events    chan tun.Event
	inbound   chan []byte
	outbound  chan []byte
	closeOnce sync.Once
	closed    chan struct{}
}

func newWGDevice(name string, mtu, queueLen int) *wgDevice {
	d := &wgDevice{
		name:     name,
		mtu:      mtu,
		events:   make(chan tun.Event, 4),
		inbound:  make(chan []byte, queueLen),
		outbound: make(chan []byte, queueLen),
		closed:   make(chan struct{}),
	}
	d.events <- tun.EventUp
	return d
}

// File implements tun.Device. There is no underlying fd.
func (d *wgDevice) File() *os.File { return nil }

// Read is called by wireguard-go to obtain plaintext IP packets that need to
// be encrypted and sent to peers. Each call returns at most one packet.
func (d *wgDevice) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case pkt, ok := <-d.outbound:
		if !ok {
			return 0, os.ErrClosed
		}
		n := copy(bufs[0][offset:], pkt)
		sizes[0] = n
		return 1, nil
	case <-d.closed:
		return 0, os.ErrClosed
	}
}

// Write is called by wireguard-go after decrypting packets received from peers.
// Each packet is copied onto the inbound queue for the handler to consume.
func (d *wgDevice) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		pkt := b[offset:]
		if len(pkt) == 0 {
			continue
		}
		cp := make([]byte, len(pkt))
		copy(cp, pkt)
		select {
		case d.inbound <- cp:
		case <-d.closed:
			return 0, os.ErrClosed
		}
	}
	return len(bufs), nil
}

func (d *wgDevice) MTU() (int, error)            { return d.mtu, nil }
func (d *wgDevice) Name() (string, error)        { return d.name, nil }
func (d *wgDevice) Events() <-chan tun.Event     { return d.events }
func (d *wgDevice) BatchSize() int               { return 1 }

func (d *wgDevice) Close() error {
	d.closeOnce.Do(func() {
		close(d.closed)
		close(d.events)
	})
	return nil
}

// readPacket blocks until a decrypted IP packet is available or the device is
// closed. Returns the number of bytes copied into p. Returns io.EOF on close
// so the downstream handler treats it as a graceful shutdown rather than an
// I/O error.
func (d *wgDevice) readPacket(p []byte) (int, error) {
	select {
	case pkt, ok := <-d.inbound:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, pkt), nil
	case <-d.closed:
		return 0, io.EOF
	}
}

// writePacket queues an IP packet for wireguard-go to encrypt and send.
func (d *wgDevice) writePacket(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case d.outbound <- cp:
		return len(p), nil
	case <-d.closed:
		return 0, io.ErrClosedPipe
	}
}
