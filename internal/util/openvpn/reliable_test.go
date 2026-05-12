package openvpn

import (
	"bytes"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time             { return c.t }
func (c *fakeClock) advance(d time.Duration)    { c.t = c.t.Add(d) }
func (c *fakeClock) set(t time.Time)            { c.t = t }

func newPair(t *testing.T) (*fakeClock, *Reliability, *Reliability) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	cli := NewReliability(SessionID{1, 1, 1, 1, 1, 1, 1, 1}, WithClock(clk.now))
	srv := NewReliability(SessionID{2, 2, 2, 2, 2, 2, 2, 2}, WithClock(clk.now))
	return clk, cli, srv
}

// ship pulls every PendingOut from `from`, encodes/decodes (catching wire
// codec bugs), and delivers to `to.Receive`. Returns the deliveries
// accumulated on the receiver side.
func ship(t *testing.T, from, to *Reliability) []Delivery {
	t.Helper()
	pkts, err := from.PendingOut()
	if err != nil {
		t.Fatalf("PendingOut: %v", err)
	}
	var all []Delivery
	for _, p := range pkts {
		enc, err := p.Encode(nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		dec, err := DecodeControlPacket(enc)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got, err := to.Receive(dec)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		all = append(all, got...)
	}
	return all
}

func TestHardResetHandshake(t *testing.T) {
	_, cli, srv := newPair(t)

	if err := cli.Enqueue(PControlHardResetClientV2, nil); err != nil {
		t.Fatal(err)
	}
	got := ship(t, cli, srv)
	if len(got) != 1 || got[0].Opcode != PControlHardResetClientV2 {
		t.Fatalf("server should have received HRC_V2, got %+v", got)
	}

	if err := srv.Enqueue(PControlHardResetServerV2, nil); err != nil {
		t.Fatal(err)
	}
	got = ship(t, srv, cli)
	if len(got) != 1 || got[0].Opcode != PControlHardResetServerV2 {
		t.Fatalf("client should have received HRS_V2, got %+v", got)
	}
	if cli.WindowFree() != DefaultReliableCapacity {
		t.Errorf("client tx window should be empty after HRS ack, free=%d", cli.WindowFree())
	}
}

func TestInOrderTLSDelivery(t *testing.T) {
	_, cli, srv := newPair(t)

	_ = cli.Enqueue(PControlHardResetClientV2, nil)
	ship(t, cli, srv)
	_ = srv.Enqueue(PControlHardResetServerV2, nil)
	ship(t, srv, cli)

	chunks := [][]byte{[]byte("client hello"), []byte("key share"), []byte("finished")}
	for _, c := range chunks {
		if err := cli.Enqueue(PControlV1, c); err != nil {
			t.Fatal(err)
		}
	}
	got := ship(t, cli, srv)
	if len(got) != len(chunks) {
		t.Fatalf("got %d deliveries, want %d", len(got), len(chunks))
	}
	for i, d := range got {
		if d.Opcode != PControlV1 || !bytes.Equal(d.Payload, chunks[i]) {
			t.Errorf("chunk %d mismatch: %+v", i, d)
		}
	}
}

func TestOutOfOrderBuffersAndDrains(t *testing.T) {
	_, cli, srv := newPair(t)

	_ = cli.Enqueue(PControlHardResetClientV2, nil)
	ship(t, cli, srv)
	_ = srv.Enqueue(PControlHardResetServerV2, nil)
	ship(t, srv, cli)

	for i, msg := range []string{"a", "b", "c"} {
		if err := cli.Enqueue(PControlV1, []byte(msg)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	pkts, err := cli.PendingOut()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 3 {
		t.Fatalf("want 3 outgoing pkts, got %d", len(pkts))
	}

	// Deliver in reverse order.
	for i := len(pkts) - 1; i >= 0; i-- {
		got, err := srv.Receive(pkts[i])
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && len(got) != 0 {
			t.Errorf("pkt %d delivered prematurely: %+v", i, got)
		}
		if i == 0 {
			if len(got) != 3 {
				t.Fatalf("final drain should yield 3, got %d", len(got))
			}
			for k, expect := range []string{"a", "b", "c"} {
				if string(got[k].Payload) != expect {
					t.Errorf("drained[%d] = %q, want %q", k, got[k].Payload, expect)
				}
			}
		}
	}
}

func TestDuplicatePacketReAcked(t *testing.T) {
	_, cli, srv := newPair(t)

	_ = cli.Enqueue(PControlHardResetClientV2, nil)
	pkts, _ := cli.PendingOut()
	pkt := pkts[0]

	if _, err := srv.Receive(pkt); err != nil {
		t.Fatal(err)
	}
	// Duplicate
	got, err := srv.Receive(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("duplicate should not redeliver: %+v", got)
	}
	if len(srv.pendingAck) != 1 {
		t.Errorf("server should still have one pending ack for the dup, got %d", len(srv.pendingAck))
	}
}

func TestWindowFull(t *testing.T) {
	_, cli, _ := newPair(t)

	for i := 0; i < DefaultReliableCapacity; i++ {
		if err := cli.Enqueue(PControlV1, []byte{byte(i)}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := cli.Enqueue(PControlV1, []byte{0xff}); err != ErrReliableFull {
		t.Errorf("expected ErrReliableFull, got %v", err)
	}
}

func TestWindowOpensOnAck(t *testing.T) {
	_, cli, srv := newPair(t)

	for i := 0; i < DefaultReliableCapacity; i++ {
		_ = cli.Enqueue(PControlV1, []byte{byte(i)})
	}
	if cli.WindowFree() != 0 {
		t.Fatalf("window should be full, free=%d", cli.WindowFree())
	}
	ship(t, cli, srv)
	// Server's pendingAck has all 12 ids; an ACK packet carries at most
	// MaxAckCount (8), so multiple ack rounds are needed to fully drain.
	// Push past ackDelay so standalone ACKs flow.
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).Add(time.Second)}
	srv.now = clk.now
	cli.now = clk.now
	for round := 0; round < 5 && cli.WindowFree() < DefaultReliableCapacity; round++ {
		ship(t, srv, cli)
	}
	if cli.WindowFree() != DefaultReliableCapacity {
		t.Errorf("window should reopen after ack, free=%d", cli.WindowFree())
	}
}

func TestRetransmitOnTimeout(t *testing.T) {
	clk, cli, _ := newPair(t)
	_ = cli.Enqueue(PControlV1, []byte("rtx"))

	pkts, err := cli.PendingOut()
	if err != nil || len(pkts) != 1 {
		t.Fatalf("initial send: %d pkts err=%v", len(pkts), err)
	}
	// Same instant: no retransmit yet.
	pkts, _ = cli.PendingOut()
	if len(pkts) != 0 {
		t.Errorf("should not retransmit immediately, got %d", len(pkts))
	}
	// Advance past retransmit interval.
	clk.advance(DefaultRetransmit + time.Millisecond)
	pkts, err = cli.PendingOut()
	if err != nil || len(pkts) != 1 {
		t.Fatalf("retransmit: %d pkts err=%v", len(pkts), err)
	}
}

func TestMaxRetriesExhausted(t *testing.T) {
	clk, cli, _ := newPair(t)
	_ = cli.Enqueue(PControlV1, []byte("doomed"))

	for i := 0; i < DefaultMaxRetries+5; i++ {
		if _, err := cli.PendingOut(); err == ErrReliableExhausted {
			return
		}
		clk.advance(DefaultRetransmit + time.Millisecond)
	}
	t.Errorf("expected ErrReliableExhausted after retransmissions")
}

func TestStandaloneAckAfterDelay(t *testing.T) {
	clk, cli, srv := newPair(t)
	_ = cli.Enqueue(PControlHardResetClientV2, nil)
	pkts, _ := cli.PendingOut()
	if _, err := srv.Receive(pkts[0]); err != nil {
		t.Fatal(err)
	}
	// Immediately: server's PendingOut shouldn't emit standalone ACK yet.
	out, _ := srv.PendingOut()
	if len(out) != 0 {
		t.Errorf("standalone ACK should wait for ackDelay, got %d", len(out))
	}
	clk.advance(DefaultAckDelay + time.Millisecond)
	out, _ = srv.PendingOut()
	if len(out) != 1 || out[0].Opcode != PAckV1 {
		t.Fatalf("expected one P_ACK_V1, got %+v", out)
	}
	if len(out[0].Acks) != 1 || out[0].Acks[0] != 0 {
		t.Errorf("expected ack of packet id 0, got %v", out[0].Acks)
	}
}

func TestSessionMismatchRejected(t *testing.T) {
	_, _, srv := newPair(t)

	pkt := &ControlPacket{
		Opcode:    PControlHardResetClientV2,
		SessionID: SessionID{1, 1, 1, 1, 1, 1, 1, 1},
	}
	if _, err := srv.Receive(pkt); err != nil {
		t.Fatalf("first receive should set remote sid: %v", err)
	}
	pkt.SessionID = SessionID{9, 9, 9, 9, 9, 9, 9, 9}
	if _, err := srv.Receive(pkt); err != ErrSessionMismatch {
		t.Errorf("expected ErrSessionMismatch, got %v", err)
	}
}

func TestFarAheadPacketDroppedSilently(t *testing.T) {
	_, _, srv := newPair(t)
	pkt := &ControlPacket{
		Opcode:    PControlV1,
		SessionID: SessionID{1, 1, 1, 1, 1, 1, 1, 1},
		PacketID:  uint32(DefaultReliableCapacity + 10),
		Payload:   []byte("future"),
	}
	got, err := srv.Receive(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("far-ahead packet should not deliver")
	}
	if len(srv.pendingAck) != 0 {
		t.Errorf("far-ahead packet should not be acked, got %d pending acks", len(srv.pendingAck))
	}
}

func TestPureAckDoesNotConsumePacketIDSlot(t *testing.T) {
	_, cli, srv := newPair(t)
	_ = cli.Enqueue(PControlHardResetClientV2, nil)
	pkts, _ := cli.PendingOut()
	_, _ = srv.Receive(pkts[0])

	ackPkt := &ControlPacket{
		Opcode:    PAckV1,
		SessionID: srv.localSID,
		Acks:      []uint32{0},
		RemoteID:  cli.localSID,
	}
	got, err := cli.Receive(ackPkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("pure ACK should not deliver anything: %+v", got)
	}
	if cli.WindowFree() != DefaultReliableCapacity {
		t.Errorf("pure ACK should have freed window slot, free=%d", cli.WindowFree())
	}
	if cli.expectedRx != 0 {
		t.Errorf("pure ACK should not advance expected rx, got %d", cli.expectedRx)
	}
}

func TestAckPiggybacksOnNextSend(t *testing.T) {
	_, cli, srv := newPair(t)
	_ = cli.Enqueue(PControlHardResetClientV2, nil)
	ship(t, cli, srv)

	// Server replies with HRS; this packet should piggyback ACK of client's id 0.
	_ = srv.Enqueue(PControlHardResetServerV2, nil)
	pkts, err := srv.PendingOut()
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 1 {
		t.Fatalf("want 1 outgoing, got %d", len(pkts))
	}
	if len(pkts[0].Acks) != 1 || pkts[0].Acks[0] != 0 {
		t.Errorf("HRS reply should piggyback ACK of client's id 0, got %v", pkts[0].Acks)
	}
	if pkts[0].RemoteID != cli.localSID {
		t.Errorf("piggyback should carry client's session id as remote, got %s", pkts[0].RemoteID)
	}
}

func TestNextDeadlineTracksRetransmitAndAck(t *testing.T) {
	clk, cli, srv := newPair(t)
	if d := cli.NextDeadline(); !d.IsZero() {
		t.Errorf("empty reliability should have zero deadline, got %v", d)
	}
	_ = cli.Enqueue(PControlV1, []byte("x"))
	// fresh send → deadline is "now"
	if d := cli.NextDeadline(); !d.Equal(clk.now()) {
		t.Errorf("fresh send deadline should be now, got %v", d)
	}
	pkts, _ := cli.PendingOut()
	_, _ = srv.Receive(pkts[0])
	// after send, deadline is sentAt + retransmit
	want := clk.now().Add(DefaultRetransmit)
	if d := cli.NextDeadline(); !d.Equal(want) {
		t.Errorf("after send, deadline=%v want=%v", d, want)
	}
	// server has a pending ack; deadline is pendingAt + ackDelay
	want = clk.now().Add(DefaultAckDelay)
	if d := srv.NextDeadline(); !d.Equal(want) {
		t.Errorf("server ack deadline=%v want=%v", d, want)
	}
}
