package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterIncludesFillFirstSwitchFields(t *testing.T) {
	entry := &log.Entry{
		Logger:  log.New(),
		Time:    time.Date(2026, time.April, 24, 5, 25, 49, 0, time.UTC),
		Level:   log.InfoLevel,
		Message: "fill-first auth switched",
		Data: log.Fields{
			"request_id":         "ca290cd9",
			"provider":           "codex",
			"model":              "gpt-5.4",
			"previous_auth_id":   "auth-prev",
			"selected_auth_id":   "auth-next",
			"selection_attempt":  2,
			"request_retry":      true,
			"selection_snapshot": "auth_id=auth-prev | auth_id=auth-next",
		},
	}

	output, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatalf("Format() error = %v", err)
	}

	got := string(output)
	for _, want := range []string{
		"fill-first auth switched",
		"provider=codex",
		"model=gpt-5.4",
		"previous_auth_id=auth-prev",
		"selected_auth_id=auth-next",
		"selection_attempt=2",
		"request_retry=true",
		"selection_snapshot=auth_id=auth-prev | auth_id=auth-next",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected formatted log to contain %q, got %q", want, got)
		}
	}
}
