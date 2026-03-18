package auth

import (
	"testing"
	"time"
)

func TestUpdateAggregatedAvailability_UnavailableWithoutNextRetryDoesNotBlockAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusError,
				Unavailable: true,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if auth.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false")
	}
	if !auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = %v, want zero", auth.NextRetryAfter)
	}
}

func TestUpdateAggregatedAvailability_FutureNextRetryBlocksAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	next := now.Add(5 * time.Minute)
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if !auth.Unavailable {
		t.Fatalf("auth.Unavailable = false, want true")
	}
	if auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = zero, want %v", next)
	}
	if auth.NextRetryAfter.Sub(next) > time.Second || next.Sub(auth.NextRetryAfter) > time.Second {
		t.Fatalf("auth.NextRetryAfter = %v, want %v", auth.NextRetryAfter, next)
	}
}

func TestModelNotFoundFromError_ExtractsRequestedModel(t *testing.T) {
	t.Parallel()

	err := &Error{HTTPStatus: 404, Message: "HTTP 404 invalid_request_error: Model not found gpt-5.2"}
	got, ok := modelNotFoundFromError(err, "")
	if !ok {
		t.Fatalf("modelNotFoundFromError() ok = false, want true")
	}
	if got != "gpt-5.2" {
		t.Fatalf("modelNotFoundFromError() = %q, want %q", got, "gpt-5.2")
	}
}

func TestMergeExcludedModels_AddsModelWithoutDuplicates(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		Attributes: map[string]string{"excluded_models": "gpt-4.1,gpt-5.2"},
		Metadata:   map[string]any{"excluded_models": []string{"gpt-4.1", "gpt-5.2"}},
	}
	if changed := mergeExcludedModels(auth, "gpt-5.2"); changed {
		t.Fatalf("mergeExcludedModels() changed = true, want false")
	}
	if changed := mergeExcludedModels(auth, "gpt-5.2-codex"); !changed {
		t.Fatalf("mergeExcludedModels() changed = false, want true")
	}
	if got := auth.Attributes["excluded_models"]; got != "gpt-4.1,gpt-5.2,gpt-5.2-codex" {
		t.Fatalf("excluded_models = %q", got)
	}
}
