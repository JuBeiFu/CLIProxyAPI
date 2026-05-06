package codexroute

import (
	"context"
	"strings"
)

type RequestRoute struct {
	Pool     string
	Entry    string
	Direct   bool
	Assisted bool
}

type RouteDescriptor struct {
	Pool   string
	Entry  string
	Direct bool
}

type AuthRoutePlan struct {
	Primary RouteDescriptor
	Standby RouteDescriptor
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

func (r RouteDescriptor) RequestRoute() RequestRoute {
	return RequestRoute{
		Pool:     strings.TrimSpace(r.Pool),
		Entry:    strings.TrimSpace(r.Entry),
		Direct:   r.Direct,
		Assisted: !r.Direct,
	}
}
