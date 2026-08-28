package mieru

import (
	"context"

	"github.com/enfein/mieru/v3/apis/model"
)

type requestKey struct{}

// ContextWithRequest attaches the SOCKS5 request parsed by the mieru listener.
func ContextWithRequest(ctx context.Context, req *model.Request) context.Context {
	return context.WithValue(ctx, requestKey{}, req)
}

// RequestFromContext returns the SOCKS5 request from the listener, if any.
func RequestFromContext(ctx context.Context) *model.Request {
	v, _ := ctx.Value(requestKey{}).(*model.Request)
	return v
}
