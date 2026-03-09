package util

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestOrderedProxyURLs_RotatesStartIndex(t *testing.T) {
	proxyPoolCounter.Store(0)

	first := OrderedProxyURLs("http://proxy-1,http://proxy-2,http://proxy-3")
	second := OrderedProxyURLs("http://proxy-1,http://proxy-2,http://proxy-3")

	if got, want := strings.Join(first, ","), "http://proxy-1,http://proxy-2,http://proxy-3"; got != want {
		t.Fatalf("first order = %q, want %q", got, want)
	}
	if got, want := strings.Join(second, ","), "http://proxy-2,http://proxy-3,http://proxy-1"; got != want {
		t.Fatalf("second order = %q, want %q", got, want)
	}
}

func TestProxyPoolTransport_FallsBackAndReplaysBody(t *testing.T) {
	var seenBodies []string
	transport := &proxyPoolTransport{
		entries: []proxyTransportEntry{
			{
				proxyURL: "http://proxy-1",
				transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(req.Body)
					seenBodies = append(seenBodies, string(body))
					return nil, errors.New("dial tcp proxy-1: connect failed")
				}),
			},
			{
				proxyURL: "http://proxy-2",
				transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(req.Body)
					seenBodies = append(seenBodies, string(body))
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader("ok")),
						Header:     make(http.Header),
					}, nil
				}),
			},
		},
	}

	req, errReq := http.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", strings.NewReader(`{"hello":"world"}`))
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}

	resp, errRoundTrip := transport.RoundTrip(req)
	if errRoundTrip != nil {
		t.Fatalf("round trip: %v", errRoundTrip)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if got := len(seenBodies); got != 2 {
		t.Fatalf("expected 2 proxy attempts, got %d", got)
	}
	if seenBodies[0] != `{"hello":"world"}` || seenBodies[1] != `{"hello":"world"}` {
		t.Fatalf("unexpected replayed bodies: %#v", seenBodies)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestProxyPoolTransport_DoesNotFallbackOnContextCancellation(t *testing.T) {
	proxyPoolCounter.Store(0)

	transport := &proxyPoolTransport{
		entries: []proxyTransportEntry{
			{
				proxyURL: "http://proxy-1",
				transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					return nil, req.Context().Err()
				}),
			},
			{
				proxyURL: "http://proxy-2",
				transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					t.Fatal("second proxy should not be used on context cancellation")
					return nil, nil
				}),
			},
		},
	}

	req, errReq := http.NewRequest(http.MethodGet, "https://example.com/v1/models", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	canceledReq := req.Clone(req.Context())
	ctx, cancel := context.WithCancel(canceledReq.Context())
	cancel()
	canceledReq = canceledReq.WithContext(ctx)

	if _, errRoundTrip := transport.RoundTrip(canceledReq); !errors.Is(errRoundTrip, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", errRoundTrip)
	}
}
