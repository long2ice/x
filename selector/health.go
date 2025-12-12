package selector

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/core/selector"
)

type CheckType string

const (
	CheckTypeTCP  CheckType = "tcp"
	CheckTypeHTTP CheckType = "http"
)

type HealthCheckConfig struct {
	Interval     time.Duration
	Timeout      time.Duration
	Type         CheckType
	Path         string
	ExpectStatus int
}

type HealthChecker struct {
	config     HealthCheckConfig
	logger     logger.Logger
	cancelFunc context.CancelFunc
}

type HealthCheckerOption func(*HealthChecker)

func HealthCheckIntervalOption(d time.Duration) HealthCheckerOption {
	return func(hc *HealthChecker) {
		hc.config.Interval = d
	}
}

func HealthCheckTimeoutOption(d time.Duration) HealthCheckerOption {
	return func(hc *HealthChecker) {
		hc.config.Timeout = d
	}
}

func HealthCheckTypeOption(t CheckType) HealthCheckerOption {
	return func(hc *HealthChecker) {
		hc.config.Type = t
	}
}

func HealthCheckPathOption(path string) HealthCheckerOption {
	return func(hc *HealthChecker) {
		hc.config.Path = path
	}
}

func HealthCheckExpectStatusOption(status int) HealthCheckerOption {
	return func(hc *HealthChecker) {
		hc.config.ExpectStatus = status
	}
}

func HealthCheckLoggerOption(l logger.Logger) HealthCheckerOption {
	return func(hc *HealthChecker) {
		hc.logger = l
	}
}

func NewHealthChecker(opts ...HealthCheckerOption) *HealthChecker {
	hc := &HealthChecker{
		config: HealthCheckConfig{
			Interval:     30 * time.Second,
			Timeout:      5 * time.Second,
			Type:         CheckTypeTCP,
			ExpectStatus: 200,
		},
	}
	for _, opt := range opts {
		opt(hc)
	}
	if hc.config.Interval <= 0 {
		hc.config.Interval = 30 * time.Second
	}
	if hc.config.Timeout <= 0 {
		hc.config.Timeout = 5 * time.Second
	}
	return hc
}

func (hc *HealthChecker) Start(nodes []any) {
	ctx, cancel := context.WithCancel(context.Background())
	hc.cancelFunc = cancel
	go hc.run(ctx, nodes)
}

func (hc *HealthChecker) Stop() {
	if hc.cancelFunc != nil {
		hc.cancelFunc()
	}
}

func (hc *HealthChecker) run(ctx context.Context, nodes []any) {
	ticker := time.NewTicker(hc.config.Interval)
	defer ticker.Stop()

	hc.checkAll(nodes)

	for {
		select {
		case <-ticker.C:
			hc.checkAll(nodes)
		case <-ctx.Done():
			return
		}
	}
}

func (hc *HealthChecker) checkAll(nodes []any) {
	var wg sync.WaitGroup
	for _, node := range nodes {
		wg.Add(1)
		go func(n any) {
			defer wg.Done()
			hc.check(n)
		}(node)
	}
	wg.Wait()
}

func (hc *HealthChecker) check(v any) {
	node, ok := v.(*chain.Node)
	if !ok || node == nil {
		return
	}

	addr := node.Addr
	if addr == "" {
		return
	}

	marker := node.Marker()
	if marker == nil {
		markable, ok := v.(selector.Markable)
		if !ok {
			return
		}
		marker = markable.Marker()
		if marker == nil {
			return
		}
	}

	var err error
	switch hc.config.Type {
	case CheckTypeHTTP:
		err = hc.checkHTTP(addr)
	default:
		err = hc.checkTCP(addr)
	}

	if err != nil {
		marker.Mark()
		if hc.logger != nil {
			hc.logger.Debugf("health check failed for %s: %v", addr, err)
		}
	} else {
		marker.Reset()
		if hc.logger != nil {
			hc.logger.Debugf("health check passed for %s", addr)
		}
	}
}

func (hc *HealthChecker) checkTCP(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, hc.config.Timeout)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (hc *HealthChecker) checkHTTP(addr string) error {
	client := &http.Client{
		Timeout: hc.config.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	path := hc.config.Path
	if path == "" {
		path = "/"
	}

	url := fmt.Sprintf("http://%s%s", addr, path)
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if hc.config.ExpectStatus > 0 && resp.StatusCode != hc.config.ExpectStatus {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}

	return fmt.Errorf("health check failed with status: %d", resp.StatusCode)
}
