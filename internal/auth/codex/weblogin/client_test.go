package weblogin

import (
	"net/http/httptest"
	"testing"
)

func TestClientCommonHeaders(t *testing.T) {
	c := &Client{userAgent: "UA/1.0", deviceID: "dev-1"}
	req := httptest.NewRequest("GET", "https://chatgpt.com/", nil)
	c.applyCommonHeaders(req)
	if req.Header.Get("User-Agent") != "UA/1.0" {
		t.Fatalf("UA not set")
	}
	if req.Header.Get("oai-device-id") != "dev-1" {
		t.Fatalf("device id not set")
	}
}
