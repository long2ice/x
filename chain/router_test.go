package chain_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/go-gost/core/chain"
	"github.com/go-gost/core/logger"
	xchain "github.com/go-gost/x/chain"
	"github.com/go-gost/x/config"
	chainparser "github.com/go-gost/x/config/parsing/chain"
	_ "github.com/go-gost/x/connector/http"
	_ "github.com/go-gost/x/dialer/tcp"
	xlogger "github.com/go-gost/x/logger"
)

func TestRouterDefaultsAndOverrides(t *testing.T) {
	for _, tt := range []struct {
		name    string
		opts    []core.RouterOption
		retries int
		timeout time.Duration
	}{
		{name: "defaults", retries: 1, timeout: 3 * time.Second},
		{name: "explicit zero disables retries", opts: []core.RouterOption{core.RetriesRouterOption(0)}, retries: 0, timeout: 3 * time.Second},
		{name: "custom settings", opts: []core.RouterOption{core.RetriesRouterOption(4), core.TimeoutRouterOption(8 * time.Second)}, retries: 4, timeout: 8 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := xchain.NewRouter(append(tt.opts, core.LoggerRouterOption(xlogger.Nop()))...)
			if r.Options().Retries != tt.retries || r.Options().Timeout != tt.timeout {
				t.Fatalf("router options = %+v, want retries=%d timeout=%s", r.Options(), tt.retries, tt.timeout)
			}
		})
	}
}

func TestDefaultRouterFailsOverWithinCurrentRequest(t *testing.T) {
	logger.SetDefault(xlogger.Nop())
	for _, disable := range []bool{false, true} {
		t.Run(fmt.Sprintf("disableRetries=%v", disable), func(t *testing.T) {
			// Force the first selection to an unavailable proxy. The second proxy
			// accepts CONNECT, exercising the real route selector and fail marker.
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				c, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer c.Close()
				fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				bufio.NewReader(c).ReadByte()
			}))
			defer good.Close()
			dead, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			bad := dead.Addr().String()
			dead.Close()
			nodes := []*config.NodeConfig{}
			for i, addr := range []string{bad, strings.TrimPrefix(good.URL, "http://")} {
				nodes = append(nodes, &config.NodeConfig{Name: fmt.Sprint(i), Addr: addr, Connector: &config.ConnectorConfig{Type: "http"}, Dialer: &config.DialerConfig{Type: "tcp"}})
			}
			ch, err := chainparser.ParseChain(&config.ChainConfig{Name: "failover", Hops: []*config.HopConfig{{Name: "exit", Selector: &config.SelectorConfig{Strategy: "fifo"}, Nodes: nodes}}}, xlogger.Nop())
			if err != nil {
				t.Fatal(err)
			}
			opts := []core.RouterOption{core.ChainRouterOption(ch), core.LoggerRouterOption(xlogger.Nop())}
			if disable {
				opts = append(opts, core.RetriesRouterOption(0))
			}
			r := xchain.NewRouter(opts...)
			c, err := r.Dial(context.Background(), "tcp", "127.0.0.1:80")
			if disable {
				if err == nil {
					c.Close()
					t.Fatal("explicit zero retries unexpectedly retried the failed proxy")
				}
				// Failure filtering alone helps the next request, not the first.
				c, err = r.Dial(context.Background(), "tcp", "127.0.0.1:80")
			}
			if err != nil {
				t.Fatalf("healthy proxy was not selected: %v", err)
			}
			c.Close()
		})
	}
}
