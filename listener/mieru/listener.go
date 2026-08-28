package mieru

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	pb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"github.com/go-gost/core/limiter"
	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	md "github.com/go-gost/core/metadata"
	xctx "github.com/go-gost/x/ctx"
	umieru "github.com/go-gost/x/internal/util/mieru"
	climiter "github.com/go-gost/x/limiter/conn/wrapper"
	limiter_wrapper "github.com/go-gost/x/limiter/traffic/wrapper"
	traffic_limiter "github.com/go-gost/x/limiter/traffic"
	metrics "github.com/go-gost/x/metrics/wrapper"
	stats "github.com/go-gost/x/observer/stats/wrapper"
	"github.com/go-gost/x/registry"
	"google.golang.org/protobuf/proto"
)

func init() {
	registry.ListenerRegistry().Register("mieru", NewListener)
}

type mieruListener struct {
	server  mieruserver.Server
	addr    net.Addr
	log     logger.Logger
	md      metadata
	options listener.Options
	closed  sync.Once
}

func NewListener(opts ...listener.Option) listener.Listener {
	options := listener.Options{}
	for _, opt := range opts {
		opt(&options)
	}
	return &mieruListener{
		log:     options.Logger,
		options: options,
	}
}

func (l *mieruListener) Init(m md.Metadata) error {
	if err := l.parseMetadata(m); err != nil {
		return err
	}
	if len(l.md.users) == 0 {
		return fmt.Errorf("mieru: no user configured")
	}

	port, err := parseListenPort(l.options.Addr)
	if err != nil {
		return err
	}

	users := make([]*pb.User, 0, len(l.md.users))
	for name, password := range l.md.users {
		if password == "" {
			return fmt.Errorf("mieru: empty password for user %q", name)
		}
		users = append(users, &pb.User{
			Name:     proto.String(name),
			Password: proto.String(password),
		})
	}

	serverCfg := &mieruserver.ServerConfig{
		Config: &pb.ServerConfig{
			PortBindings: buildPortBindings(port, l.md.protocol),
			Users:        users,
			AdvancedSettings: &pb.ServerAdvancedSettings{
				UserHintIsMandatory: proto.Bool(l.md.userHintIsMandatory),
			},
		},
	}

	s := mieruserver.NewServer()
	if err := s.Store(serverCfg); err != nil {
		return fmt.Errorf("mieru: store config: %w", err)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("mieru: start: %w", err)
	}

	l.server = s
	l.addr = listenerAddr(port, l.md.protocol)
	return nil
}

func buildPortBindings(port int, protocol string) []*pb.PortBinding {
	add := func(p pb.TransportProtocol) *pb.PortBinding {
		return &pb.PortBinding{
			Port:     proto.Int32(int32(port)),
			Protocol: p.Enum(),
		}
	}
	switch protocol {
	case "udp":
		return []*pb.PortBinding{add(pb.TransportProtocol_UDP)}
	case "both", "tcp,udp", "tcp+udp", "tcpudp":
		return []*pb.PortBinding{
			add(pb.TransportProtocol_TCP),
			add(pb.TransportProtocol_UDP),
		}
	default:
		return []*pb.PortBinding{add(pb.TransportProtocol_TCP)}
	}
}

func listenerAddr(port int, protocol string) net.Addr {
	if protocol == "udp" {
		return &net.UDPAddr{IP: net.IPv4zero, Port: port}
	}
	return &net.TCPAddr{IP: net.IPv4zero, Port: port}
}

func (l *mieruListener) Accept() (net.Conn, error) {
	conn, req, err := l.server.Accept()
	if err != nil {
		if !l.server.IsRunning() {
			return nil, listener.ErrClosed
		}
		return nil, err
	}

	baseCtx := context.Background()
	if userCtx, ok := conn.(apicommon.UserContext); ok && userCtx.UserName() != "" {
		baseCtx = xctx.ContextWithClientID(baseCtx, xctx.ClientID(userCtx.UserName()))
	}
	baseCtx = umieru.ContextWithRequest(baseCtx, req)

	c := &contextConn{
		Conn: conn,
		ctx:  baseCtx,
	}

	if l.options.ConnLimiter != nil {
		host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
		if lim := l.options.ConnLimiter.Limiter(host); lim != nil {
			if lim.Allow(1) {
				c.Conn = climiter.WrapConn(lim, c.Conn)
			} else {
				c.Close()
				return nil, errors.New("mieru: connection limit exceeded")
			}
		}
	}

	c.Conn = metrics.WrapConn(l.options.Service, c.Conn)
	c.Conn = stats.WrapConn(c.Conn, l.options.Stats)
	c.Conn = limiter_wrapper.WrapConn(
		c.Conn,
		l.options.TrafficLimiter,
		traffic_limiter.ServiceLimitKey,
		limiter.ScopeOption(limiter.ScopeService),
		limiter.ServiceOption(l.options.Service),
		limiter.NetworkOption(c.LocalAddr().Network()),
	)
	c.Conn = limiter_wrapper.WrapConn(
		c.Conn,
		l.options.TrafficLimiter,
		c.RemoteAddr().String(),
		limiter.ScopeOption(limiter.ScopeConn),
		limiter.ServiceOption(l.options.Service),
		limiter.NetworkOption(c.LocalAddr().Network()),
		limiter.SrcOption(c.RemoteAddr().String()),
	)

	return c, nil
}

func (l *mieruListener) Addr() net.Addr {
	return l.addr
}

func (l *mieruListener) Close() error {
	var err error
	l.closed.Do(func() {
		if l.server != nil {
			err = l.server.Stop()
		}
	})
	return err
}

func parseListenPort(addr string) (int, error) {
	if addr == "" {
		return 0, fmt.Errorf("mieru: listen address is empty")
	}
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("mieru: invalid listen address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("mieru: invalid listen port %q", portStr)
	}
	return port, nil
}

type contextConn struct {
	net.Conn
	ctx context.Context
}

func (c *contextConn) Context() context.Context {
	return c.ctx
}
