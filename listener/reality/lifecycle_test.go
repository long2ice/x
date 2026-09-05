package reality

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/go-gost/x/internal/net/accept"
	"github.com/xtls/reality"
)

func TestHandshakeCancelsBlockedDest(t *testing.T) {
	for _, mode := range []string{"read", "mirror-write", "proxy-write", "close"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			target, peer := net.Pipe()
			defer target.Close()
			defer peer.Close()
			dialed := make(chan struct{})
			finished := make(chan struct{})
			l := &realityListener{cfg: &reality.Config{
				Type: "tcp", Dest: "unused",
				DialContext: func(context.Context, string, string) (net.Conn, error) { close(dialed); return target, nil },
			}}
			if mode == "proxy-write" {
				l.cfg.Xver = 1
			}
			timeout := 150 * time.Millisecond
			if mode == "close" {
				timeout = time.Minute
			}
			ln := accept.NewListener(raw, accept.Config{Timeout: timeout, Prepare: func(ctx context.Context, c net.Conn) (net.Conn, error) {
				defer close(finished)
				return l.handshake(ctx, c)
			}})
			defer ln.Close()
			client, err := net.Dial("tcp", raw.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			select {
			case <-dialed:
			case <-time.After(time.Second):
				t.Fatal("dest not dialed")
			}
			if mode == "mirror-write" {
				client.Write([]byte{22, 3, 3, 0, 5, 1, 0, 0, 1, 0})
			}
			if mode == "close" {
				ln.Close()
			}
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("handshake outlived cancellation with blocked dest")
			}
			client.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := client.Read(make([]byte, 1)); err == nil {
				t.Fatal("client left open")
			}
			peer.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := peer.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("dest left open: %v", err)
			}
		})
	}
}

func TestHandshakeCancelsDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	l := &realityListener{cfg: &reality.Config{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }}}
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	done := make(chan struct{})
	go func() { defer close(done); l.handshake(ctx, s) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dial did not observe handshake deadline")
	}
}

func TestWaitRecordDetectionIsCancellable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := waitRecordDetection(ctx, &reality.Config{Dest: "missing", ServerNames: map[string]bool{"example.invalid": true}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
}

func TestRecordDetectionTruncatedRecord(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	key := t.Name()
	defer reality.GlobalPostHandshakeRecordsLens.Delete(key)
	d := &recordDetectConn{Conn: s, key: key, ccsSent: true}
	go func() { c.Write([]byte{23, 3, 3, 0xff, 0xff, 1}); c.Close() }()
	if _, err := d.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	value, _ := reality.GlobalPostHandshakeRecordsLens.Load(key)
	if lens, ok := value.([]int); !ok || len(lens) != 0 {
		t.Fatalf("invalid record lengths: %v", value)
	}
}

func TestProbeClosesDestOnHandshakeFailure(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()
	cfg := &reality.Config{DialContext: func(context.Context, string, string) (net.Conn, error) { return s, nil }}
	go func() { io.Copy(io.Discard, c) }()
	// Invalid server name fails the TLS setup without reading a response.
	probeRecords(cfg, "", 0, t.Name(), false)
	if _, err := c.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("probe leaked dest: %v", err)
	}
}

func TestProbeCancelsSilentDest(t *testing.T) {
	for _, ccs := range []bool{false, true} {
		s, c := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		cfg := &reality.Config{DialContext: func(context.Context, string, string) (net.Conn, error) { return s, nil }}
		done := make(chan struct{})
		go func() { defer close(done); probeRecordsContext(ctx, cfg, "example.com", 2, t.Name(), ccs) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("probe blocked on silent dest")
		}
		cancel()
		if _, err := c.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("probe leaked dest: %v", err)
		}
		s.Close()
		c.Close()
	}
}
