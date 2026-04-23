package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// CODEX_GPTDRAW_PER_AUTH_LIMIT sets the per-auth daily cap on gpt-draw-*
	// calls. Zero or negative disables the check.
	gptDrawPerAuthLimitEnv     = "CODEX_GPTDRAW_PER_AUTH_LIMIT"
	gptDrawPerAuthLimitDefault = 20

	// CODEX_GPTDRAW_QUOTA_FILE overrides the on-disk path of the quota store.
	// Defaults to /CLIProxyAPI/logs/gpt_draw_quota.json, which sits inside the
	// mounted logs volume so it survives container replacement.
	gptDrawQuotaFileEnv     = "CODEX_GPTDRAW_QUOTA_FILE"
	gptDrawQuotaFileDefault = "/CLIProxyAPI/logs/gpt_draw_quota.json"
)

// GptDrawUsageEntry is the persisted form of a single auth's gpt-draw counter.
type GptDrawUsageEntry struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type gptDrawQuotaStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]*GptDrawUsageEntry
	loaded  bool
}

var globalGptDrawQuota = &gptDrawQuotaStore{
	entries: make(map[string]*GptDrawUsageEntry),
}

// GptDrawPerAuthLimit returns the configured daily per-auth limit. When
// unset or non-positive, it returns 0 meaning "no limit".
func GptDrawPerAuthLimit() int {
	if v := strings.TrimSpace(os.Getenv(gptDrawPerAuthLimitEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return gptDrawPerAuthLimitDefault
}

func gptDrawQuotaFile() string {
	if v := strings.TrimSpace(os.Getenv(gptDrawQuotaFileEnv)); v != "" {
		return v
	}
	return gptDrawQuotaFileDefault
}

// utcToday returns today's date in UTC as "2006-01-02".
func utcToday() string {
	return time.Now().UTC().Format("2006-01-02")
}

func (s *gptDrawQuotaStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.path = gptDrawQuotaFile()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("gpt-draw quota: read %s failed: %v (starting empty)", s.path, err)
		}
		return
	}
	if len(data) == 0 {
		return
	}
	var decoded map[string]*GptDrawUsageEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		log.Warnf("gpt-draw quota: parse %s failed: %v (starting empty)", s.path, err)
		return
	}
	for id, entry := range decoded {
		if entry == nil {
			continue
		}
		s.entries[id] = entry
	}
}

func (s *gptDrawQuotaStore) persistLocked() {
	if s.path == "" {
		s.path = gptDrawQuotaFile()
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Warnf("gpt-draw quota: mkdir %s failed: %v", filepath.Dir(s.path), err)
		return
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		log.Warnf("gpt-draw quota: marshal failed: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Warnf("gpt-draw quota: write %s failed: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Warnf("gpt-draw quota: rename %s -> %s failed: %v", tmp, s.path, err)
	}
}

// CheckAndIncrement atomically checks whether authID still has gpt-draw quota
// for today and, if so, increments the counter and persists.
// Returns (allowed, usedAfter, limit).
// When limit == 0, no check is performed and (true, 0, 0) is returned.
func (s *gptDrawQuotaStore) CheckAndIncrement(authID string) (bool, int, int) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return true, 0, 0
	}
	limit := GptDrawPerAuthLimit()
	if limit <= 0 {
		return true, 0, 0
	}
	today := utcToday()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	entry, ok := s.entries[authID]
	if !ok || entry == nil || entry.Date != today {
		entry = &GptDrawUsageEntry{Date: today, Count: 0}
		s.entries[authID] = entry
	}
	if entry.Count >= limit {
		return false, entry.Count, limit
	}
	entry.Count++
	s.persistLocked()
	return true, entry.Count, limit
}

// Snapshot returns a copy of the current per-auth counters for inspection.
// Also returns the configured limit.
func (s *gptDrawQuotaStore) Snapshot() (map[string]GptDrawUsageEntry, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	out := make(map[string]GptDrawUsageEntry, len(s.entries))
	today := utcToday()
	for id, entry := range s.entries {
		if entry == nil {
			continue
		}
		// Surface stale entries as zero for today so callers see current-day usage.
		if entry.Date != today {
			out[id] = GptDrawUsageEntry{Date: today, Count: 0}
			continue
		}
		out[id] = *entry
	}
	return out, GptDrawPerAuthLimit()
}

// QuotaExhaustedError is emitted to the scheduler when a specific auth has
// used up its daily gpt-draw allowance; callers format the message directly.
func gptDrawQuotaErrMsg(authID string, used, limit int) string {
	return fmt.Sprintf("gpt-draw daily quota exhausted for auth %s (%d/%d); reset at next UTC midnight", authID, used, limit)
}

// GptDrawQuotaSnapshot exposes the current per-auth counters plus the
// configured limit. Used by management endpoints.
func GptDrawQuotaSnapshot() (map[string]GptDrawUsageEntry, int) {
	return globalGptDrawQuota.Snapshot()
}
