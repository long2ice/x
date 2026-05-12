package openvpn

import (
	"bytes"
	crand "crypto/rand"
	"errors"
	mrand "math/rand/v2"
	"net"
	"os"
	"sync"
	"time"
)

const (
	// DataChunkMax / DataChunkMin frame the random size of each
	// data-channel packet. Real OpenVPN data packets carry IP frames of
	// varied sizes; always-1400-byte packets are an obvious statistical
	// fingerprint. Jittering 1200-1400 adds enough variation to hide that
	// while staying under MTU.
	DataChunkMax = 1400
	DataChunkMin = 1200

	defaultKeepaliveInterval = 10 * time.Second
	keepaliveJitterPct       = 20 // ±20%

	// Mimic OpenVPN's PUSH_REPLY-then-idle pattern: server emits one
	// fake control packet of TLS-record-ish size, then both ends sleep
	// briefly before letting data flow. Without this, a passive
	// observer sees "handshake done → instant fullspeed" which is not
	// how real OpenVPN behaves.
	fakePushMinSize          = 280
	fakePushMaxSize          = 420
	postHandshakeDelayMin    = 600 * time.Millisecond
	postHandshakeDelayMax    = 1400 * time.Millisecond
	postHandshakeReadTimeout = 3 * time.Second

	udpReadBuffer = 2048
)

// pingString is OpenVPN's standard keepalive payload (src/openvpn/ping.h).
// Reusing the exact bytes means a captured-and-decrypted keepalive looks
// bit-for-bit identical to real OpenVPN's pings.
var pingString = []byte{
	0x2a, 0x18, 0x7b, 0xf3, 0x64, 0x1e, 0xb4, 0xcb,
	0x07, 0xed, 0x2d, 0x0a, 0x98, 0x1f, 0xc7, 0x48,
}

var (
	ErrUnexpectedOpcode = errors.New("openvpn: unexpected control opcode during handshake")
	ErrHandshakeAborted = errors.New("openvpn: handshake aborted before completion")
)

// Tunnel implements net.Conn for the post-handshake data channel.
//
//   - framed=true  (TCP): wire I/O uses 2-byte length-prefix framing
//   - framed=false (UDP): each net.Conn Read/Write is one datagram
type Tunnel struct {
	conn       net.Conn
	framed     bool
	sendCipher *DataCipher
	recvCipher *DataCipher

	readMu  sync.Mutex
	readBuf []byte

	writeMu sync.Mutex

	lastSendMu sync.Mutex
	lastSend   time.Time

	keepaliveInterval time.Duration
	closeOnce         sync.Once
	closed            chan struct{}
	closeErr          error
}

func ServerHandshake(conn net.Conn, psk []byte) (*Tunnel, error) {
	return runHandshake(conn, psk, true, true)
}

func ClientHandshake(conn net.Conn, psk []byte) (*Tunnel, error) {
	return runHandshake(conn, psk, false, true)
}

func ServerHandshakePacket(conn net.Conn, psk []byte) (*Tunnel, error) {
	return runHandshake(conn, psk, true, false)
}

func ClientHandshakePacket(conn net.Conn, psk []byte) (*Tunnel, error) {
	return runHandshake(conn, psk, false, false)
}

func runHandshake(conn net.Conn, psk []byte, isServer, framed bool) (*Tunnel, error) {
	localSID, err := NewSessionID()
	if err != nil {
		return nil, err
	}
	rel := NewReliability(localSID)
	crypt, err := NewControlCipher(psk)
	if err != nil {
		return nil, err
	}
	var hs *Handshake
	if isServer {
		hs = NewServerHandshake(psk)
	} else {
		hs = NewClientHandshake(psk)
	}

	if !isServer {
		if err := rel.Enqueue(PControlHardResetClientV2, nil); err != nil {
			return nil, err
		}
		if err := flushControl(conn, rel, crypt, framed); err != nil {
			return nil, err
		}
	}

	for !hs.Done() {
		if !framed {
			next := rel.NextDeadline()
			if next.IsZero() {
				next = time.Now().Add(time.Second)
			}
			_ = conn.SetReadDeadline(next)
		}

		wire, err := readControlWire(conn, framed)
		if err != nil {
			if !framed && isDeadlineErr(err) {
				if err := flushControl(conn, rel, crypt, framed); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}
		if wire == nil {
			continue
		}

		plain, err := crypt.Unwrap(wire)
		if err != nil {
			if !framed {
				continue
			}
			return nil, err
		}
		ctrl, err := DecodeControlPacket(plain)
		if err != nil {
			if !framed {
				continue
			}
			return nil, err
		}
		deliveries, err := rel.Receive(ctrl)
		if err != nil {
			if !framed {
				continue
			}
			return nil, err
		}
		for _, d := range deliveries {
			if err := stepHandshake(rel, hs, isServer, d); err != nil {
				return nil, err
			}
		}
		if err := flushControl(conn, rel, crypt, framed); err != nil {
			return nil, err
		}
	}

	if !framed {
		_ = conn.SetReadDeadline(time.Time{})
	}

	sessionKey := hs.SessionKey()
	if len(sessionKey) == 0 {
		return nil, ErrHandshakeAborted
	}

	if err := postHandshakeDecoy(conn, rel, crypt, framed, isServer); err != nil {
		return nil, err
	}

	send, recv, err := NewDataCipherPair(sessionKey, isServer)
	if err != nil {
		return nil, err
	}
	t := &Tunnel{
		conn:              conn,
		framed:            framed,
		sendCipher:        send,
		recvCipher:        recv,
		keepaliveInterval: defaultKeepaliveInterval,
		closed:            make(chan struct{}),
		lastSend:          time.Now(),
	}
	go t.keepaliveLoop()
	return t, nil
}

// postHandshakeDecoy mimics OpenVPN's PUSH_REPLY round trip + the brief
// idle period before data starts to flow. Server emits one fake control
// packet (encrypted random bytes sized like a small TLS record); client
// reads one packet (and ignores its decrypted content). Both then sleep
// for a randomised 600-1400ms.
func postHandshakeDecoy(conn net.Conn, rel *Reliability, crypt *ControlCipher, framed, isServer bool) error {
	if isServer {
		sz := fakePushMinSize + mrand.IntN(fakePushMaxSize-fakePushMinSize+1)
		payload := make([]byte, sz)
		if _, err := crand.Read(payload); err != nil {
			return err
		}
		if err := rel.Enqueue(PControlV1, payload); err != nil {
			return err
		}
		if err := flushControl(conn, rel, crypt, framed); err != nil {
			return err
		}
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(postHandshakeReadTimeout))
		_, err := readControlWire(conn, framed)
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil && !isDeadlineErr(err) {
			return err
		}
	}
	jitter := postHandshakeDelayMin + time.Duration(mrand.Int64N(int64(postHandshakeDelayMax-postHandshakeDelayMin)))
	time.Sleep(jitter)
	return nil
}

func stepHandshake(rel *Reliability, hs *Handshake, isServer bool, d Delivery) error {
	switch d.Opcode {
	case PControlHardResetClientV2:
		if !isServer {
			return ErrUnexpectedOpcode
		}
		return rel.Enqueue(PControlHardResetServerV2, nil)
	case PControlHardResetServerV2:
		if isServer {
			return ErrUnexpectedOpcode
		}
		hello, err := hs.Initial()
		if err != nil {
			return err
		}
		return rel.Enqueue(PControlV1, hello)
	case PControlV1:
		resp, _, err := hs.Receive(d.Payload)
		if err != nil {
			return err
		}
		if resp == nil {
			return nil
		}
		return rel.Enqueue(PControlV1, resp)
	default:
		return ErrUnexpectedOpcode
	}
}

func readControlWire(conn net.Conn, framed bool) ([]byte, error) {
	if framed {
		return ReadFramedPacket(conn)
	}
	buf := make([]byte, udpReadBuffer)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func flushControl(conn net.Conn, rel *Reliability, crypt *ControlCipher, framed bool) error {
	pkts, err := rel.PendingOut()
	if err != nil {
		return err
	}
	for _, pkt := range pkts {
		plain, err := pkt.Encode(nil)
		if err != nil {
			return err
		}
		wire, err := crypt.Wrap(plain)
		if err != nil {
			return err
		}
		if framed {
			if err := WriteFramedPacket(conn, wire); err != nil {
				return err
			}
		} else {
			if _, err := conn.Write(wire); err != nil {
				return err
			}
		}
	}
	return nil
}

func isDeadlineErr(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

func (t *Tunnel) Read(b []byte) (int, error) {
	t.readMu.Lock()
	defer t.readMu.Unlock()
	for len(t.readBuf) == 0 {
		if err := t.refill(); err != nil {
			return 0, err
		}
	}
	n := copy(b, t.readBuf)
	t.readBuf = t.readBuf[n:]
	return n, nil
}

func (t *Tunnel) refill() error {
	for {
		wire, err := readControlWire(t.conn, t.framed)
		if err != nil {
			return err
		}
		if wire == nil {
			continue
		}
		dp, err := DecodeDataPacket(wire)
		if err != nil {
			if !t.framed {
				continue
			}
			return err
		}
		pt, err := t.recvCipher.Open(dp.Payload)
		if err != nil {
			if !t.framed {
				continue
			}
			return err
		}
		// Drop keepalives at the protocol layer; the upper handler should
		// never see them.
		if bytes.Equal(pt, pingString) {
			continue
		}
		t.readBuf = pt
		return nil
	}
}

func (t *Tunnel) Write(b []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	total := 0
	for len(b) > 0 {
		n := len(b)
		maxChunk := DataChunkMin + mrand.IntN(DataChunkMax-DataChunkMin+1)
		if n > maxChunk {
			n = maxChunk
		}
		if err := t.writeDataLocked(b[:n]); err != nil {
			return total, err
		}
		total += n
		b = b[n:]
	}
	return total, nil
}

// writeDataLocked encrypts and ships one data-channel packet. Caller
// must hold writeMu.
func (t *Tunnel) writeDataLocked(payload []byte) error {
	ct, err := t.sendCipher.Seal(payload)
	if err != nil {
		return err
	}
	dp := &DataPacket{Opcode: PDataV2, Payload: ct}
	wire, err := dp.Encode(nil)
	if err != nil {
		return err
	}
	if t.framed {
		if err := WriteFramedPacket(t.conn, wire); err != nil {
			return err
		}
	} else {
		if _, err := t.conn.Write(wire); err != nil {
			return err
		}
	}
	t.lastSendMu.Lock()
	t.lastSend = time.Now()
	t.lastSendMu.Unlock()
	return nil
}

func (t *Tunnel) keepaliveLoop() {
	if t.keepaliveInterval <= 0 {
		return
	}
	for {
		base := t.keepaliveInterval
		jitter := time.Duration(mrand.Int64N(int64(base)*2*keepaliveJitterPct/100)) - time.Duration(int64(base)*keepaliveJitterPct/100)
		wait := base + jitter
		select {
		case <-time.After(wait):
			t.lastSendMu.Lock()
			idle := time.Since(t.lastSend) >= base
			t.lastSendMu.Unlock()
			if !idle {
				continue
			}
			t.writeMu.Lock()
			err := t.writeDataLocked(pingString)
			t.writeMu.Unlock()
			if err != nil {
				_ = t.Close()
				return
			}
		case <-t.closed:
			return
		}
	}
}

func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.closeErr = t.conn.Close()
	})
	return t.closeErr
}

func (t *Tunnel) LocalAddr() net.Addr                { return t.conn.LocalAddr() }
func (t *Tunnel) RemoteAddr() net.Addr               { return t.conn.RemoteAddr() }
func (t *Tunnel) SetDeadline(d time.Time) error      { return t.conn.SetDeadline(d) }
func (t *Tunnel) SetReadDeadline(d time.Time) error  { return t.conn.SetReadDeadline(d) }
func (t *Tunnel) SetWriteDeadline(d time.Time) error { return t.conn.SetWriteDeadline(d) }
