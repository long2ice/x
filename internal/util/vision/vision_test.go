package vision

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// tlsConn stands in for the outer TLS layer: it reads ahead into a buffer the
// way a TLS connection buffers records, and can hand the transport below it
// over to direct copy.
type tlsConn struct {
	net.Conn
	br *bufio.Reader
}

func newTLSConn(c net.Conn) *tlsConn {
	return &tlsConn{Conn: c, br: bufio.NewReaderSize(c, 4096)}
}

func (c *tlsConn) Read(b []byte) (int, error) {
	return c.br.Read(b)
}

func (c *tlsConn) RawConn() net.Conn {
	return c.Conn
}

func (c *tlsConn) TLSBuffered() io.Reader {
	buffered, _ := c.br.Peek(c.br.Buffered())
	rest := make([]byte, len(buffered))
	copy(rest, buffered)
	c.br.Discard(len(buffered))
	return bytes.NewReader(rest)
}

// serverHello is a synthetic TLS 1.3 server hello record: the length prefixed
// handshake header, a session id, a cipher suite, and the supported_versions
// extension that tells both ends the inner traffic is TLS 1.3.
func serverHello() []byte {
	body := make([]byte, 100)
	body[0] = 0x02 // handshake type: server hello
	body[43] = 0   // session id length
	body[44] = 0x13
	body[45] = 0x01 // TLS_AES_128_GCM_SHA256
	copy(body[50:], tls13SupportedVersions)

	b := []byte{0x16, 0x03, 0x03, byte(len(body) >> 8), byte(len(body))}
	return append(b, body...)
}

func appData(size int) []byte {
	body := make([]byte, size)
	rand.Read(body)
	b := []byte{0x17, 0x03, 0x03, byte(size >> 8), byte(size)}
	return append(b, body...)
}

func pipeConns(t *testing.T, uuid [16]byte, direct bool) (*Conn, *Conn) {
	t.Helper()

	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})

	t1, t2 := newTLSConn(c1), newTLSConn(c2)
	return NewConn(t1, uuid, t1, direct), NewConn(t2, uuid, t2, direct)
}

// exchange writes the payloads on one side and reads them all back on the
// other, returning what arrived.
func exchange(t *testing.T, w, r *Conn, payloads [][]byte) []byte {
	t.Helper()

	var want int
	for _, p := range payloads {
		want += len(p)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var werr error
	go func() {
		defer wg.Done()
		for _, p := range payloads {
			if _, err := w.Write(p); err != nil {
				werr = err
				return
			}
		}
	}()

	got := make([]byte, want)
	r.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := io.ReadFull(r, got)
	wg.Wait()

	if werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

func TestPaddingRoundTrip(t *testing.T) {
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	client, server := pipeConns(t, uuid, false)

	payloads := [][]byte{
		[]byte("hello"),
		serverHello(),
		appData(500),
		appData(4000),
		[]byte("after padding"),
	}

	var want []byte
	for _, p := range payloads {
		want = append(want, p...)
	}

	if got := exchange(t, client, server, payloads); !bytes.Equal(got, want) {
		t.Errorf("payload got mangled, got %d bytes, want %d", len(got), len(want))
	}

	// Padding is over on both ends by now, data keeps flowing unpadded.
	if got := exchange(t, server, client, [][]byte{[]byte("reply")}); string(got) != "reply" {
		t.Errorf("got %q", got)
	}
}

func TestDirectCopy(t *testing.T) {
	uuid := [16]byte{9: 1}
	client, server := pipeConns(t, uuid, true)

	payloads := [][]byte{
		serverHello(),
		appData(1000),
		appData(9000), // larger than a padded frame, gets split
		appData(200),
	}

	var want []byte
	for _, p := range payloads {
		want = append(want, p...)
	}

	if got := exchange(t, client, server, payloads); !bytes.Equal(got, want) {
		t.Fatalf("payload got mangled, got %d bytes, want %d", len(got), len(want))
	}

	if !client.enableDirect {
		t.Error("client did not detect TLS 1.3")
	}
	if !server.directRead.Load() {
		t.Error("server did not switch to direct copy")
	}

	// Everything after the switch goes over the raw transport.
	big := appData(20000)
	if got := exchange(t, client, server, [][]byte{big}); !bytes.Equal(got, big) {
		t.Error("direct copy payload got mangled")
	}
}

func TestNoPaddingFromPeer(t *testing.T) {
	// A peer that does not pad at all is passed through untouched.
	uuid := [16]byte{1}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	conn := NewConn(newTLSConn(c2), uuid, nil, false)
	go func() {
		c1.Write([]byte("plain data, no padding here"))
	}()

	b := make([]byte, 27)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "plain data, no padding here" {
		t.Errorf("got %q", b)
	}
}

func TestIsCompleteRecord(t *testing.T) {
	if !isCompleteRecord(appData(10)) {
		t.Error("a whole record was not recognized")
	}
	if isCompleteRecord(appData(10)[:8]) {
		t.Error("a partial record was recognized")
	}
	if !isCompleteRecord(append(appData(10), appData(20)...)) {
		t.Error("two whole records were not recognized")
	}
	if isCompleteRecord(append(appData(10), 0x17, 0x03)) {
		t.Error("a trailing partial header was recognized")
	}
	if isCompleteRecord(serverHello()) {
		t.Error("a handshake record was taken for application data")
	}
}

// TestConcurrentReadWrite guards against a race on the state both directions
// share, the way a relay uses the connection.
func TestConcurrentReadWrite(t *testing.T) {
	uuid := [16]byte{3: 7}
	client, server := pipeConns(t, uuid, true)

	var sends, recvs sync.WaitGroup
	sends.Add(2)
	recvs.Add(2)

	send := func(c *Conn) {
		defer sends.Done()
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		c.Write(serverHello())
		for range 20 {
			if _, err := c.Write(appData(1000)); err != nil {
				return
			}
		}
	}
	recv := func(c *Conn) {
		defer recvs.Done()
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		io.Copy(io.Discard, c)
	}

	go send(client)
	go send(server)
	go recv(client)
	go recv(server)

	sends.Wait()
	client.Close()
	server.Close()
	recvs.Wait()
}
