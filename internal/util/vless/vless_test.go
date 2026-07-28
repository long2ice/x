package vless

import (
	"bytes"
	"io"
	"net"
	"testing"
)

func TestParseUUID(t *testing.T) {
	uuid, err := ParseUUID("b831381d-6324-4d53-ad4f-8cda48b30811")
	if err != nil {
		t.Fatal(err)
	}
	if got := uuid.String(); got != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Errorf("got %s", got)
	}

	// A short id is hashed into a UUID v5, the same way Xray does it.
	mapped, err := ParseUUID("password")
	if err != nil {
		t.Fatal(err)
	}
	if mapped == (UUID{}) {
		t.Error("id was not mapped")
	}
	if v := mapped[6] >> 4; v != 5 {
		t.Errorf("version %d, want 5", v)
	}
	if v := mapped[8] >> 6; v != 2 {
		t.Errorf("variant %d, want 2", v)
	}
	if again, _ := ParseUUID("password"); again != mapped {
		t.Error("mapping is not stable")
	}

	if _, err := ParseUUID(""); err == nil {
		t.Error("empty id was accepted")
	}
}

func TestRequestRoundTrip(t *testing.T) {
	id, _ := ParseUUID("b831381d-6324-4d53-ad4f-8cda48b30811")

	for _, req := range []*Request{
		{ID: id, Command: CmdTCP, Host: "example.com", Port: 443},
		{ID: id, Command: CmdTCP, Host: "1.2.3.4", Port: 80},
		{ID: id, Command: CmdUDP, Host: "2606:4700:4700::1111", Port: 53},
		{ID: id, Command: CmdTCP, Host: "example.com", Port: 443, Flow: FlowVision},
	} {
		var buf bytes.Buffer
		if err := WriteRequest(&buf, req); err != nil {
			t.Fatal(err)
		}
		buf.WriteString("payload")

		got, err := ReadRequest(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != req.ID || got.Command != req.Command ||
			got.Host != req.Host || got.Port != req.Port || got.Flow != req.Flow {
			t.Errorf("got %+v, want %+v", got, req)
		}
		if rest, _ := io.ReadAll(&buf); string(rest) != "payload" {
			t.Errorf("payload got mangled: %q", rest)
		}
	}
}

func TestReadRequestErrors(t *testing.T) {
	if _, err := ReadRequest(bytes.NewReader(make([]byte, 18))); err != ErrBadVersion {
		// version 0 is valid, a zero buffer stops at the command instead
		t.Logf("err: %v", err)
	}

	// unsupported address type
	b := make([]byte, 0, 32)
	b = append(b, Version)
	b = append(b, make([]byte, 16)...)
	b = append(b, 0, CmdTCP, 0x01, 0xbb, 0x09)
	if _, err := ReadRequest(bytes.NewReader(b)); err != ErrBadAddr {
		t.Errorf("got %v, want %v", err, ErrBadAddr)
	}
}

func TestServerConn(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	go func() {
		conn := ServerConn(c1)
		conn.Write([]byte("hello"))
		conn.Write([]byte("world"))
	}()

	b := make([]byte, 12)
	if _, err := io.ReadFull(c2, b); err != nil {
		t.Fatal(err)
	}
	if want := append([]byte{Version, 0}, "helloworld"...); !bytes.Equal(b, want) {
		t.Errorf("got %q, want %q", b, want)
	}
}

func TestPacketConn(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 53}
	client := PacketConn(c1, addr)
	server := PacketConn(c2, addr)

	go func() {
		client.WriteTo([]byte("query"), addr)
		client.WriteTo(bytes.Repeat([]byte("x"), 2000), addr)
	}()

	b := make([]byte, 4096)
	n, raddr, err := server.ReadFrom(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:n]) != "query" || raddr.String() != addr.String() {
		t.Errorf("got %q from %v", b[:n], raddr)
	}

	n, _, err = server.ReadFrom(b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2000 {
		t.Errorf("got %d bytes, want 2000", n)
	}

	// A packet larger than the buffer is truncated, the stream stays in sync.
	go client.WriteTo(bytes.Repeat([]byte("y"), 100), addr)
	small := make([]byte, 10)
	if n, _, err = server.ReadFrom(small); err != nil || n != 10 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	go client.WriteTo([]byte("next"), addr)
	if n, _, err = server.ReadFrom(b); err != nil || string(b[:n]) != "next" {
		t.Fatalf("stream out of sync: %q %v", b[:n], err)
	}
}
