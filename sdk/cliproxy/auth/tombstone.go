package auth

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	revokedAuthTombstoneTTL       = 24 * time.Hour
	banRecordCacheRefreshInterval = 30 * time.Second
)

type revokedAuthTombstone struct {
	Reason    string
	ExpiresAt time.Time
}

type banRecordCacheEntry struct {
	LoadedAt time.Time
	Names    map[string]time.Time
}

var (
	revokedAuthTombstonesMu sync.Mutex
	revokedAuthTombstones   = make(map[string]revokedAuthTombstone)
	banRecordCache          = make(map[string]banRecordCacheEntry)
)

func RegisterRevokedAuthTombstone(auth *Auth, reason string, now time.Time) {
	if auth == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	keys := revokedAuthTombstoneKeys(auth)
	if len(keys) == 0 {
		return
	}

	entry := revokedAuthTombstone{
		Reason:    strings.TrimSpace(reason),
		ExpiresAt: now.Add(revokedAuthTombstoneTTL),
	}

	revokedAuthTombstonesMu.Lock()
	defer revokedAuthTombstonesMu.Unlock()
	pruneExpiredRevokedAuthTombstonesLocked(now)
	for _, key := range keys {
		revokedAuthTombstones[key] = entry
	}
}

func HasRevokedAuthTombstone(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	if hasRevokedAuthTombstoneMemory(auth, now) {
		return true
	}

	authDir := banRecordAuthDir(auth)
	name := banRecordName(auth)
	if authDir == "" || name == "" {
		return false
	}

	bannedAt, ok := lookupRecentBanRecord(authDir, name, now)
	if !ok {
		return false
	}
	RegisterRevokedAuthTombstone(auth, "ban_record", bannedAt)
	return true
}

func hasRevokedAuthTombstoneMemory(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return hasRevokedAuthTombstoneKeys(revokedAuthTombstoneKeys(auth), now)
}

func HasRevokedAuthTombstoneForPath(path string, now time.Time) bool {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false
	}
	auth := &Auth{
		ID:       filepath.Base(trimmed),
		FileName: filepath.Base(trimmed),
		Attributes: map[string]string{
			"path": trimmed,
		},
	}
	return HasRevokedAuthTombstone(auth, now)
}

func revokedAuthTombstoneKeys(auth *Auth) []string {
	if auth == nil {
		return nil
	}
	keys := make([]string, 0, 4)
	appendKey := func(raw string) {
		if key := normalizeRevokedAuthTombstoneKey(raw); key != "" {
			keys = append(keys, key)
		}
	}

	appendKey(auth.ID)
	appendKey(auth.FileName)
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			appendKey(path)
			appendKey(filepath.Base(path))
		}
	}
	return uniqueStrings(keys)
}

func hasRevokedAuthTombstoneKeys(keys []string, now time.Time) bool {
	if len(keys) == 0 {
		return false
	}
	revokedAuthTombstonesMu.Lock()
	defer revokedAuthTombstonesMu.Unlock()
	pruneExpiredRevokedAuthTombstonesLocked(now)
	for _, key := range keys {
		if _, ok := revokedAuthTombstones[key]; ok {
			return true
		}
	}
	return false
}

func lookupRecentBanRecord(authDir, name string, now time.Time) (time.Time, bool) {
	authDir = strings.TrimSpace(authDir)
	name = strings.TrimSpace(filepath.Base(name))
	if authDir == "" || name == "" {
		return time.Time{}, false
	}

	revokedAuthTombstonesMu.Lock()
	entry, ok := banRecordCache[authDir]
	if !ok || now.Sub(entry.LoadedAt) >= banRecordCacheRefreshInterval {
		entry = loadBanRecordCacheEntry(authDir, now)
		banRecordCache[authDir] = entry
	}
	revokedAuthTombstonesMu.Unlock()

	bannedAt, ok := entry.Names[name]
	if !ok {
		return time.Time{}, false
	}
	if now.Sub(bannedAt) > revokedAuthTombstoneTTL {
		return time.Time{}, false
	}
	return bannedAt, true
}

func loadBanRecordCacheEntry(authDir string, now time.Time) banRecordCacheEntry {
	names := make(map[string]time.Time)
	days := []time.Time{now}
	if previousDay := now.Add(-24 * time.Hour); previousDay.Format("2006-01-02") != now.Format("2006-01-02") {
		days = append(days, previousDay)
	}
	for _, day := range days {
		records, err := LoadBanRecordsForDay(authDir, day)
		if err != nil {
			continue
		}
		for _, record := range records {
			key := strings.TrimSpace(filepath.Base(record.Name))
			if key == "" {
				continue
			}
			if existing, ok := names[key]; ok && existing.After(record.BannedAt) {
				continue
			}
			names[key] = record.BannedAt
		}
	}
	return banRecordCacheEntry{
		LoadedAt: now,
		Names:    names,
	}
}

func pruneExpiredRevokedAuthTombstonesLocked(now time.Time) {
	for key, entry := range revokedAuthTombstones {
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			delete(revokedAuthTombstones, key)
		}
	}
	for key, entry := range banRecordCache {
		if now.Sub(entry.LoadedAt) >= 2*banRecordCacheRefreshInterval {
			delete(banRecordCache, key)
		}
	}
}

func normalizeRevokedAuthTombstoneKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	if runtime.GOOS == "windows" {
		cleaned = strings.TrimPrefix(cleaned, `\\?\`)
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
