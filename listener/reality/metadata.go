package reality

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	mdata "github.com/go-gost/core/metadata"
	mdutil "github.com/go-gost/x/metadata/util"
)

type metadata struct {
	mptcp bool
	show  bool

	dest        string
	typ         string
	xver        byte
	serverNames []string

	privateKey   []byte
	shortIDs     [][8]byte
	minClientVer []byte
	maxClientVer []byte
	maxTimeDiff  time.Duration

	dialTimeout      time.Duration
	handshakeTimeout time.Duration
}

func (l *realityListener) parseMetadata(md mdata.Metadata) (err error) {
	l.md.mptcp = mdutil.GetBool(md, "mptcp")
	l.md.show = mdutil.GetBool(md, "reality.show", "show")

	l.md.typ = mdutil.GetString(md, "reality.type", "type")
	if l.md.typ == "" {
		l.md.typ = "tcp"
	}

	l.md.serverNames = getStrings(md, "reality.serverNames", "serverNames", "reality.serverName", "serverName")
	l.md.dest = mdutil.GetString(md, "reality.dest", "dest", "reality.target", "target")

	if l.md.dest == "" {
		if len(l.md.serverNames) == 0 {
			return errors.New("reality: dest or serverNames is required")
		}
		l.md.dest = net.JoinHostPort(l.md.serverNames[0], "443")
	}
	if _, _, err := net.SplitHostPort(l.md.dest); err != nil {
		l.md.dest = net.JoinHostPort(l.md.dest, "443")
	}
	if len(l.md.serverNames) == 0 {
		host, _, _ := net.SplitHostPort(l.md.dest)
		l.md.serverNames = []string{host}
	}

	l.md.xver = byte(mdutil.GetInt(md, "reality.xver", "xver"))

	key := mdutil.GetString(md, "reality.privateKey", "privateKey")
	if key == "" {
		return errors.New("reality: privateKey is required")
	}
	if l.md.privateKey, err = decodeKey(key); err != nil {
		return err
	}

	for _, s := range getStrings(md, "reality.shortIds", "shortIds", "reality.shortId", "shortId") {
		var id [8]byte
		if len(s) > 16 {
			return fmt.Errorf("reality: shortId %q is too long", s)
		}
		if _, err := hex.Decode(id[:], []byte(s+strings.Repeat("0", 16-len(s)))); err != nil {
			return fmt.Errorf("reality: invalid shortId %q: %w", s, err)
		}
		l.md.shortIDs = append(l.md.shortIDs, id)
	}
	if len(l.md.shortIDs) == 0 {
		// An empty shortId is the default of the clients that do not set one.
		l.md.shortIDs = append(l.md.shortIDs, [8]byte{})
	}

	if l.md.minClientVer, err = parseVersion(mdutil.GetString(md, "reality.minClientVer", "minClientVer")); err != nil {
		return err
	}
	if l.md.maxClientVer, err = parseVersion(mdutil.GetString(md, "reality.maxClientVer", "maxClientVer")); err != nil {
		return err
	}
	l.md.maxTimeDiff = mdutil.GetDuration(md, "reality.maxTimeDiff", "maxTimeDiff")

	l.md.dialTimeout = mdutil.GetDuration(md, "reality.dialTimeout", "dialTimeout")
	if l.md.dialTimeout <= 0 {
		l.md.dialTimeout = 10 * time.Second
	}

	// A whole-handshake deadline. REALITY reads the client's ClientHello with
	// no deadline of its own, so a client that opens a connection and then
	// sends nothing (or dribbles) parks a handshake goroutine forever. One
	// misbehaving client opening thousands of such connections exhausts the
	// port and starves real clients. Bound the handshake so those go away.
	l.md.handshakeTimeout = mdutil.GetDuration(md, "reality.handshakeTimeout", "handshakeTimeout")
	if l.md.handshakeTimeout <= 0 {
		l.md.handshakeTimeout = 15 * time.Second
	}

	return nil
}

// getStrings reads a list that may also be given as a comma separated string.
func getStrings(md mdata.Metadata, keys ...string) []string {
	if ss := mdutil.GetStrings(md, keys...); len(ss) > 0 {
		return ss
	}

	var ss []string
	for _, s := range strings.Split(mdutil.GetString(md, keys...), ",") {
		if s = strings.TrimSpace(s); s != "" {
			ss = append(ss, s)
		}
	}
	return ss
}

// decodeKey decodes a X25519 key in the base64 form used by `xray x25519`,
// also accepting standard base64 and hex.
func decodeKey(s string) ([]byte, error) {
	for _, dec := range []func(string) ([]byte, error){
		base64.RawURLEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, fmt.Errorf("reality: invalid key %q, expect a 32 bytes X25519 key", s)
}

// parseVersion converts a dotted version such as 1.8.0 into its byte form.
func parseVersion(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}

	var v []byte
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return nil, fmt.Errorf("reality: invalid version %q", s)
		}
		v = append(v, byte(n))
	}
	return v, nil
}
