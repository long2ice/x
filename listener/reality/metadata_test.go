package reality

import (
	"encoding/base64"
	"testing"
	"time"

	xmd "github.com/go-gost/x/metadata"
)

func TestGenerateKeyPair(t *testing.T) {
	private, public, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	key, err := decodeKey(private)
	if err != nil {
		t.Fatalf("the private key is not accepted by the listener: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key is %d bytes", len(key))
	}
	if b, err := base64.RawURLEncoding.DecodeString(public); err != nil || len(b) != 32 {
		t.Errorf("public key %q: %v", public, err)
	}
	if private == public {
		t.Error("the keys are the same")
	}
}

func TestParseMetadata(t *testing.T) {
	private, _, _ := GenerateKeyPair()

	l := &realityListener{}
	err := l.parseMetadata(xmd.NewMetadata(map[string]any{
		"privateKey":   private,
		"dest":         "www.apple.com:443",
		"serverNames":  "www.apple.com, apple.com",
		"shortIds":     "0123456789abcdef, ab",
		"minClientVer": "1.8.0",
		"maxTimeDiff":  "30s",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if got := l.md.serverNames; len(got) != 2 || got[1] != "apple.com" {
		t.Errorf("serverNames: %v", got)
	}
	// A short id is padded with zeros to its full width.
	if got := l.md.shortIDs; len(got) != 2 ||
		got[0] != [8]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef} ||
		got[1] != [8]byte{0xab} {
		t.Errorf("shortIds: %v", got)
	}
	if got := l.md.minClientVer; len(got) != 3 || got[0] != 1 || got[1] != 8 {
		t.Errorf("minClientVer: %v", got)
	}
	if l.md.maxTimeDiff != 30*time.Second {
		t.Errorf("maxTimeDiff: %v", l.md.maxTimeDiff)
	}

	// The dest defaults to the first server name, and the server names to the
	// host of the dest.
	l = &realityListener{}
	if err := l.parseMetadata(xmd.NewMetadata(map[string]any{
		"privateKey":  private,
		"serverNames": "www.apple.com",
	})); err != nil {
		t.Fatal(err)
	}
	if l.md.dest != "www.apple.com:443" {
		t.Errorf("dest: %s", l.md.dest)
	}

	l = &realityListener{}
	if err := l.parseMetadata(xmd.NewMetadata(map[string]any{
		"privateKey": private,
		"dest":       "www.apple.com",
	})); err != nil {
		t.Fatal(err)
	}
	if l.md.dest != "www.apple.com:443" || len(l.md.serverNames) != 1 || l.md.serverNames[0] != "www.apple.com" {
		t.Errorf("dest %s, serverNames %v", l.md.dest, l.md.serverNames)
	}

	// An empty short id list still admits the clients that send none.
	if len(l.md.shortIDs) != 1 || l.md.shortIDs[0] != [8]byte{} {
		t.Errorf("shortIds: %v", l.md.shortIDs)
	}
}

func TestParseMetadataErrors(t *testing.T) {
	for _, md := range []map[string]any{
		{"dest": "www.apple.com:443"},                                 // no key
		{"privateKey": "not-a-key", "dest": "www.apple.com:443"},      // bad key
		{"privateKey": "x", "serverNames": "a.com", "shortIds": "zz"}, // bad short id
	} {
		l := &realityListener{}
		if err := l.parseMetadata(xmd.NewMetadata(md)); err == nil {
			t.Errorf("%v was accepted", md)
		}
	}
}

func TestVisionSupported(t *testing.T) {
	// XTLS Vision needs the private buffers of the reality package, a version
	// that moved them would silently disable the flow.
	if !visionSupported() {
		t.Error("the TLS buffers of the reality package could not be located")
	}
}
