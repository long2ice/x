package tungo

import (
	"testing"
	"time"

	xmd "github.com/go-gost/x/metadata"
)

func TestTCPResourceConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values map[string]any
		idle   time.Duration
		buffer int
	}{
		{"defaults", nil, time.Minute, 16 * 1024},
		{"legacy key", map[string]any{"tungo.tcpIdleTimeout": "2m", "tcp.bufferSize": 8192}, 2 * time.Minute, 8192},
		{"shared key", map[string]any{"idleTimeout": "3m"}, 3 * time.Minute, 16 * 1024},
		{"disabled", map[string]any{"tcpIdleTimeout": 0}, 0, 16 * 1024},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &tungoHandler{}
			if err := h.parseMetadata(xmd.NewMetadata(tt.values)); err != nil {
				t.Fatal(err)
			}
			if h.md.tcpIdleTimeout != tt.idle || h.md.tcpBufferSize != tt.buffer {
				t.Fatalf("TCP resources: idle=%v buffer=%d", h.md.tcpIdleTimeout, h.md.tcpBufferSize)
			}
		})
	}
	h := &tungoHandler{}
	if err := h.parseMetadata(xmd.NewMetadata(map[string]any{"tcpIdleTimeout": "-1s"})); err == nil {
		t.Fatal("negative idle timeout must be rejected")
	}
}
