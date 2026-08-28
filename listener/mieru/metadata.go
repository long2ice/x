package mieru

import (
	"fmt"
	"strings"

	mdata "github.com/go-gost/core/metadata"
	"github.com/enfein/mieru/v3/pkg/common"
	mdutil "github.com/go-gost/x/metadata/util"
)

type metadata struct {
	users                 map[string]string
	mtu                   int
	protocol              string
	userHintIsMandatory   bool
}

func (l *mieruListener) parseMetadata(md mdata.Metadata) error {
	l.md.users = mdutil.GetStringMapString(md, "users")
	if user := mdutil.GetString(md, "user", "username", "name"); user != "" {
		if l.md.users == nil {
			l.md.users = make(map[string]string)
		}
		l.md.users[user] = mdutil.GetString(md, "password", "pass")
	}

	l.md.mtu = mdutil.GetInt(md, "mtu")
	if l.md.mtu <= 0 {
		l.md.mtu = common.DefaultMTU
	}

	l.md.protocol = strings.ToLower(mdutil.GetString(md, "protocol"))
	if l.md.protocol == "" {
		l.md.protocol = "tcp"
	}
	switch l.md.protocol {
	case "tcp", "udp", "both", "tcp,udp", "tcp+udp", "tcpudp":
	default:
		return fmt.Errorf("mieru: unsupported protocol %q", l.md.protocol)
	}

	l.md.userHintIsMandatory = mdutil.GetBool(md, "userHintIsMandatory", "userHint.mandatory")
	return nil
}
