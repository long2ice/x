package mieru

import (
	"bytes"
	"net"
	"testing"

	"github.com/enfein/mieru/v3/apis/model"
)

func TestParseSocks5UDPDatagram(t *testing.T) {
	t.Parallel()

	spec := model.AddrSpec{IP: net.ParseIP("127.0.0.1"), Port: 53}
	var addrBuf bytes.Buffer
	if err := spec.WriteToSocks5(&addrBuf); err != nil {
		t.Fatal(err)
	}

	raw := append([]byte{0, 0, 0}, addrBuf.Bytes()...)
	raw = append(raw, []byte("ping")...)

	dg, err := parseSocks5UDPDatagram(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(dg.Payload) != "ping" {
		t.Fatalf("payload = %q", dg.Payload)
	}
	if dg.Addr.String() != "127.0.0.1:53" {
		t.Fatalf("addr = %s", dg.Addr.String())
	}
}
