package auth

import "testing"

func clearRevokedAuthTombstoneForTest(t *testing.T, auths ...*Auth) {
	t.Helper()

	revokedAuthTombstonesMu.Lock()
	defer revokedAuthTombstonesMu.Unlock()

	if len(auths) == 0 {
		revokedAuthTombstones = make(map[string]revokedAuthTombstone)
		banRecordCache = make(map[string]banRecordCacheEntry)
		return
	}

	for _, auth := range auths {
		for _, key := range revokedAuthTombstoneKeys(auth) {
			delete(revokedAuthTombstones, key)
		}
		if authDir := banRecordAuthDir(auth); authDir != "" {
			delete(banRecordCache, authDir)
		}
	}
}
