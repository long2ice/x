package reality

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/go-gost/core/listener"
	"github.com/go-gost/core/logger"
	xlogger "github.com/go-gost/x/logger"
	xmd "github.com/go-gost/x/metadata"
)

func TestMain(m *testing.M) {
	logger.SetDefault(xlogger.NewLogger(xlogger.LevelOption(logger.ErrorLevel)))
	os.Exit(m.Run())
}

// TestListenerReload checks that starting and stopping the listener, as a
// config reload does, leaves nothing behind.
func TestListenerReload(t *testing.T) {
	private, _, _ := GenerateKeyPair()

	md := xmd.NewMetadata(map[string]any{
		"privateKey":  private,
		"dest":        "127.0.0.1:1",
		"serverNames": "example.com",
	})

	before := runtime.NumGoroutine()
	for range 5 {
		ln := NewListener(
			listener.AddrOption("127.0.0.1:0"),
			listener.LoggerOption(logger.Default()),
		)
		if err := ln.Init(md); err != nil {
			t.Fatal(err)
		}
		if err := ln.Close(); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Errorf("%d goroutines left over, started with %d", n, before)
	}
}
