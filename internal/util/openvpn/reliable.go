package openvpn

import (
	"errors"
	"time"
)

const (
	DefaultReliableCapacity = 12
	DefaultRetransmit       = 2 * time.Second
	DefaultMaxRetries       = 10
	DefaultAckDelay         = 50 * time.Millisecond
)

var (
	ErrReliableFull      = errors.New("openvpn: reliable send window full")
	ErrSessionMismatch   = errors.New("openvpn: control packet from unexpected session")
	ErrReliableExhausted = errors.New("openvpn: peer unreachable, retransmissions exhausted")
)

// Delivery is one in-order control packet handed up to the caller. The
// Opcode lets the caller distinguish handshake-state events (hard/soft
// reset) from TLS payload chunks (P_CONTROL_V1) without re-parsing.
type Delivery struct {
	Opcode  Opcode
	Payload []byte
}

// Reliability implements OpenVPN's control-channel reliability layer. It
// is transport-agnostic: feed inbound ControlPackets via Receive, pull
// outbound packets via PendingOut, drive time via NextDeadline + repeated
// PendingOut calls.
type Reliability struct {
	capacity   int
	retransmit time.Duration
	maxRetries int
	ackDelay   time.Duration
	now        func() time.Time

	localSID   SessionID
	remoteSID  SessionID
	haveRemote bool

	nextTxID uint32
	tx       []*txEntry

	expectedRx uint32
	rx         map[uint32][]byte
	rxOpcodes  map[uint32]Opcode

	pendingAck []uint32
	pendingAt  time.Time
}

type txEntry struct {
	id      uint32
	opcode  Opcode
	payload []byte
	sentAt  time.Time
	retries int
}

type Option func(*Reliability)

func WithCapacity(n int) Option             { return func(r *Reliability) { r.capacity = n } }
func WithRetransmit(d time.Duration) Option { return func(r *Reliability) { r.retransmit = d } }
func WithMaxRetries(n int) Option           { return func(r *Reliability) { r.maxRetries = n } }
func WithAckDelay(d time.Duration) Option   { return func(r *Reliability) { r.ackDelay = d } }
func WithClock(fn func() time.Time) Option  { return func(r *Reliability) { r.now = fn } }

func NewReliability(local SessionID, opts ...Option) *Reliability {
	r := &Reliability{
		capacity:   DefaultReliableCapacity,
		retransmit: DefaultRetransmit,
		maxRetries: DefaultMaxRetries,
		ackDelay:   DefaultAckDelay,
		now:        time.Now,
		localSID:   local,
		rx:         make(map[uint32][]byte),
		rxOpcodes:  make(map[uint32]Opcode),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Reliability) LocalSession() SessionID         { return r.localSID }
func (r *Reliability) RemoteSession() (SessionID, bool) { return r.remoteSID, r.haveRemote }
func (r *Reliability) WindowFree() int                  { return r.capacity - len(r.tx) }

// Receive processes one inbound control packet:
//   - locks/validates the remote session id
//   - consumes piggybacked ACKs (frees tx slots)
//   - schedules an ACK for the packet's id
//   - buffers the payload and drains any contiguous in-order prefix
//
// Returns the in-order deliveries newly ready for the caller.
func (r *Reliability) Receive(p *ControlPacket) ([]Delivery, error) {
	if !r.haveRemote {
		r.remoteSID = p.SessionID
		r.haveRemote = true
	} else if r.remoteSID != p.SessionID {
		return nil, ErrSessionMismatch
	}

	if len(p.Acks) > 0 {
		r.consumeAcks(p.Acks)
	}

	// Pure ACK packets carry no packet_id and no payload.
	if p.Opcode.IsAck() {
		return nil, nil
	}

	// Drop silently if peer raced too far ahead of our window; they'll
	// retransmit later once our ACKs catch up.
	if p.PacketID >= r.expectedRx+uint32(r.capacity) {
		return nil, nil
	}

	// Old or already-buffered duplicates: still ACK (the prior ACK may
	// have been lost) but don't re-buffer.
	r.scheduleAck(p.PacketID)
	if p.PacketID < r.expectedRx {
		return nil, nil
	}
	if _, ok := r.rx[p.PacketID]; ok {
		return nil, nil
	}

	r.rx[p.PacketID] = append([]byte(nil), p.Payload...)
	r.rxOpcodes[p.PacketID] = p.Opcode

	var out []Delivery
	for {
		payload, ok := r.rx[r.expectedRx]
		if !ok {
			break
		}
		op := r.rxOpcodes[r.expectedRx]
		delete(r.rx, r.expectedRx)
		delete(r.rxOpcodes, r.expectedRx)
		r.expectedRx++
		out = append(out, Delivery{Opcode: op, Payload: payload})
	}
	return out, nil
}

func (r *Reliability) consumeAcks(ids []uint32) {
	acked := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		acked[id] = struct{}{}
	}
	dst := r.tx[:0]
	for _, e := range r.tx {
		if _, ok := acked[e.id]; ok {
			continue
		}
		dst = append(dst, e)
	}
	r.tx = dst
}

func (r *Reliability) scheduleAck(id uint32) {
	for _, a := range r.pendingAck {
		if a == id {
			return
		}
	}
	if len(r.pendingAck) == 0 {
		r.pendingAt = r.now()
	}
	r.pendingAck = append(r.pendingAck, id)
}

// Enqueue queues a payload for reliable transmission under the given
// control opcode (P_CONTROL_V1 for TLS bytes, P_CONTROL_HARD_RESET_*_V2
// for session init, P_CONTROL_SOFT_RESET_V1 for rekey). Returns
// ErrReliableFull if the send window is saturated.
func (r *Reliability) Enqueue(op Opcode, payload []byte) error {
	if !op.IsControl() {
		return ErrWrongOpcode
	}
	if len(r.tx) >= r.capacity {
		return ErrReliableFull
	}
	r.tx = append(r.tx, &txEntry{
		id:      r.nextTxID,
		opcode:  op,
		payload: append([]byte(nil), payload...),
	})
	r.nextTxID++
	return nil
}

// PendingOut returns the control packets that should be transmitted now:
// any never-sent tx entries (fresh sends), any retransmissions whose
// timer has elapsed, plus a standalone P_ACK_V1 if pending ACKs aged past
// ackDelay without a piggyback opportunity.
func (r *Reliability) PendingOut() ([]*ControlPacket, error) {
	now := r.now()
	var out []*ControlPacket

	for _, e := range r.tx {
		fresh := e.sentAt.IsZero()
		if !fresh && now.Sub(e.sentAt) < r.retransmit {
			continue
		}
		if !fresh {
			if e.retries >= r.maxRetries {
				return out, ErrReliableExhausted
			}
			e.retries++
		}
		e.sentAt = now
		pkt := &ControlPacket{
			Opcode:    e.opcode,
			SessionID: r.localSID,
			PacketID:  e.id,
			Payload:   append([]byte(nil), e.payload...),
		}
		r.attachAcks(pkt)
		out = append(out, pkt)
	}

	if len(r.pendingAck) > 0 && r.haveRemote && now.Sub(r.pendingAt) >= r.ackDelay {
		pkt := &ControlPacket{Opcode: PAckV1, SessionID: r.localSID}
		r.attachAcks(pkt)
		out = append(out, pkt)
	}
	return out, nil
}

func (r *Reliability) attachAcks(pkt *ControlPacket) {
	if !r.haveRemote || len(r.pendingAck) == 0 {
		return
	}
	n := len(r.pendingAck)
	if n > MaxAckCount {
		n = MaxAckCount
	}
	pkt.Acks = append([]uint32(nil), r.pendingAck[:n]...)
	pkt.RemoteID = r.remoteSID
	r.pendingAck = r.pendingAck[n:]
	if len(r.pendingAck) == 0 {
		r.pendingAt = time.Time{}
	}
}

// NextDeadline returns the next time at which PendingOut may have work
// (a retransmit timer firing or an ack-delay piggyback window closing).
// Zero time means no work is scheduled.
func (r *Reliability) NextDeadline() time.Time {
	var next time.Time
	for _, e := range r.tx {
		if e.sentAt.IsZero() {
			return r.now()
		}
		due := e.sentAt.Add(r.retransmit)
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	if len(r.pendingAck) > 0 && r.haveRemote {
		due := r.pendingAt.Add(r.ackDelay)
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}
