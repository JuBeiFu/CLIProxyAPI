package cliproxy

import (
	"net/http"
	"reflect"
	"testing"
	"unsafe"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRoundTripperForDirectBypassesProxy(t *testing.T) {
	provider := newDefaultRoundTripperProvider(func() *internalconfig.Config {
		return &internalconfig.Config{SDKConfig: internalconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}}
	})

	rt := provider.RoundTripperFor(&coreauth.Auth{ProxyURL: "direct"})
	transport := unwrapTransport(t, rt)
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func unwrapTransport(t *testing.T, rt http.RoundTripper) *http.Transport {
	t.Helper()
	if transport, ok := rt.(*http.Transport); ok {
		return transport
	}
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
