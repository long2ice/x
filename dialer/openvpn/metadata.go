package openvpn

import (
	"errors"
	"os"
	"strings"
	"time"

	md "github.com/go-gost/core/metadata"
	ovpn "github.com/go-gost/x/internal/util/openvpn"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultHandshakeTimeout = 30 * time.Second
	defaultMTU              = 1500
	defaultCipher           = "AES-256-GCM"
	defaultAuth             = "SHA256"
)

type metadata struct {
	udp              bool
	cipher           string
	auth             string
	ca               []byte
	cert             []byte
	key              []byte
	tlsCrypt         []byte
	username         string
	password         string
	mtu              int
	handshakeTimeout time.Duration
}

func (d *openvpnDialer) parseMetadata(m md.Metadata) error {
	d.md.udp = mdutil.GetBool(m, "udp")

	d.md.cipher = mdutil.GetString(m, "cipher")
	if d.md.cipher == "" {
		d.md.cipher = defaultCipher
	}
	d.md.auth = mdutil.GetString(m, "auth")
	if d.md.auth == "" {
		d.md.auth = defaultAuth
	}

	var err error
	if d.md.ca, err = loadPEM(mdutil.GetString(m, "ca")); err != nil {
		return err
	}
	if len(d.md.ca) == 0 {
		return errors.New("openvpn dialer: metadata 'ca' is required")
	}
	if d.md.cert, err = loadPEM(mdutil.GetString(m, "cert")); err != nil {
		return err
	}
	if d.md.key, err = loadPEM(mdutil.GetString(m, "key")); err != nil {
		return err
	}

	tlsCryptRaw, err := loadPEM(mdutil.GetString(m, "tlsCrypt"))
	if err != nil {
		return err
	}
	if len(tlsCryptRaw) == 0 {
		return errors.New("openvpn dialer: metadata 'tlsCrypt' is required")
	}
	if d.md.tlsCrypt, err = ovpn.DecodeStaticKey(tlsCryptRaw); err != nil {
		return err
	}

	d.md.username = mdutil.GetString(m, "username")
	d.md.password = mdutil.GetString(m, "password")

	d.md.mtu = mdutil.GetInt(m, "mtu")
	if d.md.mtu <= 0 {
		d.md.mtu = defaultMTU
	}
	d.md.handshakeTimeout = mdutil.GetDuration(m, "handshakeTimeout")
	if d.md.handshakeTimeout <= 0 {
		d.md.handshakeTimeout = defaultHandshakeTimeout
	}
	return nil
}

// loadPEM accepts either an inline PEM/key block or a filesystem path.
func loadPEM(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.Contains(s, "-----BEGIN") || strings.Contains(s, "\n") {
		return []byte(s), nil
	}
	return os.ReadFile(s)
}
