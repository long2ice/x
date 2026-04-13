package observer

import (
	"crypto/tls"
	"strings"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/observer"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/internal/plugin"
	observer_plugin "github.com/go-gost/x/observer/plugin"
)

func ParseObserver(cfg *config.ObserverConfig) observer.Observer {
	if cfg == nil || cfg.Plugin == nil {
		return nil
	}

	var tlsCfg *tls.Config
	if cfg.Plugin.TLS != nil {
		tlsCfg = &tls.Config{
			ServerName:         cfg.Plugin.TLS.ServerName,
			InsecureSkipVerify: !cfg.Plugin.TLS.Secure,
		}
	}

	var obs observer.Observer
	switch strings.ToLower(cfg.Plugin.Type) {
	case "http":
		obs = observer_plugin.NewHTTPPlugin(
			cfg.Name, cfg.Plugin.Addr,
			plugin.TLSConfigOption(tlsCfg),
			plugin.TimeoutOption(cfg.Plugin.Timeout),
		)
	default:
		obs = observer_plugin.NewGRPCPlugin(
			cfg.Name, cfg.Plugin.Addr,
			plugin.TokenOption(cfg.Plugin.Token),
			plugin.TLSConfigOption(tlsCfg),
		)
	}

	if obs == nil {
		return nil
	}

	// Wrap with batch observer to aggregate events from all services/handlers
	// and send them in batched HTTP/gRPC calls instead of one-per-service.
	return observer_plugin.NewBatchObserver(obs,
		observer_plugin.BatchFlushTimeoutOption(cfg.Plugin.Timeout),
		observer_plugin.BatchLoggerOption(logger.Default().WithFields(map[string]any{
			"kind":     "observer",
			"observer": cfg.Name,
		})),
	)
}
