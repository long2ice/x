package selector

import (
	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/selector"
	"github.com/go-gost/x/config"
	xs "github.com/go-gost/x/selector"
)

func ParseChainSelector(cfg *config.SelectorConfig) selector.Selector[chain.Chainer] {
	if cfg == nil {
		return nil
	}

	var strategy selector.Strategy[chain.Chainer]
	switch cfg.Strategy {
	case "round", "rr":
		strategy = xs.RoundRobinStrategy[chain.Chainer]()
	case "random", "rand":
		strategy = xs.RandomStrategy[chain.Chainer]()
	case "fifo", "ha":
		strategy = xs.FIFOStrategy[chain.Chainer]()
	case "hash":
		strategy = xs.HashStrategy[chain.Chainer]()
	case "leastconn", "lc":
		strategy = xs.LeastConnStrategy[chain.Chainer]()
	case "leastlatency", "ll":
		strategy = xs.LeastLatencyStrategy[chain.Chainer]()
	default:
		strategy = xs.RoundRobinStrategy[chain.Chainer]()
	}
	return xs.NewSelector(
		strategy,
		xs.FailFilter[chain.Chainer](cfg.MaxFails, cfg.FailTimeout),
		xs.BackupFilter[chain.Chainer](),
	)
}

func ParseNodeSelector(cfg *config.SelectorConfig) selector.Selector[*chain.Node] {
	if cfg == nil {
		return nil
	}

	var strategy selector.Strategy[*chain.Node]
	switch cfg.Strategy {
	case "round", "rr":
		strategy = xs.RoundRobinStrategy[*chain.Node]()
	case "random", "rand":
		strategy = xs.RandomStrategy[*chain.Node]()
	case "fifo", "ha":
		strategy = xs.FIFOStrategy[*chain.Node]()
	case "hash":
		strategy = xs.HashStrategy[*chain.Node]()
	case "leastconn", "lc":
		strategy = xs.LeastConnStrategy[*chain.Node]()
	case "leastlatency", "ll":
		strategy = xs.LeastLatencyStrategy[*chain.Node]()
	default:
		strategy = xs.RoundRobinStrategy[*chain.Node]()
	}

	var failFilter selector.Filter[*chain.Node]
	if cfg.HealthCheck {
		failFilter = xs.HealthCheckFilter[*chain.Node](cfg.MaxFails)
	} else {
		failFilter = xs.FailFilter[*chain.Node](cfg.MaxFails, cfg.FailTimeout)
	}

	return xs.NewSelector(
		strategy,
		failFilter,
		xs.BackupFilter[*chain.Node](),
	)
}

func DefaultNodeSelector() selector.Selector[*chain.Node] {
	return xs.NewSelector(
		xs.RoundRobinStrategy[*chain.Node](),
		xs.FailFilter[*chain.Node](xs.DefaultMaxFails, xs.DefaultFailTimeout),
		xs.BackupFilter[*chain.Node](),
	)
}

func DefaultChainSelector() selector.Selector[chain.Chainer] {
	return xs.NewSelector(
		xs.RoundRobinStrategy[chain.Chainer](),
		xs.FailFilter[chain.Chainer](xs.DefaultMaxFails, xs.DefaultFailTimeout),
		xs.BackupFilter[chain.Chainer](),
	)
}

func ParseHealthChecker(cfg *config.SelectorConfig, log logger.Logger) *xs.HealthChecker {
	if cfg == nil || !cfg.HealthCheck {
		return nil
	}

	var checkType xs.CheckType
	switch cfg.HealthCheckType {
	case "http":
		checkType = xs.CheckTypeHTTP
	default:
		checkType = xs.CheckTypeTCP
	}

	return xs.NewHealthChecker(
		xs.HealthCheckTypeOption(checkType),
		xs.HealthCheckIntervalOption(cfg.HealthInterval),
		xs.HealthCheckTimeoutOption(cfg.HealthTimeout),
		xs.HealthCheckPathOption(cfg.HealthPath),
		xs.HealthCheckExpectStatusOption(cfg.HealthExpectStatus),
		xs.HealthCheckLoggerOption(log),
	)
}
