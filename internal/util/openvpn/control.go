package openvpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// PacketIO is the datagram abstraction the protocol runs on: one
// Read/Write moves one OpenVPN packet, regardless of the UDP/TCP
// transport underneath.
type PacketIO interface {
	ReadPacket(ctx context.Context) ([]byte, error)
	WritePacket(ctx context.Context, packet []byte) error
	Close() error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// --- transport adapters ---------------------------------------------------

type datagramPacketIO struct{ conn net.Conn }

// NewDatagramPacketIO adapts a connected UDP socket: one datagram == one
// OpenVPN packet.
func NewDatagramPacketIO(conn net.Conn) PacketIO { return &datagramPacketIO{conn: conn} }

func (d *datagramPacketIO) ReadPacket(ctx context.Context) ([]byte, error) {
	type result struct {
		pkt []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 64*1024)
		n, err := d.conn.Read(buf)
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{pkt: cloneBytes(buf[:n])}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.pkt, r.err
	}
}

func (d *datagramPacketIO) WritePacket(ctx context.Context, packet []byte) error {
	_, err := d.conn.Write(packet)
	return err
}

func (d *datagramPacketIO) Close() error         { return d.conn.Close() }
func (d *datagramPacketIO) LocalAddr() net.Addr  { return d.conn.LocalAddr() }
func (d *datagramPacketIO) RemoteAddr() net.Addr { return d.conn.RemoteAddr() }

type streamPacketIO struct {
	conn net.Conn
	mu   sync.Mutex
}

// NewStreamPacketIO adapts a TCP stream: each packet is framed with a
// 2-byte big-endian length prefix.
func NewStreamPacketIO(conn net.Conn) PacketIO { return &streamPacketIO{conn: conn} }

func (s *streamPacketIO) ReadPacket(ctx context.Context) ([]byte, error) {
	type result struct {
		pkt []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var lb [2]byte
		if _, err := io.ReadFull(s.conn, lb[:]); err != nil {
			ch <- result{err: err}
			return
		}
		size := int(lb[0])<<8 | int(lb[1])
		if size == 0 {
			ch <- result{err: errors.New("openvpn: empty tcp frame")}
			return
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(s.conn, buf); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{pkt: buf}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.pkt, r.err
	}
}

func (s *streamPacketIO) WritePacket(ctx context.Context, packet []byte) error {
	if len(packet) > 0xffff {
		return fmt.Errorf("openvpn: tcp frame too large: %d", len(packet))
	}
	frame := make([]byte, 2+len(packet))
	frame[0] = byte(len(packet) >> 8)
	frame[1] = byte(len(packet))
	copy(frame[2:], packet)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Write(frame)
	return err
}

func (s *streamPacketIO) Close() error         { return s.conn.Close() }
func (s *streamPacketIO) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *streamPacketIO) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// --- packet mux -----------------------------------------------------------

// PacketMux runs a single read loop over a PacketIO and demultiplexes
// packets into a control queue and a data queue by opcode.
type PacketMux struct {
	io      PacketIO
	control chan []byte
	data    chan []byte
	done    chan struct{}
	once    sync.Once
}

func NewPacketMux(io PacketIO) *PacketMux {
	return &PacketMux{
		io:      io,
		control: make(chan []byte, 64),
		data:    make(chan []byte, 512),
		done:    make(chan struct{}),
	}
}

// Run is the read loop; it exits when ctx is cancelled or the transport
// fails. Start it in its own goroutine.
func (m *PacketMux) Run(ctx context.Context) {
	defer m.Close()
	for ctx.Err() == nil {
		pkt, err := m.io.ReadPacket(ctx)
		if err != nil {
			return
		}
		if len(pkt) == 0 {
			continue
		}
		opcode, _ := parseOpcodeKeyID(pkt[0])
		ch := m.data
		if opcode.IsControl() {
			ch = m.control
		}
		select {
		case ch <- pkt:
		case <-ctx.Done():
			return
		case <-m.done:
			return
		}
	}
}

func (m *PacketMux) ReadPacket(ctx context.Context) ([]byte, error) {
	// Drain any already-buffered packet before reporting a closed mux,
	// so a transport that EOFs after delivering does not lose them.
	select {
	case pkt := <-m.control:
		return pkt, nil
	default:
	}
	select {
	case pkt := <-m.control:
		return pkt, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *PacketMux) ReadDataPacket(ctx context.Context) ([]byte, error) {
	select {
	case pkt := <-m.data:
		return pkt, nil
	default:
	}
	select {
	case pkt := <-m.data:
		return pkt, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *PacketMux) WritePacket(ctx context.Context, packet []byte) error {
	return m.io.WritePacket(ctx, packet)
}

func (m *PacketMux) Close() error {
	m.once.Do(func() {
		close(m.done)
		_ = m.io.Close()
	})
	return nil
}

func (m *PacketMux) LocalAddr() net.Addr  { return m.io.LocalAddr() }
func (m *PacketMux) RemoteAddr() net.Addr { return m.io.RemoteAddr() }

// --- reliable control channel --------------------------------------------

// ControlChannel layers OpenVPN's reliability (message ids, ACKs,
// retransmission) over a PacketMux. It is not a net.Conn; ControlConn
// adapts it into one for the TLS stack.
type ControlChannel struct {
	io    *PacketMux
	crypt *TLSCrypt
	clock func() time.Time

	keyID  uint8
	local  SessionID
	mu     sync.Mutex
	remote SessionID

	sendPacketID uint32
	sendMessage  uint32
	ackPending   []uint32
	unacked      map[uint32]*ControlPacket

	// Receive-side ordering: control messages must reach the TLS stack
	// strictly in order. recvNext is the next message id to deliver;
	// out-of-order arrivals wait in recvBuf; recvReady holds decoded
	// in-order packets not yet returned by Read.
	recvNext  uint32
	recvBuf   map[uint32]*ControlPacket
	recvReady []*ControlPacket
}

const recvBufferMax = 64 // cap out-of-order control buffering

func NewControlChannel(mux *PacketMux, crypt *TLSCrypt, local SessionID) *ControlChannel {
	return &ControlChannel{
		io:      mux,
		crypt:   crypt,
		clock:   time.Now,
		local:   local,
		unacked: make(map[uint32]*ControlPacket),
		recvBuf: make(map[uint32]*ControlPacket),
	}
}

func (c *ControlChannel) LocalSessionID() SessionID { return c.local }

func (c *ControlChannel) RemoteSessionID() SessionID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remote
}

func (c *ControlChannel) SetRemoteSessionID(id SessionID) {
	c.mu.Lock()
	c.remote = id
	c.mu.Unlock()
}

// Send reliably transmits one control message and returns its id.
func (c *ControlChannel) Send(ctx context.Context, opcode Opcode, payload []byte) (uint32, error) {
	if !opcode.HasMessageID() {
		return 0, fmt.Errorf("openvpn: %s cannot carry a reliable message", opcode)
	}
	c.mu.Lock()
	messageID := c.sendMessage
	c.sendMessage++
	pkt := &ControlPacket{
		Opcode:           opcode,
		KeyID:            c.keyID,
		LocalSession:     c.local,
		AckIDs:           append([]uint32(nil), c.ackPending...),
		AckRemoteSession: c.remote,
		MessageID:        messageID,
		Payload:          cloneBytes(payload),
	}
	c.ackPending = nil
	c.unacked[messageID] = pkt
	c.mu.Unlock()
	if err := c.write(ctx, pkt); err != nil {
		return 0, err
	}
	return messageID, nil
}

// SendAck flushes any pending ACKs as a standalone P_ACK_V1.
func (c *ControlChannel) SendAck(ctx context.Context) error {
	c.mu.Lock()
	if len(c.ackPending) == 0 {
		c.mu.Unlock()
		return nil
	}
	pkt := &ControlPacket{
		Opcode:           PAckV1,
		KeyID:            c.keyID,
		LocalSession:     c.local,
		AckIDs:           append([]uint32(nil), c.ackPending...),
		AckRemoteSession: c.remote,
	}
	c.ackPending = nil
	c.mu.Unlock()
	return c.write(ctx, pkt)
}

// Retransmit re-sends every still-unacked control packet. Drive it on a
// timer while a handshake is in flight to survive UDP loss.
func (c *ControlChannel) Retransmit(ctx context.Context) error {
	c.mu.Lock()
	pkts := make([]*ControlPacket, 0, len(c.unacked))
	for _, p := range c.unacked {
		cp := *p
		cp.AckIDs = append([]uint32(nil), c.ackPending...)
		cp.AckRemoteSession = c.remote
		pkts = append(pkts, &cp)
	}
	c.ackPending = nil
	c.mu.Unlock()
	for _, p := range pkts {
		if err := c.write(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// Read returns the next in-order control packet. P_ACK_V1 packets are
// consumed internally; duplicate or stale messages are re-ACKed but not
// re-delivered; out-of-order messages are buffered until their turn.
func (c *ControlChannel) Read(ctx context.Context) (*ControlPacket, error) {
	for {
		c.mu.Lock()
		if len(c.recvReady) > 0 {
			pkt := c.recvReady[0]
			c.recvReady = c.recvReady[1:]
			c.mu.Unlock()
			return pkt, nil
		}
		c.mu.Unlock()

		raw, err := c.io.ReadPacket(ctx)
		if err != nil {
			return nil, err
		}
		pkt, err := DecodeControlPacket(c.crypt, raw)
		if err != nil {
			continue // unauthenticated / malformed: drop
		}
		if pkt.KeyID != c.keyID {
			continue // belongs to a different key_id (e.g. another renegotiation)
		}

		c.mu.Lock()
		if c.remote.IsZero() && pkt.LocalSession != c.local {
			c.remote = pkt.LocalSession
		}
		for _, ackID := range pkt.AckIDs {
			delete(c.unacked, ackID)
		}
		if pkt.Opcode == PAckV1 {
			c.mu.Unlock()
			continue
		}
		// Always (re-)ACK a received message; a prior ACK may be lost.
		c.ackPending = appendUint32Set(c.ackPending, pkt.MessageID)

		switch {
		case pkt.MessageID < c.recvNext:
			// duplicate / already delivered
		case pkt.MessageID > c.recvNext:
			if _, dup := c.recvBuf[pkt.MessageID]; !dup && len(c.recvBuf) < recvBufferMax {
				c.recvBuf[pkt.MessageID] = pkt
			}
		default: // == recvNext: deliver, then drain any contiguous buffer
			c.recvReady = append(c.recvReady, pkt)
			c.recvNext++
			for {
				next, ok := c.recvBuf[c.recvNext]
				if !ok {
					break
				}
				delete(c.recvBuf, c.recvNext)
				c.recvReady = append(c.recvReady, next)
				c.recvNext++
			}
		}
		c.mu.Unlock()
	}
}

func (c *ControlChannel) write(ctx context.Context, pkt *ControlPacket) error {
	c.mu.Lock()
	c.sendPacketID++
	packetID := c.sendPacketID
	c.mu.Unlock()
	encoded, err := pkt.Encode(c.crypt, packetID, uint32(c.clock().Unix()))
	if err != nil {
		return err
	}
	return c.io.WritePacket(ctx, encoded)
}

func appendUint32Set(s []uint32, v uint32) []uint32 {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}

// ControlConn adapts a ControlChannel into a net.Conn so the standard
// crypto/tls stack can run the control-channel TLS handshake over it.
// Each Write becomes one P_CONTROL_V1 message; each Read drains one.
type ControlConn struct {
	channel *ControlChannel
	ctx     context.Context

	mu      sync.Mutex
	readBuf []byte
	closed  bool
}

func NewControlConn(ctx context.Context, channel *ControlChannel) *ControlConn {
	return &ControlConn{channel: channel, ctx: ctx}
}

func (c *ControlConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if len(c.readBuf) > 0 {
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	for {
		pkt, err := c.channel.Read(c.ctx)
		if err != nil {
			return 0, err
		}
		if err := c.channel.SendAck(c.ctx); err != nil {
			return 0, err
		}
		if pkt.Opcode != PControlV1 || len(pkt.Payload) == 0 {
			continue
		}
		n := copy(b, pkt.Payload)
		if n < len(pkt.Payload) {
			c.mu.Lock()
			c.readBuf = append(c.readBuf, pkt.Payload[n:]...)
			c.mu.Unlock()
		}
		return n, nil
	}
}

// maxControlPayload caps the TLS bytes carried by one P_CONTROL_V1
// packet. OpenVPN's control channel has an MTU (tls-mtu, default 1250);
// an oversized control packet is dropped by the peer. The TLS stream is
// fragmented across packets and reassembled in order by the reliability
// layer.
const maxControlPayload = 1100

func (c *ControlConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	total := 0
	for len(b) > 0 {
		n := len(b)
		if n > maxControlPayload {
			n = maxControlPayload
		}
		if _, err := c.channel.Send(c.ctx, PControlV1, b[:n]); err != nil {
			return total, err
		}
		b = b[n:]
		total += n
	}
	return total, nil
}

func (c *ControlConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *ControlConn) LocalAddr() net.Addr                { return c.channel.io.LocalAddr() }
func (c *ControlConn) RemoteAddr() net.Addr               { return c.channel.io.RemoteAddr() }
func (c *ControlConn) SetDeadline(t time.Time) error      { return nil }
func (c *ControlConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *ControlConn) SetWriteDeadline(t time.Time) error { return nil }

var _ net.Conn = (*ControlConn)(nil)
