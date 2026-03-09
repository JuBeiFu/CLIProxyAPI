package auth

import (
	"testing"
	"time"
)

func TestIsAuthBlockedForModel_GlobalQuotaBlocksAllModels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	recoverAt := now.Add(2 * time.Hour)
	auth := &Auth{
		ID: "auth-1",
		Quota: QuotaState{
			Exceeded:      true,
			NextRecoverAt: recoverAt,
		},
		// No ModelStates entry for the queried model: this is the cross-model case
		// that previously allowed the auth to be selected and fail repeatedly.
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {
				Status:         StatusActive,
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: recoverAt,
				},
			},
		},
	}

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked {
		t.Fatalf("expected auth to be blocked by global quota")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("expected cooldown reason, got %v", reason)
	}
	if next.IsZero() || !next.Equal(recoverAt) {
		t.Fatalf("expected next=%v, got %v", recoverAt, next)
	}
}

