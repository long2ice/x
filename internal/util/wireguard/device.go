package wireguard

import (
	"io"
	"os"
	"sync"

	wgconn "golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/tun"
)

// Device is an in-process tun.Device used by wireguard-go.
//
// Direction:
//   - Inbound  (peer -> us): wireguard-go decrypts a packet and calls Write;
//     the packet is queued on inbound and surfaced through ReadPacket so the
//     downstream consumer (e.g. tungo) can feed it into its TCP/IP stack.
//   - Outbound (us -> peer): the consumer emits an IP packet via WritePacket;
//     it is queued on outbound and consumed by Read so wireguard-go can encrypt
//     and send it to the peer.
type Device struct {
	name      string
	mtu       int
	events    chan tun.Event
	inbound   chan []byte
	outbound  chan []byte
	closeOnce sync.Once
	closed    chan struct{}
}

func NewDevice(name string, mtu, queueLen int) *Device {
	d := &Device{
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
func (d *Device) File() *os.File { return nil }

// Read is called by wireguard-go to obtain plaintext IP packets that need to
// be encrypted and sent to peers. It blocks until at least one packet is
// available, then opportunistically drains more packets from the queue up to
// len(bufs) so wireguard-go can batch-encrypt them in parallel.
func (d *Device) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if len(bufs) == 0 {
		return 0, nil
	}

	select {
	case pkt, ok := <-d.outbound:
		if !ok {
			return 0, os.ErrClosed
		}
		sizes[0] = copy(bufs[0][offset:], pkt)
	case <-d.closed:
		return 0, os.ErrClosed
	}

	n := 1
	for n < len(bufs) {
		select {
		case pkt, ok := <-d.outbound:
			if !ok {
				return n, nil
			}
			sizes[n] = copy(bufs[n][offset:], pkt)
			n++
		default:
			return n, nil
		}
	}
	return n, nil
}

// Write is called by wireguard-go after decrypting packets received from peers.
// Each packet is copied onto the inbound queue for the consumer.
func (d *Device) Write(bufs [][]byte, offset int) (int, error) {
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

func (d *Device) MTU() (int, error)        { return d.mtu, nil }
func (d *Device) Name() (string, error)    { return d.name, nil }
func (d *Device) Events() <-chan tun.Event { return d.events }

// BatchSize tells wireguard-go how many packets it may stage per Read/Write
// call. Matching IdealBatchSize lets the encryption pipeline parallelise and
// is the single biggest knob for tun-side throughput.
func (d *Device) BatchSize() int { return wgconn.IdealBatchSize }

func (d *Device) Close() error {
	d.closeOnce.Do(func() {
		close(d.closed)
		close(d.events)
	})
	return nil
}

// ReadPacket blocks until a decrypted IP packet is available or the device is
// closed. Returns io.EOF on close so the consumer treats it as a graceful
// shutdown rather than an I/O error.
func (d *Device) ReadPacket(p []byte) (int, error) {
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

// WritePacket queues an IP packet for wireguard-go to encrypt and send.
func (d *Device) WritePacket(p []byte) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case d.outbound <- cp:
		return len(p), nil
	case <-d.closed:
		return 0, io.ErrClosedPipe
	}
}
