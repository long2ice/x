package vless

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// streamConn serves a fixed byte stream, so that a parser can be fed without
// a second goroutine that could block on an unread pipe.
type streamConn struct {
	r io.Reader
}

func (c *streamConn) Read(b []byte) (int, error)         { return c.r.Read(b) }
func (c *streamConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *streamConn) Close() error                       { return nil }
func (c *streamConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *streamConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *streamConn) SetDeadline(t time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return nil }

// FuzzReadRequest feeds the request parser what an unauthenticated peer can
// send, it must never panic.
func FuzzReadRequest(f *testing.F) {
	id, _ := ParseUUID("b831381d-6324-4d53-ad4f-8cda48b30811")
	for _, req := range []*Request{
		{ID: id, Command: CmdTCP, Host: "example.com", Port: 443},
		{ID: id, Command: CmdUDP, Host: "1.2.3.4", Port: 53},
		{ID: id, Command: CmdMux},
		{ID: id, Command: CmdTCP, Host: "::1", Port: 80, Flow: FlowVision},
	} {
		var buf bytes.Buffer
		WriteRequest(&buf, req)
		f.Add(buf.Bytes())
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		ReadRequest(bytes.NewReader(b))
	})
}

// FuzzXUDP feeds the XUDP framing what an authenticated but hostile peer can
// send, it must never panic.
func FuzzXUDP(f *testing.F) {
	f.Add([]byte{0, 8, 0, 0, 1, 1, 2, 0, 53, 1, 8, 8, 8, 8, 0, 4, 't', 'e', 's', 't'})
	f.Add([]byte{0, 4, 0, 0, 2, 1, 0, 2, 'h', 'i'})
	f.Add([]byte{0, 5, 0, 0, 1, 1, 1})

	f.Fuzz(func(t *testing.T, b []byte) {
		pc := XUDPConn(&streamConn{r: bytes.NewReader(b)}, func(addr string) (*net.UDPAddr, error) {
			return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, nil
		})

		buf := make([]byte, 2048)
		for range 4 {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	})
}

// FuzzPacketConn feeds the plain UDP framing the same way.
func FuzzPacketConn(f *testing.F) {
	f.Add([]byte{0, 4, 't', 'e', 's', 't'})
	f.Add([]byte{255, 255, 0})

	f.Fuzz(func(t *testing.T, b []byte) {
		pc := PacketConn(&streamConn{r: bytes.NewReader(b)}, &net.UDPAddr{})
		buf := make([]byte, 128)
		for range 4 {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	})
}
