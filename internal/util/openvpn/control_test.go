package openvpn

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// scriptedPIO is a PacketIO that replays a fixed list of packets, then EOF.
type scriptedPIO struct {
	mu   sync.Mutex
	pkts [][]byte
}

func (p *scriptedPIO) ReadPacket(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pkts) == 0 {
		return nil, io.EOF
	}
	pkt := p.pkts[0]
	p.pkts = p.pkts[1:]
	return pkt, nil
}

func (p *scriptedPIO) WritePacket(context.Context, []byte) error { return nil }
func (p *scriptedPIO) Close() error                              { return nil }
func (p *scriptedPIO) LocalAddr() net.Addr                       { return dummyAddr{} }
func (p *scriptedPIO) RemoteAddr() net.Addr                      { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "test" }
func (dummyAddr) String() string  { return "test" }

// TestControlChannelReordersAndDedups feeds control messages out of order
// with a duplicate and verifies Read delivers them strictly in order,
// exactly once each.
func TestControlChannelReordersAndDedups(t *testing.T) {
	key := make([]byte, 256)
	for i := range key {
		key[i] = byte(i * 7)
	}
	chanCrypt, err := NewTLSCrypt(key, false)
	if err != nil {
		t.Fatal(err)
	}
	peerCrypt, err := NewTLSCrypt(key, true) // the remote end's codec
	if err != nil {
		t.Fatal(err)
	}
	peerSID, _ := NewSessionID()
	localSID, _ := NewSessionID()

	mk := func(msgID uint32) []byte {
		p := ControlPacket{
			Opcode:       PControlV1,
			LocalSession: peerSID,
			MessageID:    msgID,
			Payload:      []byte{byte(msgID)},
		}
		wire, err := p.Encode(peerCrypt, msgID+1, 0)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}

	// Arrival order: 0, 2, 1, 1 (dup), 3.
	pio := &scriptedPIO{pkts: [][]byte{mk(0), mk(2), mk(1), mk(1), mk(3)}}
	mux := NewPacketMux(pio)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go mux.Run(ctx)

	cc := NewControlChannel(mux, chanCrypt, localSID)
	for want := uint32(0); want < 4; want++ {
		pkt, err := cc.Read(ctx)
		if err != nil {
			t.Fatalf("Read #%d: %v", want, err)
		}
		if pkt.MessageID != want {
			t.Fatalf("out-of-order delivery: got message %d, want %d", pkt.MessageID, want)
		}
		if len(pkt.Payload) != 1 || pkt.Payload[0] != byte(want) {
			t.Fatalf("payload mismatch for message %d: %x", want, pkt.Payload)
		}
	}
}
