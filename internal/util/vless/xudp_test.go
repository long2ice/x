package vless

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// muxFrame builds a frame the way an XUDP client does.
func muxFrame(sessionID uint16, status byte, host string, port int, data []byte) []byte {
	meta := binary.BigEndian.AppendUint16(nil, sessionID)
	meta = append(meta, status, muxOptionData)
	if host != "" {
		meta = append(meta, muxNetworkUDP)
		meta, _ = appendAddrPort(meta, host, port)
	}

	b := binary.BigEndian.AppendUint16(nil, uint16(len(meta)))
	b = append(b, meta...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

func TestXUDPReadFrom(t *testing.T) {
	var stream bytes.Buffer
	// A new session, then a packet for the same destination, then one for
	// another destination, the way a cone NAT client sends them.
	stream.Write(muxFrame(1, muxStatusNew, "1.2.3.4", 53, []byte("first")))
	stream.Write(muxFrame(1, muxStatusKeep, "", 0, []byte("second")))
	stream.Write(muxFrame(1, muxStatusKeep, "5.6.7.8", 443, []byte("third")))

	pc := XUDPConn(&streamConn{r: &stream}, nil)

	b := make([]byte, 1024)
	for _, want := range []struct {
		data string
		addr string
	}{
		{"first", "1.2.3.4:53"},
		{"second", "1.2.3.4:53"},
		{"third", "5.6.7.8:443"},
	} {
		n, addr, err := pc.ReadFrom(b)
		if err != nil {
			t.Fatal(err)
		}
		if string(b[:n]) != want.data || addr.String() != want.addr {
			t.Errorf("got %q from %v, want %q from %s", b[:n], addr, want.data, want.addr)
		}
	}

	if _, _, err := pc.ReadFrom(b); err != io.EOF {
		t.Errorf("got %v, want EOF", err)
	}
}

func TestXUDPWriteTo(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// A domain destination is resolved for the relay, but the reply has to be
	// reported back under the name the client used, or a cone NAT drops it.
	resolve := func(string) (*net.UDPAddr, error) {
		return &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 53}, nil
	}
	pc := XUDPConn(c2, resolve)

	go func() {
		c1.Write(muxFrame(0x1234, muxStatusNew, "dns.example.com", 53, []byte("query")))
	}()

	b := make([]byte, 1024)
	_, addr, err := pc.ReadFrom(b)
	if err != nil {
		t.Fatal(err)
	}

	go pc.WriteTo([]byte("answer"), addr)

	frame := make([]byte, 64)
	c1.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := c1.Read(frame)
	if err != nil {
		t.Fatal(err)
	}
	frame = frame[:n]

	metaLen := int(binary.BigEndian.Uint16(frame[:2]))
	meta := frame[2 : 2+metaLen]
	if id := binary.BigEndian.Uint16(meta[:2]); id != 0x1234 {
		t.Errorf("session id %#x, want 0x1234", id)
	}
	if meta[2] != muxStatusKeep {
		t.Errorf("status %d, want keep", meta[2])
	}
	if got, _, err := readAddrPort(meta[5:]); err != nil || got != "dns.example.com:53" {
		t.Errorf("reply source %q (%v), want the name the client asked for", got, err)
	}
	if data := frame[2+metaLen:]; string(data[2:]) != "answer" {
		t.Errorf("data %q", data[2:])
	}
}

// TestXUDPConcurrent drives the connection the way the relay does, one
// goroutine reading packets while another writes the replies.
func TestXUDPConcurrent(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	pc := XUDPConn(c2, func(string) (*net.UDPAddr, error) {
		return &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 53}, nil
	})

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		defer c1.Close()
		c1.SetWriteDeadline(time.Now().Add(10 * time.Second))
		c1.Write(muxFrame(7, muxStatusNew, "1.2.3.4", 53, []byte("q")))
		for i := range 50 {
			if _, err := c1.Write(muxFrame(uint16(i), muxStatusKeep, "", 0, []byte("q"))); err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		b := make([]byte, 1024)
		for {
			if _, _, err := pc.ReadFrom(b); err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 53}
		for range 50 {
			if _, err := pc.WriteTo([]byte("a"), addr); err != nil {
				return
			}
		}
	}()

	// The peer never reads the replies, so drain them.
	go io.Copy(io.Discard, c1)

	wg.Wait()
}
