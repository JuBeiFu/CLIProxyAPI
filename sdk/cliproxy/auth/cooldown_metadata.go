package auth

const (
	// Persisted metadata keys written into auth JSON files so cooldown state can
	// survive restarts and be inspected externally.
	metadataCooldownUntilKey  = "cliproxy_cooldown_until"
	metadataCooldownReasonKey = "cliproxy_cooldown_reason"
)

const (
	cooldownReasonQuota = "quota"
)

