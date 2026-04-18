package relayx

import (
	"strings"
	"sync"
	"time"
)

// sessionKey identifies a "dialer config equivalence class".  Two relayxDialer
// instances with identical keys can safely share the same underlying TCP and
// smux session, so thousands of services pointed at the same upstream collapse
// into one TCP connection.  All fields are comparable so this works directly
// as a map key.
type sessionKey struct {
	addr              string
	mdKey             string
	host              string
	serverName        string
	insecureSkip      bool
	nextProtos        string
	muxVersion        int
	muxKAInterval     time.Duration
	muxKADisabled     bool
	muxKATimeout      time.Duration
	muxMaxFrameSize   int
	muxMaxReceiveBuf  int
	muxMaxStreamBuf   int
	proxyProto        int
}

// sharedEntry holds one shared muxSession plus a per-key mutex used to
// serialize handshake attempts for that key.  Callers take the lightweight
// sharedRegistryMu only long enough to look up / create the entry; the
// heavyweight work (TLS + HTTP auth + smux negotiation) runs under entry.mu
// so other keys are not blocked.
type sharedEntry struct {
	mu      sync.Mutex
	session *muxSession
}

var (
	sharedRegistryMu sync.Mutex
	sharedRegistry   = map[sessionKey]*sharedEntry{}
)

func (d *relayxDialer) sessionKey(addr string) sessionKey {
	var (
		serverName   string
		insecureSkip bool
		nextProtos   string
	)
	if tlsCfg := d.options.TLSConfig; tlsCfg != nil {
		serverName = tlsCfg.ServerName
		insecureSkip = tlsCfg.InsecureSkipVerify
		nextProtos = strings.Join(tlsCfg.NextProtos, ",")
	}
	k := sessionKey{
		addr:         addr,
		mdKey:        d.md.key,
		host:         d.md.host,
		serverName:   serverName,
		insecureSkip: insecureSkip,
		nextProtos:   nextProtos,
		proxyProto:   int(d.options.ProxyProtocol),
	}
	if d.md.muxCfg != nil {
		k.muxVersion = d.md.muxCfg.Version
		k.muxKAInterval = d.md.muxCfg.KeepAliveInterval
		k.muxKADisabled = d.md.muxCfg.KeepAliveDisabled
		k.muxKATimeout = d.md.muxCfg.KeepAliveTimeout
		k.muxMaxFrameSize = d.md.muxCfg.MaxFrameSize
		k.muxMaxReceiveBuf = d.md.muxCfg.MaxReceiveBuffer
		k.muxMaxStreamBuf = d.md.muxCfg.MaxStreamBuffer
	}
	return k
}

func getSharedEntry(k sessionKey) *sharedEntry {
	sharedRegistryMu.Lock()
	defer sharedRegistryMu.Unlock()
	e, ok := sharedRegistry[k]
	if !ok {
		e = &sharedEntry{}
		sharedRegistry[k] = e
	}
	return e
}

func dropSharedEntry(k sessionKey, expect *sharedEntry) {
	sharedRegistryMu.Lock()
	defer sharedRegistryMu.Unlock()
	if e, ok := sharedRegistry[k]; ok && e == expect {
		delete(sharedRegistry, k)
	}
}

