package reality

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/go-gost/core/logger"
	"github.com/xtls/reality"
)

type tempError struct{}

func (tempError) Error() string   { return "temporary accept error" }
func (tempError) Timeout() bool   { return false }
func (tempError) Temporary() bool { return true }

// fakeListener hands out queued Accept results, then blocks until closed.
type fakeListener struct {
	results chan any // net.Conn or error
	done    chan struct{}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.results:
		if err, ok := r.(error); ok {
			return nil, err
		}
		return r.(net.Conn), nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

// TestAcceptLoopSurvivesTemporaryError reproduces the accept loop of
// reality.NewListener going down for good on the first transient error: the
// socket stayed open, the kernel kept filling the backlog and the port went
// dark until a config reload. The loop must instead keep accepting, and a
// connection whose handshake fails must be closed, not leaked in CLOSE-WAIT.
func TestAcceptLoopSurvivesTemporaryError(t *testing.T) {
	privateKey, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	private, err := decodeKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}

	fl := &fakeListener{results: make(chan any, 2), done: make(chan struct{})}
	l := &realityListener{
		ln: fl,
		cfg: &reality.Config{
			Dest:        "127.0.0.1:1", // nothing listens there, fallback dial fails fast
			Type:        "tcp",
			PrivateKey:  private,
			ServerNames: map[string]bool{"example.com": true},
			ShortIds:    map[[8]byte]bool{{}: true},
			DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
		},
		conns:  make(chan net.Conn, 128),
		done:   make(chan struct{}),
		logger: logger.Default(),
	}
	defer l.Close()
	defer fl.Close()

	server, client := net.Pipe()
	fl.results <- tempError{}
	fl.results <- server

	go l.acceptLoop()

	// The connection is only ever accepted if the loop outlived the
	// temporary error. Its bogus handshake must end with the conn closed,
	// which the client observes as its read failing instead of hanging.
	client.SetDeadline(time.Now().Add(10 * time.Second))
	client.Write([]byte("bogus client hello")) // may already be closed
	if _, err := io.ReadAll(client); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the connection of the failed handshake was leaked instead of closed")
	}

	// Closing must end Accept with net.ErrClosed.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fl.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed from Accept, got %v", err)
	}
}

// TestHandshakeSlotsAreBoundedPerIP checks that one source address cannot
// occupy more than its share of concurrent handshakes, which is what let a
// single reconnecting client fill a port's accept queue and lock everyone
// else out for minutes.
func TestHandshakeSlotsAreBoundedPerIP(t *testing.T) {
	l := &realityListener{
		inflight: make(map[string]int),
		md:       metadata{maxHandshakesPerIP: 2},
	}

	if !l.acquire("1.2.3.4") || !l.acquire("1.2.3.4") {
		t.Fatal("the first two handshakes should get a slot")
	}
	if l.acquire("1.2.3.4") {
		t.Fatal("the third handshake from the same address should be refused")
	}
	// A different client must not be affected by a noisy neighbour.
	if !l.acquire("5.6.7.8") {
		t.Fatal("another address should still get a slot")
	}

	l.release("1.2.3.4")
	if !l.acquire("1.2.3.4") {
		t.Fatal("a released slot should be reusable")
	}

	l.release("1.2.3.4")
	l.release("1.2.3.4")
	l.release("5.6.7.8")
	if len(l.inflight) != 0 {
		t.Fatalf("released slots should not be retained: %v", l.inflight)
	}
}
