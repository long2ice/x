package reality

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/pires/go-proxyproto"
	utls "github.com/refraction-networking/utls"
	"github.com/xtls/reality"
)

const recordProbeTimeout = 10 * time.Second

var probeSlots = make(chan struct{}, 32)

// Detection is shared by dest/SNI/ALPN, not by listener. Every probe has a
// deadline and closes its socket on EVERY exit. The upstream detector uses
// unbounded net.Dial/Handshake and leaves sockets open on several error paths.
func startRecordDetection(cfg *reality.Config) {
	for sni := range cfg.ServerNames {
		for alpn := range 3 {
			key := cfg.Dest + " " + sni + " " + strconv.Itoa(alpn)
			if _, loaded := reality.GlobalPostHandshakeRecordsLens.LoadOrStore(key, false); loaded {
				continue
			}
			go func() {
				defer func() {
					if v, _ := reality.GlobalPostHandshakeRecordsLens.Load(key); v == false {
						reality.GlobalPostHandshakeRecordsLens.Store(key, []int{})
					}
				}()
				probeRecords(cfg, sni, alpn, key, false)
			}()
			go probeRecords(cfg, sni, alpn, key, true)
		}
	}
}

func waitRecordDetection(ctx context.Context, cfg *reality.Config) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready := true
		for sni := range cfg.ServerNames {
			for alpn := range 3 {
				v, _ := reality.GlobalPostHandshakeRecordsLens.Load(cfg.Dest + " " + sni + " " + strconv.Itoa(alpn))
				if _, ok := v.([]int); !ok {
					ready = false
				}
			}
		}
		if ready {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func probeRecords(cfg *reality.Config, sni string, alpn int, key string, ccs bool) {
	ctx, cancel := context.WithTimeout(context.Background(), recordProbeTimeout)
	defer cancel()
	probeRecordsContext(ctx, cfg, sni, alpn, key, ccs)
}

func probeRecordsContext(ctx context.Context, cfg *reality.Config, sni string, alpn int, key string, ccs bool) {
	select {
	case probeSlots <- struct{}{}:
		defer func() { <-probeSlots }()
	case <-ctx.Done():
		return
	}
	target, err := cfg.DialContext(ctx, cfg.Type, cfg.Dest)
	if err != nil {
		return
	}
	defer target.Close()
	stop := context.AfterFunc(ctx, func() { target.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err = target.SetDeadline(deadline); err != nil {
			return
		}
	}
	if cfg.Xver == 1 || cfg.Xver == 2 {
		if _, err = proxyproto.HeaderProxyFromAddrs(cfg.Xver, target.LocalAddr(), target.RemoteAddr()).WriteTo(target); err != nil {
			return
		}
	}
	var conn net.Conn = &recordDetectConn{Conn: target, key: key}
	if ccs {
		conn = &ccsDetectConn{Conn: target, ctx: ctx, key: key}
	}
	fingerprint := utls.HelloChrome_Auto
	protos := []string{"h2", "http/1.1"}
	if alpn != 2 {
		fingerprint = utls.HelloGolang
	}
	if alpn == 1 {
		protos = []string{"http/1.1"}
	}
	if alpn == 0 {
		protos = nil
	}
	uconn := utls.UClient(conn, &utls.Config{ServerName: sni, NextProtos: protos}, fingerprint)
	if err = uconn.HandshakeContext(ctx); err != nil {
		return
	}
	if !ccs {
		io.Copy(io.Discard, uconn)
	}
}

type recordDetectConn struct {
	net.Conn
	key     string
	ccsSent bool
}

func (c *recordDetectConn) Write(b []byte) (int, error) {
	if len(b) >= 3 && bytes.Equal(b[:3], []byte{20, 3, 3}) {
		c.ccsSent = true
	}
	return c.Conn.Write(b)
}

func (c *recordDetectConn) Read(b []byte) (int, error) {
	if !c.ccsSent {
		return c.Conn.Read(b)
	}
	// Preserve the absolute probe deadline: do not extend it with each Read.
	// Bound both collection size and record lengths for an untrusted dest.
	data, _ := io.ReadAll(io.LimitReader(c.Conn, 64*1024))
	lengths := []int{}
	for len(data) >= 5 && bytes.Equal(data[:3], []byte{23, 3, 3}) {
		n := int(binary.BigEndian.Uint16(data[3:5])) + 5
		if n > len(data) || n < 17 {
			break
		}
		lengths = append(lengths, n)
		data = data[n:]
	}
	reality.GlobalPostHandshakeRecordsLens.Store(c.key, lengths)
	return 0, io.EOF
}

type ccsDetectConn struct {
	net.Conn
	ctx context.Context
	key string
}

func (c *ccsDetectConn) Write(b []byte) (int, error) {
	if len(b) < 3 || !bytes.Equal(b[:3], []byte{20, 3, 3}) {
		return c.Conn.Write(b)
	}
	alert := make(chan struct{})
	go func() {
		defer close(alert)
		buf := make([]byte, 512)
		for {
			n, err := c.Conn.Read(buf)
			if err != nil || n > 0 && buf[0] == 0x15 {
				return
			}
		}
	}()
	for i, count := range []int{2, 15, 16} {
		if _, err := c.Conn.Write(bytes.Repeat(reality.CCSMsg, count)); err != nil {
			return 0, err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return 0, c.ctx.Err()
		case <-alert:
			timer.Stop()
			reality.GlobalMaxCSSMsgCount.Store(c.key, []int{1, 16, 32}[i])
			return c.Conn.Write(b)
		case <-timer.C:
		}
	}
	reality.GlobalMaxCSSMsgCount.Store(c.key, math.MaxInt)
	return c.Conn.Write(b)
}
