package weblogin

import (
	"context"
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

func TestPoWSatisfiesDifficulty(t *testing.T) {
	tok := solvePoW(context.Background(), "seed123", "0", "UA/1.0", "https://sdk.js", "sid-1")
	if tok[:7] != "gAAAAAB" {
		t.Fatalf("expected success token, got prefix %q", tok[:7])
	}
	data := tok[7 : len(tok)-2] // strip gAAAAAB ... ~S
	if fnv1a32("seed123"+data)[:1] > "0" {
		t.Fatalf("PoW does not satisfy difficulty")
	}
}

func TestFNV1aKnown(t *testing.T) {
	if len(fnv1a32("")) != 8 {
		t.Fatalf("fnv length")
	}
}
