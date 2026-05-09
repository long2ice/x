// Package wspad wraps a WebSocket connection so that every binary message
// carries a length-prefixed payload padded with random bytes to a bucket
// size. The goal is to break the 1:1 size correlation between the inner
// stream's writes and the outer WS frame, hiding inner-protocol shape
// against statistical traffic classifiers.
//
// Wire format of each WS binary message:
//
//	[2 bytes BE: real payload length N]
//	[N bytes:     real payload]
//	[K bytes:     random padding so total is one of the bucket sizes]
package wspad

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	xio "github.com/go-gost/x/internal/io"
	ws_util "github.com/go-gost/x/internal/util/ws"
	"github.com/gorilla/websocket"
)

const (
	headerSize       = 2
	maxRealPerFrame  = 0xFFFF // uint16 cap per padded frame
	upgradeBucketP   = 3      // 30% chance to upgrade to the next bucket
	upgradeBucketBN  = 10
	largeUnpadCutoff = 16384 // payloads larger than this are sent without padding
)

// sizeBuckets are the target frame sizes (including 2-byte header).
// 256 / 1024 / 4096 / 16384 — chosen to span the typical inner-frame range.
var sizeBuckets = [...]int{256, 1024, 4096, 16384}

// Conn wraps a WebsocketConn with a padding layer. The returned net.Conn
// reads and writes plaintext bytes; the wire carries padded WS messages.
//
// Both peers MUST use this wrapper or wire-incompatible.
func Conn(c ws_util.WebsocketConn) net.Conn {
	return &paddedConn{ws: c}
}

type paddedConn struct {
	ws ws_util.WebsocketConn

	rmu     sync.Mutex
	readBuf []byte // bytes from current message not yet returned to caller

	wmu sync.Mutex
}

func (c *paddedConn) Read(b []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()

	if len(c.readBuf) > 0 {
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}

	mt, msg, err := c.ws.ReadMessage()
	if err != nil {
		return 0, err
	}
	if mt != websocket.BinaryMessage {
		return 0, fmt.Errorf("wspad: unexpected message type %d", mt)
	}
	if len(msg) < headerSize {
		return 0, errors.New("wspad: short padded frame")
	}
	realLen := int(binary.BigEndian.Uint16(msg[:headerSize]))
	if headerSize+realLen > len(msg) {
		return 0, fmt.Errorf("wspad: padded frame length overflow: real=%d msg=%d", realLen, len(msg))
	}

	payload := msg[headerSize : headerSize+realLen]
	n := copy(b, payload)
	if n < len(payload) {
		c.readBuf = append(c.readBuf, payload[n:]...)
	}
	return n, nil
}

func (c *paddedConn) Write(b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	written := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > maxRealPerFrame {
			chunk = chunk[:maxRealPerFrame]
		}
		if err := c.writeFrame(chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		b = b[len(chunk):]
	}
	return written, nil
}

func (c *paddedConn) writeFrame(b []byte) error {
	needed := headerSize + len(b)

	var target int
	if needed > largeUnpadCutoff {
		target = needed // do not pad very large frames
	} else {
		target = chooseBucket(needed)
	}

	frame := make([]byte, target)
	binary.BigEndian.PutUint16(frame[:headerSize], uint16(len(b)))
	copy(frame[headerSize:], b)
	if needed < target {
		if _, err := crand.Read(frame[needed:]); err != nil {
			return err
		}
	}
	return c.ws.WriteMessage(websocket.BinaryMessage, frame)
}

func chooseBucket(needed int) int {
	for i, b := range sizeBuckets {
		if b >= needed {
			if i+1 < len(sizeBuckets) && rand.IntN(upgradeBucketBN) < upgradeBucketP {
				return sizeBuckets[i+1]
			}
			return b
		}
	}
	return needed
}

// net.Conn delegations

func (c *paddedConn) Close() error                       { return c.ws.Close() }
func (c *paddedConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *paddedConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *paddedConn) SetDeadline(t time.Time) error      { return c.ws.SetDeadline(t) }
func (c *paddedConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *paddedConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

// Optional pass-through for context-aware callers.
func (c *paddedConn) Context() context.Context {
	if cc, ok := c.ws.(interface{ Context() context.Context }); ok {
		return cc.Context()
	}
	return context.Background()
}

// CloseRead/CloseWrite pass-through.
func (c *paddedConn) CloseRead() error {
	if sc, ok := c.ws.(xio.CloseRead); ok {
		return sc.CloseRead()
	}
	return xio.ErrUnsupported
}

func (c *paddedConn) CloseWrite() error {
	if sc, ok := c.ws.(xio.CloseWrite); ok {
		return sc.CloseWrite()
	}
	return xio.ErrUnsupported
}
