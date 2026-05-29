package weblogin

import "testing"

func TestExtractOTPCode(t *testing.T) {
	if got := extractOTPCode("Your ChatGPT code is 481596. It expires"); got != "481596" {
		t.Fatalf("got %q", got)
	}
	if got := extractOTPCode("no code here"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
