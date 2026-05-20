package openvpn

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Control messages exchanged inside the TLS session are NUL-terminated
// ASCII. PUSH_REQUEST is sent by the client; the server answers with a
// comma-separated PUSH_REPLY of pushed options.
const (
	PushRequest = "PUSH_REQUEST"
	pushReply   = "PUSH_REPLY"
)

// PushReply is the parsed result of a server PUSH_REPLY.
type PushReply struct {
	Raw      string
	Prefixes []netip.Prefix // assigned tunnel addresses (ifconfig)
	DNS      []netip.Addr
	PeerID   uint32
	Cipher   string
	TunMTU   int
	Redirect bool
}

// ParsePushReply decodes a PUSH_REPLY control message.
func ParsePushReply(message string) (*PushReply, error) {
	message = strings.TrimRight(message, "\x00")
	if !strings.HasPrefix(message, pushReply) {
		return nil, fmt.Errorf("openvpn: unexpected push message %q", message)
	}
	reply := &PushReply{Raw: message, PeerID: PeerIDUnset}
	for _, opt := range splitPushOptions(message) {
		fields := strings.Fields(opt)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "ifconfig":
			if len(fields) >= 3 {
				prefix, err := parseIPv4Ifconfig(fields[1], fields[2])
				if err != nil {
					return nil, err
				}
				reply.Prefixes = append(reply.Prefixes, prefix)
			}
		case "ifconfig-ipv6":
			if len(fields) >= 2 {
				prefix, err := netip.ParsePrefix(fields[1])
				if err != nil {
					return nil, fmt.Errorf("openvpn: parse pushed ipv6 %q: %w", fields[1], err)
				}
				reply.Prefixes = append(reply.Prefixes, prefix)
			}
		case "dhcp-option":
			if len(fields) >= 3 && fields[1] == "DNS" {
				if addr, err := netip.ParseAddr(fields[2]); err == nil {
					reply.DNS = append(reply.DNS, addr)
				}
			}
		case "peer-id":
			if len(fields) >= 2 {
				id, err := strconv.ParseUint(fields[1], 10, 24)
				if err != nil {
					return nil, fmt.Errorf("openvpn: parse pushed peer-id %q: %w", fields[1], err)
				}
				reply.PeerID = uint32(id)
			}
		case "cipher":
			if len(fields) >= 2 {
				reply.Cipher = fields[1]
			}
		case "tun-mtu":
			if len(fields) >= 2 {
				reply.TunMTU, _ = strconv.Atoi(fields[1])
			}
		case "redirect-gateway":
			reply.Redirect = true
		}
	}
	if len(reply.Prefixes) == 0 {
		return nil, fmt.Errorf("openvpn: push reply missing ifconfig address")
	}
	return reply, nil
}

// PushConfig is the server-side description of what to push to a client.
type PushConfig struct {
	ClientIP  netip.Addr // address assigned to the client
	Netmask   netip.Addr // tunnel netmask, e.g. 255.255.255.0
	Gateway   netip.Addr // route-gateway (the server's tunnel IP)
	PeerID    uint32
	Cipher    string
	TunMTU    int
	PingEvery int // keepalive ping interval, seconds
	PingExpit int // keepalive restart timeout, seconds
}

// Build renders the PUSH_REPLY control message (NUL-terminated).
func (c PushConfig) Build() string {
	var b strings.Builder
	b.WriteString(pushReply)
	fmt.Fprintf(&b, ",route-gateway %s", c.Gateway)
	b.WriteString(",topology subnet")
	if c.PingEvery > 0 {
		fmt.Fprintf(&b, ",ping %d", c.PingEvery)
	}
	if c.PingExpit > 0 {
		fmt.Fprintf(&b, ",ping-restart %d", c.PingExpit)
	}
	fmt.Fprintf(&b, ",ifconfig %s %s", c.ClientIP, c.Netmask)
	fmt.Fprintf(&b, ",peer-id %d", c.PeerID)
	fmt.Fprintf(&b, ",cipher %s", c.Cipher)
	if c.TunMTU > 0 {
		fmt.Fprintf(&b, ",tun-mtu %d", c.TunMTU)
	}
	// We do not implement TLS renegotiation; tell the client never to
	// trigger it so long-lived connections stay up.
	b.WriteString(",reneg-sec 0")
	b.WriteByte(0)
	return b.String()
}

func splitPushOptions(message string) []string {
	message = strings.TrimRight(message, "\x00")
	parts := strings.Split(message, ",")
	if len(parts) > 0 && parts[0] == pushReply {
		parts = parts[1:]
	}
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseIPv4Ifconfig(address, mask string) (netip.Prefix, error) {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("openvpn: parse pushed ipv4 %q: %w", address, err)
	}
	maskAddr, err := netip.ParseAddr(mask)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("openvpn: parse pushed netmask %q: %w", mask, err)
	}
	if !addr.Is4() || !maskAddr.Is4() {
		return netip.Prefix{}, fmt.Errorf("openvpn: ifconfig requires ipv4 address and mask")
	}
	ones := 0
	for _, b := range maskAddr.As4() {
		for i := 7; i >= 0; i-- {
			if b&(1<<i) == 0 {
				return netip.PrefixFrom(addr, ones), nil
			}
			ones++
		}
	}
	return netip.PrefixFrom(addr, ones), nil
}

// occOptionsString builds the OCC ("options consistency check") string
// sent in the key-method-2 record. The peer compares a subset of it.
func occOptionsString(proto, cipher, auth string, isServer bool) string {
	protoName := "UDPv4"
	role := "tls-client"
	if proto == "tcp" {
		protoName = "TCPv4_CLIENT"
		if isServer {
			protoName = "TCPv4_SERVER"
		}
	}
	if isServer {
		role = "tls-server"
	}
	keysize := "128"
	if cipher == "AES-256-GCM" {
		keysize = "256"
	}
	return fmt.Sprintf("V4,dev-type tun,link-mtu 1550,tun-mtu 1500,proto %s,cipher %s,auth %s,keysize %s,key-method 2,%s",
		protoName, cipher, auth, keysize, role)
}

// peerInfoString builds the IV_* peer-info block. IV_PROTO is kept low
// (DATA_V2 | REQUEST_PUSH) so a 2.6+ peer falls back to the legacy key
// path instead of tls-ekm / aead-epoch.
func peerInfoString(cipher string) string {
	const ivProtoDataV2RequestPush = 6
	return fmt.Sprintf("IV_VER=2.6.0\nIV_PROTO=%d\nIV_NCP=2\nIV_CIPHERS=%s\nIV_TCPNL=1\n",
		ivProtoDataV2RequestPush, cipher)
}
