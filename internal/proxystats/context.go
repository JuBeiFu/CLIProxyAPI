package proxystats

import (
	"context"
	"net/http"
	"strings"
)

type RequestMetadata struct {
	ProxyProfile    string
	SelectionSource string
	RoutingRule     string
	Provider        string
	PlanType        string
	AuthKind        string
	AuthID          string
	AuthIndex       string
}

type requestMetadataKey struct{}

type metadataRoundTripper struct {
	base http.RoundTripper
	meta RequestMetadata
}

func (m RequestMetadata) normalized() RequestMetadata {
	m.ProxyProfile = strings.TrimSpace(m.ProxyProfile)
	m.SelectionSource = strings.TrimSpace(m.SelectionSource)
	m.RoutingRule = strings.TrimSpace(m.RoutingRule)
	m.Provider = normalizeIdentifier(m.Provider)
	m.PlanType = normalizeIdentifier(m.PlanType)
	m.AuthKind = normalizeIdentifier(m.AuthKind)
	m.AuthID = strings.TrimSpace(m.AuthID)
	m.AuthIndex = strings.TrimSpace(m.AuthIndex)
	return m
}

func (m RequestMetadata) IsZero() bool {
	return m.ProxyProfile == "" &&
		m.SelectionSource == "" &&
		m.RoutingRule == "" &&
		m.Provider == "" &&
		m.PlanType == "" &&
		m.AuthKind == "" &&
		m.AuthID == "" &&
		m.AuthIndex == ""
}

func WithRequestMetadata(ctx context.Context, meta RequestMetadata) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized := meta.normalized()
	if normalized.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, requestMetadataKey{}, normalized)
}

func MetadataFromContext(ctx context.Context) (RequestMetadata, bool) {
	if ctx == nil {
		return RequestMetadata{}, false
	}
	meta, ok := ctx.Value(requestMetadataKey{}).(RequestMetadata)
	if !ok {
		return RequestMetadata{}, false
	}
	return meta.normalized(), true
}

func AttachRoundTripperMetadata(base http.RoundTripper, meta RequestMetadata) http.RoundTripper {
	if base == nil {
		return nil
	}
	normalized := meta.normalized()
	if normalized.IsZero() {
		return base
	}
	return &metadataRoundTripper{base: base, meta: normalized}
}

func (rt *metadataRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return rt.base.RoundTrip(req)
	}
	return rt.base.RoundTrip(req.Clone(WithRequestMetadata(req.Context(), rt.meta)))
}

func normalizeIdentifier(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.ReplaceAll(value, " ", "-")
	return value
}
