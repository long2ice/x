package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/metadata"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/parsing"
	_ "github.com/go-gost/x/listener/tcp"
	xlogger "github.com/go-gost/x/logger"
	"github.com/go-gost/x/registry"
	"gopkg.in/yaml.v3"
)

type routerCaptureHandler struct{}

func (*routerCaptureHandler) Init(metadata.Metadata) error { return nil }
func (*routerCaptureHandler) Handle(context.Context, net.Conn, ...handler.HandleOption) error {
	return nil
}

func TestServiceRouterDefaultsAndExplicitRetriesSurviveReload(t *testing.T) {
	logger.SetDefault(xlogger.Nop())
	parsing.SetDefaultTLSConfig(&tls.Config{})
	var captured *chain.RouterOptions
	const handlerName = "capture-router-defaults"
	registry.HandlerRegistry().Register(handlerName, func(opts ...handler.Option) handler.Handler {
		var options handler.Options
		for _, opt := range opts {
			opt(&options)
		}
		captured = options.Router.Options()
		return &routerCaptureHandler{}
	})
	t.Cleanup(func() { registry.HandlerRegistry().Unregister(handlerName) })
	for _, tt := range []struct {
		name    string
		raw     string
		retries int
		timeout time.Duration
	}{
		{"defaults", `{}`, 1, 3 * time.Second},
		{"explicit zero", `{"handler":{"retries":0}}`, 0, 3 * time.Second},
		{"explicit retries", `{"handler":{"retries":3},"metadata":{"dialTimeout":"7s"}}`, 3, 7 * time.Second},
		{"zero timeout uses default", `{"metadata":{"dialTimeout":"0s"}}`, 1, 3 * time.Second},
	} {
		for _, format := range []string{"json", "yaml"} {
			t.Run(tt.name+"/"+format, func(t *testing.T) {
				var cfg config.ServiceConfig
				if err := json.Unmarshal([]byte(tt.raw), &cfg); err != nil {
					t.Fatal(err)
				}
				// Reload serializes the typed config again. In particular, an
				// explicit retries:0 must not disappear through omitempty.
				if format == "json" {
					b, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					cfg = config.ServiceConfig{}
					if err := json.Unmarshal(b, &cfg); err != nil {
						t.Fatal(err)
					}
				} else {
					b, err := yaml.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					cfg = config.ServiceConfig{}
					if err := yaml.Unmarshal(b, &cfg); err != nil {
						t.Fatal(err)
					}
				}
				cfg.Name, cfg.Addr = tt.name, "127.0.0.1:0"
				if cfg.Handler == nil {
					cfg.Handler = &config.HandlerConfig{}
				}
				cfg.Handler.Type = handlerName
				svc, err := ParseService(&cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer svc.Close()
				if captured == nil || captured.Retries != tt.retries || captured.Timeout != tt.timeout {
					t.Fatalf("parsed router = %+v, want retries=%d timeout=%s", captured, tt.retries, tt.timeout)
				}
			})
		}
	}
}
