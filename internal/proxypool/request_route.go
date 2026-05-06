package proxypool

import "context"

type RequestRoute struct {
	Pool   string
	Entry  string
	Direct bool
}

type requestRouteContextKey struct{}

func WithRequestRoute(ctx context.Context, route RequestRoute) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestRouteContextKey{}, route)
}

func RequestRouteFromContext(ctx context.Context) (RequestRoute, bool) {
	if ctx == nil {
		return RequestRoute{}, false
	}
	route, ok := ctx.Value(requestRouteContextKey{}).(RequestRoute)
	return route, ok
}
