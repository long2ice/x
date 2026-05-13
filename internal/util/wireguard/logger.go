package wireguard

import (
	"github.com/go-gost/core/logger"
	wgdevice "golang.zx2c4.com/wireguard/device"
)

// NewLogger builds a wireguard-go *Logger that routes messages to the supplied
// core logger at a level controlled by `level` (verbose|debug|trace -> debug
// + error; error|"" -> error only; anything else -> silent).
func NewLogger(log logger.Logger, level string) *wgdevice.Logger {
	lg := &wgdevice.Logger{
		Verbosef: wgdevice.DiscardLogf,
		Errorf:   wgdevice.DiscardLogf,
	}
	if log == nil {
		return lg
	}
	switch level {
	case "verbose", "debug", "trace":
		lg.Verbosef = func(format string, args ...any) { log.Debugf(format, args...) }
		lg.Errorf = func(format string, args ...any) { log.Errorf(format, args...) }
	case "error", "":
		lg.Errorf = func(format string, args ...any) { log.Errorf(format, args...) }
	}
	return lg
}
