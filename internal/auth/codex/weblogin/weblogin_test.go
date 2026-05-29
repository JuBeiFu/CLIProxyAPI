package weblogin

import "testing"

func TestParseSessionAccessToken(t *testing.T) {
	body := []byte(`{"accessToken":"AT","user":{"id":"u-1"},"account":{"planType":"plus"}}`)
	s, err := parseSession(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessToken != "AT" || s.AccountID != "u-1" || s.PlanType != "plus" {
		t.Fatalf("bad parse: %+v", s)
	}
}
