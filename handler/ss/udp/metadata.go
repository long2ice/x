package ss

import (
	"time"

	mdata "github.com/go-gost/core/metadata"
	"github.com/go-gost/go-shadowsocks2/core"
	mdutil "github.com/go-gost/x/metadata/util"
)

const (
	defaultBufferSize  = 4096
	defaultUDPConnTTL  = 30 * time.Second
)

type metadata struct {
	key           string
	readTimeout   time.Duration
	udpConnTTL    time.Duration
	udpBufferSize int

	users []core.UserConfig
}

func (h *ssuHandler) parseMetadata(md mdata.Metadata) (err error) {

	h.md.key = mdutil.GetString(md, "key")
	h.md.readTimeout = mdutil.GetDuration(md, "readTimeout")
	h.md.udpConnTTL = mdutil.GetDuration(md, "udpConnTTL", "udp.connTTL")
	if h.md.udpConnTTL <= 0 {
		h.md.udpConnTTL = defaultUDPConnTTL
	}
	h.md.udpBufferSize = mdutil.GetInt(md, "udpBufferSize", "udp.bufferSize")

	usersMap := mdutil.GetStringMapString(md, "users")
	for name, pass := range usersMap {
		h.md.users = append(h.md.users, core.UserConfig{
			Name:     name,
			Password: pass,
		})
	}

	return
}
