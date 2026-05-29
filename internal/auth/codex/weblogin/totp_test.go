package weblogin

import "testing"

func TestTOTPRFCVector(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	code, err := totpAt(secret, 59)
	if err != nil {
		t.Fatal(err)
	}
	if code != "94287082" {
		t.Fatalf("got %s want 94287082", code)
	}
}
