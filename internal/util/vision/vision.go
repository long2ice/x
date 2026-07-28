// Package vision implements the XTLS Vision flow (xtls-rprx-vision).
//
// Vision hides the length signature of the TLS handshake that the proxied
// traffic carries inside the outer TLS connection. While the inner handshake
// is in flight both peers wrap their data in padded frames:
//
//	+-----------+---------+------------+------------+---------+---------+
//	| uuid(16)  | command | contentLen | paddingLen | content | padding |
//	+-----------+---------+------------+------------+---------+---------+
//
// The uuid only prefixes the very first frame. The command ends the padding
// phase: 0 keeps it going, 1 ends it, and 2 ends it and switches the sender to
// writing straight to the transport below the outer TLS layer, since the inner
// traffic is TLS records already and does not need to be encrypted twice.
package vision

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
)

const (
	cmdPaddingContinue byte = 0
	cmdPaddingEnd      byte = 1
	cmdPaddingDirect   byte = 2

	// bufferSize is the buffer size Xray pads against, the padding header
	// takes at most 21 bytes of it.
	bufferSize     = 8192
	maxContentSize = bufferSize - 21

	// packetsToFilter is how many buffers are inspected for a TLS handshake
	// before giving up on detecting the inner protocol.
	packetsToFilter = 8
)

var (
	tlsClientHandshakeStart = []byte{0x16, 0x03}
	tlsServerHandshakeStart = []byte{0x16, 0x03, 0x03}
	tlsApplicationDataStart = []byte{0x17, 0x03, 0x03}
	tls13SupportedVersions  = []byte{0x00, 0x2b, 0x00, 0x02, 0x03, 0x04}
)

const (
	tlsHandshakeTypeClientHello byte = 0x01
	tlsHandshakeTypeServerHello byte = 0x02

	// cipherAES128CCM8SHA256 is the only TLS 1.3 cipher direct copy is not
	// used with, its short tag makes the record lengths ambiguous.
	cipherAES128CCM8SHA256 uint16 = 0x1305
)

var ErrNoRawConn = errors.New("vision: the peer switched to direct copy but the transport is not available")

// RawConner is implemented by connections that can hand over the transport
// below their TLS layer, which XTLS Vision needs for direct copy.
type RawConner interface {
	// RawConn returns the connection below the TLS layer.
	RawConn() net.Conn

	// TLSBuffered returns the data the TLS layer has read but not handed out
	// yet, which belongs to the raw stream once the peer switched to direct
	// copy. It must only be called once, when switching the reads over.
	TLSBuffered() io.Reader
}

// Conn wraps a connection with the Vision flow. It is symmetric, both the
// client and the server side of a connection use it the same way.
type Conn struct {
	net.Conn
	uuid [16]byte
	raw  RawConner

	// direct enables switching to direct copy once the inner traffic is
	// known to be TLS 1.3.
	direct bool

	// inner protocol detection, fed by both directions and so guarded
	mu                   sync.Mutex
	filter               int
	isTLS                bool
	isTLS12orAbove       bool
	cipher               uint16
	remainingServerHello int32
	enableDirect         bool

	// read state
	reader           io.Reader
	rbuf             []byte
	pending          bytes.Buffer
	rerr             error
	withinPadding    bool
	remainingCommand int32
	remainingContent int32
	remainingPadding int32
	currentCommand   int
	directRead       atomic.Bool

	// write state
	writer      io.Writer
	wbuf        []byte
	sendUUID    bool
	isPadding   bool
	switchWrite bool
	directWrite atomic.Bool
}

// DirectCopy reports whether the connection has handed its transport over to
// direct copy, for reading and for writing.
func (c *Conn) DirectCopy() (read, write bool) {
	return c.directRead.Load(), c.directWrite.Load()
}

// NewConn returns conn wrapped with the Vision flow. raw may be nil, in which
// case the connection cannot switch to direct copy, and fails if the peer
// does.
func NewConn(conn net.Conn, uuid [16]byte, raw RawConner, direct bool) *Conn {
	return &Conn{
		Conn:             conn,
		uuid:             uuid,
		raw:              raw,
		direct:           direct,
		filter:           packetsToFilter,
		reader:           conn,
		writer:           conn,
		rbuf:             make([]byte, bufferSize),
		withinPadding:    true,
		remainingCommand: -1,
		remainingContent: -1,
		remainingPadding: -1,
		sendUUID:         true,
		isPadding:        true,
	}
}

func (c *Conn) Read(p []byte) (int, error) {
	for {
		if c.pending.Len() > 0 {
			return c.pending.Read(p)
		}
		if c.rerr != nil {
			return 0, c.rerr
		}
		if c.directRead.Load() {
			return c.reader.Read(p)
		}

		n, err := c.reader.Read(c.rbuf)
		if n > 0 {
			if perr := c.readProcess(c.rbuf[:n]); perr != nil {
				return 0, perr
			}
		}
		if err != nil {
			if c.pending.Len() == 0 {
				return 0, err
			}
			c.rerr = err
		}
	}
}

// readProcess strips the padding of a buffer just read and appends its content
// to the pending data, switching to direct copy when the peer asks for it.
func (c *Conn) readProcess(b []byte) error {
	c.mu.Lock()
	filtering := c.filter > 0
	c.mu.Unlock()

	if c.withinPadding || filtering {
		b = c.unpad(b)

		switch {
		case c.remainingContent > 0 || c.remainingPadding > 0 || c.currentCommand == 0:
			c.withinPadding = true
		case c.currentCommand == 1:
			c.withinPadding = false
		case c.currentCommand == 2:
			c.withinPadding = false
			c.directRead.Store(true)
		}
	}

	c.mu.Lock()
	if c.filter > 0 {
		c.filterTLS(b)
	}
	c.mu.Unlock()

	c.pending.Write(b)

	if c.directRead.Load() {
		if c.raw == nil {
			return ErrNoRawConn
		}
		if buffered := c.raw.TLSBuffered(); buffered != nil {
			if _, err := c.pending.ReadFrom(buffered); err != nil {
				return err
			}
		}
		c.reader = c.raw.RawConn()
	}

	return nil
}

// unpad decodes the padded frames of b and returns their content. Frames span
// buffers, the decoding state is kept between calls.
func (c *Conn) unpad(b []byte) []byte {
	if c.remainingCommand == -1 && c.remainingContent == -1 && c.remainingPadding == -1 {
		// The first frame of the peer is prefixed with the user id, anything
		// else is unpadded data.
		if len(b) < 21 || !bytes.Equal(c.uuid[:], b[:16]) {
			return b
		}
		b = b[16:]
		c.remainingCommand = 5
	}

	out := b[:0]
	for len(b) > 0 {
		switch {
		case c.remainingCommand > 0:
			switch d := b[0]; c.remainingCommand {
			case 5:
				c.currentCommand = int(d)
			case 4:
				c.remainingContent = int32(d) << 8
			case 3:
				c.remainingContent |= int32(d)
			case 2:
				c.remainingPadding = int32(d) << 8
			case 1:
				c.remainingPadding |= int32(d)
			}
			b = b[1:]
			c.remainingCommand--

		case c.remainingContent > 0:
			n := int(c.remainingContent)
			if n > len(b) {
				n = len(b)
			}
			out = append(out, b[:n]...)
			b = b[n:]
			c.remainingContent -= int32(n)

		default:
			n := int(c.remainingPadding)
			if n > len(b) {
				n = len(b)
			}
			b = b[n:]
			c.remainingPadding -= int32(n)
		}

		if c.remainingCommand <= 0 && c.remainingContent <= 0 && c.remainingPadding <= 0 {
			if c.currentCommand == 0 {
				c.remainingCommand = 5
				continue
			}
			c.remainingCommand, c.remainingContent, c.remainingPadding = -1, -1, -1
			// The peer ended the padding, the rest of the buffer is raw data.
			out = append(out, b...)
			break
		}
	}

	return out
}

func (c *Conn) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.filter > 0 {
		c.filterTLS(b)
	}
	isTLS, isTLS12orAbove, filter := c.isTLS, c.isTLS12orAbove, c.filter
	c.mu.Unlock()

	if !c.isPadding {
		n, err := c.writer.Write(b)
		c.switchDirectWrite()
		return n, err
	}

	total := len(b)
	isComplete := isCompleteRecord(b)
	longPadding := isTLS

	for len(b) > 0 {
		content := b
		if len(content) > maxContentSize {
			content = content[:maxContentSize]
		}
		b = b[len(content):]
		last := len(b) == 0

		command := cmdPaddingContinue
		notTLS := false

		switch {
		case isTLS && isComplete && len(content) >= 6 && bytes.Equal(tlsApplicationDataStart, content[:3]):
			// The inner handshake is over, stop padding after this write.
			if last {
				command = c.endCommand()
			}
			c.isPadding = false
			longPadding = false

		case !isTLS12orAbove && filter <= 1:
			// The inner traffic is not TLS, there is no handshake to hide.
			// Direct copy is not an option here, it needs TLS 1.3 records.
			command = cmdPaddingEnd
			c.isPadding = false
			notTLS = true

		case last && !c.isPadding:
			command = c.endCommand()
		}

		if _, err := c.writer.Write(c.pad(content, command, longPadding)); err != nil {
			return 0, err
		}

		if notTLS {
			// The rest of this write needs no padding anymore.
			if len(b) > 0 {
				if _, err := c.writer.Write(b); err != nil {
					return 0, err
				}
			}
			break
		}
	}

	c.switchDirectWrite()

	return total, nil
}

// endCommand returns the command that ends the padding phase, asking for
// direct copy when the inner traffic is TLS 1.3.
func (c *Conn) endCommand() byte {
	c.mu.Lock()
	enableDirect := c.enableDirect
	c.mu.Unlock()

	if c.direct && enableDirect && c.raw != nil {
		c.switchWrite = true
		return cmdPaddingDirect
	}
	return cmdPaddingEnd
}

// switchDirectWrite starts writing below the outer TLS layer, it has to happen
// after the frame that told the peer about it has been written.
func (c *Conn) switchDirectWrite() {
	if !c.switchWrite {
		return
	}
	c.switchWrite = false
	c.directWrite.Store(true)

	c.writer = c.raw.RawConn()
}

// pad wraps content in a padded frame.
func (c *Conn) pad(content []byte, command byte, longPadding bool) []byte {
	contentLen := len(content)

	var paddingLen int
	if contentLen < 900 && longPadding {
		paddingLen = randInt(500) + 900 - contentLen
	} else {
		paddingLen = randInt(256)
	}
	if n := maxContentSize - contentLen; paddingLen > n {
		paddingLen = n
	}
	if paddingLen < 0 {
		paddingLen = 0
	}

	size := 21 + contentLen + paddingLen
	if cap(c.wbuf) < size {
		c.wbuf = make([]byte, 0, size)
	}

	b := c.wbuf[:0]
	if c.sendUUID {
		b = append(b, c.uuid[:]...)
		c.sendUUID = false
	}
	b = append(b, command)
	b = binary.BigEndian.AppendUint16(b, uint16(contentLen))
	b = binary.BigEndian.AppendUint16(b, uint16(paddingLen))
	b = append(b, content...)
	b = append(b, make([]byte, paddingLen)...)
	c.wbuf = b

	return b
}

func randInt(n int64) int {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// filterTLS looks for a TLS handshake in the proxied traffic, both directions
// feed it until the inner protocol is known.
func (c *Conn) filterTLS(b []byte) {
	c.filter--

	if len(b) >= 6 {
		switch {
		case bytes.Equal(tlsServerHandshakeStart, b[:3]) && b[5] == tlsHandshakeTypeServerHello:
			c.remainingServerHello = (int32(b[3])<<8 | int32(b[4])) + 5
			c.isTLS = true
			c.isTLS12orAbove = true

			// The cipher suite follows the session id of the server hello.
			if len(b) >= 79 && c.remainingServerHello >= 79 {
				if n := 43 + int(b[43]) + 3; n <= len(b) {
					c.cipher = binary.BigEndian.Uint16(b[n-2 : n])
				}
			}

		case bytes.Equal(tlsClientHandshakeStart, b[:2]) && b[5] == tlsHandshakeTypeClientHello:
			c.isTLS = true
		}
	}

	if c.remainingServerHello > 0 {
		end := int(c.remainingServerHello)
		if end > len(b) {
			end = len(b)
		}
		c.remainingServerHello -= int32(len(b))

		if bytes.Contains(b[:end], tls13SupportedVersions) {
			c.enableDirect = c.cipher != cipherAES128CCM8SHA256
			c.filter = 0
		} else if c.remainingServerHello <= 0 {
			// TLS 1.2, direct copy is not safe with it.
			c.filter = 0
		}
	}
}

// isCompleteRecord reports whether b holds whole TLS application data records
// and nothing else. Padding may only end on a record boundary, otherwise the
// peer would receive a partial record it cannot forward.
func isCompleteRecord(b []byte) bool {
	for len(b) > 0 {
		if len(b) < 5 || !bytes.Equal(tlsApplicationDataStart, b[:3]) {
			return false
		}
		n := 5 + int(binary.BigEndian.Uint16(b[3:5]))
		if len(b) < n {
			return false
		}
		b = b[n:]
	}
	return true
}
