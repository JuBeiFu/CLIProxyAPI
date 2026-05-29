package weblogin

import (
	"encoding/json"
	"testing"
)

func TestSimpleSentinelTokenShape(t *testing.T) {
	tok := buildSimpleSentinelToken("CHAL", "TURN", "login")
	var m map[string]any
	if err := json.Unmarshal([]byte(tok), &m); err != nil {
		t.Fatal(err)
	}
	if m["p"] != "" || m["c"] != "CHAL" || m["t"] != "TURN" || m["flow"] != "login" {
		t.Fatalf("bad token: %v", m)
	}
}
