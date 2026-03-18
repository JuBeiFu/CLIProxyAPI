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

func TestIsAuthBlockedForModel_MetadataCooldownBlocksAllModels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	until := now.Add(90 * time.Minute)
	auth := &Auth{
		ID:       "auth-1",
		Metadata: map[string]any{metadataCooldownUntilKey: until.Unix()},
	}

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked {
		t.Fatalf("expected auth to be blocked by metadata cooldown")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("expected cooldown reason, got %v", reason)
	}
	if next.IsZero() || !next.Equal(time.Unix(until.Unix(), 0)) {
		t.Fatalf("expected next=%v, got %v", until, next)
	}
}

func TestIsAuthBlockedForModel_MetadataCooldownExpiredAllows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	until := now.Add(-5 * time.Minute)
	auth := &Auth{
		ID:       "auth-1",
		Metadata: map[string]any{metadataCooldownUntilKey: until.Unix()},
	}

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if blocked {
		t.Fatalf("expected auth not to be blocked, got reason=%v next=%v", reason, next)
	}
}

func TestIsAuthBlockedForModel_TransientCooldownBlocksAllModels(t *testing.T) {
	t.Parallel()

	now := time.Now()
	until := now.Add(45 * time.Minute)
	auth := &Auth{ID: "auth-transient", TransientCooldownUntil: until}

	blocked, reason, next := isAuthBlockedForModel(auth, "gpt-5", now)
	if !blocked {
		t.Fatalf("expected auth to be blocked by transient cooldown")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("expected cooldown reason, got %v", reason)
	}
	if next.IsZero() || !next.Equal(until) {
		t.Fatalf("expected next=%v, got %v", until, next)
	}
}
