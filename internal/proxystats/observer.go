package proxystats

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type observedBody struct {
	inner      io.ReadCloser
	meta       RequestMetadata
	proxyURL   string
	statusCode int
	startedAt  time.Time
	headerAt   time.Time

	mu          sync.Mutex
	firstByteAt time.Time
	finished    bool
}

func RecordTransportFailure(ctx context.Context, proxyURL string, startedAt time.Time, err error) {
	meta, _ := MetadataFromContext(ctx)
	completedAt := time.Now()
	DefaultStore().Record(Attempt{
		Timestamp:        completedAt,
		StartedAt:        startedAt,
		CompletedAt:      completedAt,
		ProxyURL:         proxyURL,
		ProxyDisplay:     RedactProxyURL(proxyURL),
		ProxyProfile:     meta.ProxyProfile,
		SelectionSource:  meta.SelectionSource,
		RoutingRule:      meta.RoutingRule,
		Provider:         meta.Provider,
		PlanType:         meta.PlanType,
		AuthKind:         meta.AuthKind,
		AuthID:           meta.AuthID,
		AuthIndex:        meta.AuthIndex,
		Success:          false,
		ResponseReceived: false,
		TotalDurationMs:  completedAt.Sub(startedAt).Milliseconds(),
		Error:            strings.TrimSpace(errorString(err)),
	})
}

func WrapResponse(ctx context.Context, proxyURL string, startedAt time.Time, resp *http.Response) *http.Response {
	if resp == nil {
		return nil
	}
	meta, _ := MetadataFromContext(ctx)
	headerAt := time.Now()
	if resp.Body == nil || resp.Body == http.NoBody {
		completedAt := headerAt
		DefaultStore().Record(Attempt{
			Timestamp:           completedAt,
			StartedAt:           startedAt,
			CompletedAt:         completedAt,
			ProxyURL:            proxyURL,
			ProxyDisplay:        RedactProxyURL(proxyURL),
			ProxyProfile:        meta.ProxyProfile,
			SelectionSource:     meta.SelectionSource,
			RoutingRule:         meta.RoutingRule,
			Provider:            meta.Provider,
			PlanType:            meta.PlanType,
			AuthKind:            meta.AuthKind,
			AuthID:              meta.AuthID,
			AuthIndex:           meta.AuthIndex,
			StatusCode:          resp.StatusCode,
			Success:             true,
			ResponseReceived:    true,
			FirstByteDurationMs: headerAt.Sub(startedAt).Milliseconds(),
			TotalDurationMs:     completedAt.Sub(startedAt).Milliseconds(),
		})
		return resp
	}
	resp.Body = &observedBody{
		inner:      resp.Body,
		meta:       meta,
		proxyURL:   proxyURL,
		statusCode: resp.StatusCode,
		startedAt:  startedAt,
		headerAt:   headerAt,
	}
	return resp
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if n > 0 {
		b.markFirstByte()
	}
	if err == io.EOF {
		b.finish(true, nil)
	} else if err != nil {
		b.finish(false, err)
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.inner.Close()
	b.finish(err == nil, err)
	return err
}

func (b *observedBody) markFirstByte() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.firstByteAt.IsZero() {
		b.firstByteAt = time.Now()
	}
}

func (b *observedBody) finish(success bool, err error) {
	b.mu.Lock()
	if b.finished {
		b.mu.Unlock()
		return
	}
	b.finished = true
	firstByteAt := b.firstByteAt
	if firstByteAt.IsZero() {
		firstByteAt = b.headerAt
	}
	statusCode := b.statusCode
	proxyURL := b.proxyURL
	meta := b.meta
	startedAt := b.startedAt
	b.mu.Unlock()

	completedAt := time.Now()
	DefaultStore().Record(Attempt{
		Timestamp:           completedAt,
		StartedAt:           startedAt,
		CompletedAt:         completedAt,
		ProxyURL:            proxyURL,
		ProxyDisplay:        RedactProxyURL(proxyURL),
		ProxyProfile:        meta.ProxyProfile,
		SelectionSource:     meta.SelectionSource,
		RoutingRule:         meta.RoutingRule,
		Provider:            meta.Provider,
		PlanType:            meta.PlanType,
		AuthKind:            meta.AuthKind,
		AuthID:              meta.AuthID,
		AuthIndex:           meta.AuthIndex,
		StatusCode:          statusCode,
		Success:             success,
		ResponseReceived:    true,
		FirstByteDurationMs: firstByteAt.Sub(startedAt).Milliseconds(),
		TotalDurationMs:     completedAt.Sub(startedAt).Milliseconds(),
		Error:               strings.TrimSpace(errorString(err)),
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", err))
}
