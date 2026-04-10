package helps

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport := unwrapTransport(t, client.Transport)
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

// unwrapTransport extracts the underlying *http.Transport from a potentially
// wrapped RoundTripper (e.g. metadataRoundTripper from proxystats).
func unwrapTransport(t *testing.T, rt http.RoundTripper) *http.Transport {
	t.Helper()
	if transport, ok := rt.(*http.Transport); ok {
		return transport
	}
	// The transport may be wrapped by proxystats.metadataRoundTripper which
	// stores the base transport in an unexported "base" field.
	value := reflect.ValueOf(rt)
	if value.Kind() != reflect.Ptr || value.IsNil() {
		t.Fatalf("unexpected round tripper type: %T", rt)
	}
	elem := value.Elem()
	field := elem.FieldByName("base")
	if !field.IsValid() || !field.CanAddr() {
		t.Fatalf("round tripper %T does not expose base transport", rt)
	}
	baseValue := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	base, ok := baseValue.Interface().(http.RoundTripper)
	if !ok {
		t.Fatalf("round tripper %T base is not http.RoundTripper", rt)
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		t.Fatalf("round tripper %T base type = %T, want *http.Transport", rt, base)
	}
	return transport
}
