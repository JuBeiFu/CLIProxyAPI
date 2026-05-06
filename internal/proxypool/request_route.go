package proxypool

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexroute"
)

type RequestRoute = codexroute.RequestRoute

func WithRequestRoute(ctx context.Context, route RequestRoute) context.Context {
	return codexroute.WithRequestRoute(ctx, route)
}

func RequestRouteFromContext(ctx context.Context) (RequestRoute, bool) {
	return codexroute.RequestRouteFromContext(ctx)
}
