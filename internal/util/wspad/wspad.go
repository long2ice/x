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
	headerSize      = 2
	maxRealPerFrame = 0xFFFF // uint16 cap on real payload per frame

	defaultUpgradeBucketP  = 3 // 30% chance to upgrade to the next bucket
	defaultUpgradeBucketBN = 10

	// ListenerConn appends linear padding sized as a random percentage of the
	// real frame, in [listenerPadPctLow, listenerPadPctHigh]. The padding scales
	// with traffic, so the bias on the dialer-side machine's RX is predictable
	// (~10-15% above its TX) without the unbounded blow-up that fixed buckets
	// would cause on small control frames.
	listenerPadPctLow  = 10
	listenerPadPctHigh = 15
)

// Bucket policies — only the write side picks a target frame size; the read
// side accepts any padding amount because the wire format is self-describing.
//
//   - defaultBuckets: balanced anti-DPI, used by Conn().
//   - lightBuckets:    pad only sub-256 control frames; anything larger goes
//     on the wire raw to keep dialer-side overhead at zero. An earlier upper
//     bucket of 16384 catastrophically inflated typical 257B-16KB frames
//     (HTTPS request, smux window update) to 16KB, blowing up the dialer
//     machine's outbound by an order of magnitude — measured ~3-4x increase
//     in the in→out direction. Now frames above 256B are sent unpadded.
//   - ListenerConn uses linear-percent padding instead of buckets.
var (
	defaultBuckets = []int{256, 1024, 4096, 16384}
	lightBuckets   = []int{2, 64, 256}
)

// Conn wraps a WebsocketConn with the default symmetric padding layer.
// Both peers MUST use a wspad wrapper or the wire format will not line up.
func Conn(c ws_util.WebsocketConn) net.Conn {
	return newPaddedConn(c, bucketPick(defaultBuckets, defaultUpgradeBucketP))
}

// ListenerConn pads server→client frames by a random 10-15% of the real
// frame size. Combined with DialerConn this biases bytes toward the
// server→client direction, so the dialer-side machine sees RX > TX by a
// controllable margin — useful when the dialer is on a metered uplink and
// the listener side's downlink is cheap.
func ListenerConn(c ws_util.WebsocketConn) net.Conn {
	return newPaddedConn(c, linearPick(listenerPadPctLow, listenerPadPctHigh))
}

// DialerConn pads client→server frames into a tight bucket set, so the
// dialer side contributes minimal extra bytes. The reader logic is identical,
// so light/heavy peers remain wire-compatible.
func DialerConn(c ws_util.WebsocketConn) net.Conn {
	// Never upgrade — keep dialer-side padding strictly minimal.
	return newPaddedConn(c, bucketPick(lightBuckets, 0))
}

// bucketPick returns a pickSize function that rounds `needed` up to the next
// bucket size, optionally upgrading to the bucket after that with probability
// upgradeP/defaultUpgradeBucketBN. Frames larger than the biggest bucket pass
// through unpadded.
func bucketPick(buckets []int, upgradeP int) func(int) int {
	return func(needed int) int {
		for i, b := range buckets {
			if b >= needed {
				if i+1 < len(buckets) && upgradeP > 0 && rand.IntN(defaultUpgradeBucketBN) < upgradeP {
					return buckets[i+1]
				}
				return b
			}
		}
		return needed
	}
}

// linearPick returns a pickSize function that adds a random [lowPct, highPct]%
// of padding on top of `needed`. The result is capped so the on-wire frame
// never exceeds maxRealPerFrame + headerSize, which keeps the writer within
// the WS read limits configured on the peer.
func linearPick(lowPct, highPct int) func(int) int {
	const cap = maxRealPerFrame + headerSize
	return func(needed int) int {
		extraPct := lowPct
		if highPct > lowPct {
			extraPct += rand.IntN(highPct - lowPct + 1)
		}
		target := needed + needed*extraPct/100
		if target > cap {
			target = cap
		}
		if target < needed {
			target = needed
		}
		return target
	}
}

type paddedConn struct {
	ws ws_util.WebsocketConn

	rmu     sync.Mutex
	readBuf []byte // bytes from current message not yet returned to caller

	wmu sync.Mutex

	// Per-instance write policy: given `needed` (= headerSize + len(payload)),
	// returns the on-wire frame size. Read path is policy-agnostic.
	pickSize func(needed int) int
}

func newPaddedConn(c ws_util.WebsocketConn, pickSize func(int) int) *paddedConn {
	return &paddedConn{ws: c, pickSize: pickSize}
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
	target := c.pickSize(needed)
	if target < needed {
		target = needed
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
