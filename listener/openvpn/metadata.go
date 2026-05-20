package openvpn

import (
	"errors"
	"net/netip"
	"os"
	"strings"
	"time"

	md "github.com/go-gost/core/metadata"
	ovpn "github.com/go-gost/x/internal/util/openvpn"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultBacklog          = 128
	defaultHandshakeTimeout = 30 * time.Second
	defaultIdleTimeout      = 5 * time.Minute
	defaultMTU              = 1500
	defaultCipher           = "AES-256-GCM"
	defaultAuth             = "SHA256"
	defaultServerSubnet     = "10.8.0.0/24"
)

type metadata struct {
	udp              bool
	cipher           string
	auth             string
	ca               []byte
	cert             []byte
	key              []byte
	tlsCrypt         []byte
	subnet           netip.Prefix
	mtu              int
	backlog          int
	handshakeTimeout time.Duration
	idleTimeout      time.Duration
}

func (l *openvpnListener) parseMetadata(m md.Metadata) error {
	l.md.udp = mdutil.GetBool(m, "udp")

	l.md.cipher = mdutil.GetString(m, "cipher")
	if l.md.cipher == "" {
		l.md.cipher = defaultCipher
	}
	l.md.auth = mdutil.GetString(m, "auth")
	if l.md.auth == "" {
		l.md.auth = defaultAuth
	}

	var err error
	if l.md.ca, err = loadPEM(mdutil.GetString(m, "ca")); err != nil {
		return err
	}
	if l.md.cert, err = loadPEM(mdutil.GetString(m, "cert")); err != nil {
		return err
	}
	if l.md.key, err = loadPEM(mdutil.GetString(m, "key")); err != nil {
		return err
	}
	if len(l.md.ca) == 0 || len(l.md.cert) == 0 || len(l.md.key) == 0 {
		return errors.New("openvpn listener: metadata 'ca', 'cert' and 'key' are required")
	}

	tlsCryptRaw, err := loadPEM(mdutil.GetString(m, "tlsCrypt"))
	if err != nil {
		return err
	}
	if len(tlsCryptRaw) == 0 {
		return errors.New("openvpn listener: metadata 'tlsCrypt' is required")
	}
	if l.md.tlsCrypt, err = ovpn.DecodeStaticKey(tlsCryptRaw); err != nil {
		return err
	}

	subnet := mdutil.GetString(m, "server", "subnet")
	if subnet == "" {
		subnet = defaultServerSubnet
	}
	if l.md.subnet, err = netip.ParsePrefix(subnet); err != nil {
		return err
	}
	if !l.md.subnet.Addr().Is4() {
		return errors.New("openvpn listener: 'server' subnet must be IPv4")
	}

	l.md.mtu = mdutil.GetInt(m, "mtu")
	if l.md.mtu <= 0 {
		l.md.mtu = defaultMTU
	}
	l.md.backlog = mdutil.GetInt(m, "backlog")
	if l.md.backlog <= 0 {
		l.md.backlog = defaultBacklog
	}
	l.md.handshakeTimeout = mdutil.GetDuration(m, "handshakeTimeout")
	if l.md.handshakeTimeout <= 0 {
		l.md.handshakeTimeout = defaultHandshakeTimeout
	}
	l.md.idleTimeout = mdutil.GetDuration(m, "idleTimeout")
	if l.md.idleTimeout <= 0 {
		l.md.idleTimeout = defaultIdleTimeout
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
