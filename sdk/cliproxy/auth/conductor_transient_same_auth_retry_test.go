package auth

import (
	"net/http"
	"testing"
)

func TestTransientSameAuthRetryBudget_DefaultsForStreamSetupTimeout(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusRequestTimeout,
		Code:       "stream_connect_timeout",
		Message:    "upstream stream setup timed out after 10s",
	}
	if got := transientSameAuthRetryBudget(&Auth{}, err); got != 0 {
		t.Fatalf("transientSameAuthRetryBudget(timeout) = %d, want %d", got, 0)
	}
}

func TestTransientSameAuthRetryBudget_DefaultsForStreamDisconnect(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusRequestTimeout,
		Message:    "stream disconnected before completion: stream closed before response.completed",
	}
	if got := transientSameAuthRetryBudget(&Auth{}, err); got != defaultTransientSameAuthRetry {
		t.Fatalf("transientSameAuthRetryBudget(disconnect) = %d, want %d", got, defaultTransientSameAuthRetry)
	}
}

func TestTransientSameAuthRetryBudget_DefaultRemainsZeroForGenericErrors(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusRequestTimeout,
		Code:       "execute_timeout",
		Message:    "execute attempt exceeded timeout",
	}
	if got := transientSameAuthRetryBudget(&Auth{}, err); got != 0 {
		t.Fatalf("transientSameAuthRetryBudget(generic) = %d, want 0", got)
	}
}

func TestTransientSameAuthRetryBudget_OverrideStillWins(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{"same_auth_retry": float64(1)}}
	err := &Error{
		HTTPStatus: http.StatusRequestTimeout,
		Code:       "stream_connect_timeout",
		Message:    "upstream stream setup timed out after 10s",
	}
	if got := transientSameAuthRetryBudget(auth, err); got != 1 {
		t.Fatalf("transientSameAuthRetryBudget(override) = %d, want 1", got)
	}
}

func TestTransientSameAuthRetryBudget_DefaultsForCompactExecuteTimeout(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusRequestTimeout,
		Code:       "execute_timeout",
		Message:    `Post "https://chatgpt.com/backend-api/codex/responses/compact": context deadline exceeded`,
	}
	if got := transientSameAuthRetryBudget(&Auth{}, err); got != 0 {
		t.Fatalf("transientSameAuthRetryBudget(compact execute timeout) = %d, want %d", got, 0)
	}
}
