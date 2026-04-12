package proxypool

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
)

type ProbeResult struct {
	Healthy    bool          `json:"healthy"`
	StatusCode int           `json:"status_code,omitempty"`
	Latency    time.Duration `json:"-"`
	Error      string        `json:"error,omitempty"`
	CheckedAt  time.Time     `json:"checked_at,omitempty"`
}

type EntryStatus struct {
	Name       string     `json:"name"`
	URL        string     `json:"url"`
	Disabled   bool       `json:"disabled,omitempty"`
	Checked    bool       `json:"checked"`
	Healthy    bool       `json:"healthy"`
	StatusCode int        `json:"status_code,omitempty"`
	Latency    string     `json:"latency,omitempty"`
	Error      string     `json:"error,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
}

type PoolStatus struct {
	Name                string        `json:"name"`
	FallbackToDirect    bool          `json:"fallback_to_direct"`
	HealthCheckURL      string        `json:"health_check_url,omitempty"`
	HealthCheckMethod   string        `json:"health_check_method,omitempty"`
	HealthCheckTimeoutS int           `json:"health_check_timeout_seconds,omitempty"`
	ExpectedStatusCodes []int         `json:"expected_status_codes,omitempty"`
	LastCheckedAt       *time.Time    `json:"last_checked_at,omitempty"`
	Entries             []EntryStatus `json:"entries"`
}

type probeClientKey struct {
	proxyURL string
	timeout  time.Duration
}

type HealthManager struct {
	mu             sync.RWMutex
	entries        map[string]map[string]ProbeResult
	probeClientMu  sync.Mutex
	probeClients   map[probeClientKey]*http.Client
	probeEntryFunc func(context.Context, config.ProxyPool, config.ProxyPoolEntry) ProbeResult
}

var defaultHealthManager = NewHealthManager()

func DefaultHealthManager() *HealthManager {
	return defaultHealthManager
}

func NewHealthManager() *HealthManager {
	return &HealthManager{
		entries:      make(map[string]map[string]ProbeResult),
		probeClients: make(map[probeClientKey]*http.Client),
	}
}

func StartBackgroundRefresh(ctx context.Context, cfgProvider func() *config.Config, tick time.Duration) {
	if ctx == nil || cfgProvider == nil {
		return
	}
	if tick <= 0 {
		tick = 5 * time.Second
	}

	go func() {
		DefaultHealthManager().RunDueChecks(ctx, cfgProvider())

		ticker := time.NewTicker(tick)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				DefaultHealthManager().RunDueChecks(ctx, cfgProvider())
			}
		}
	}()
}

func (m *HealthManager) StoreResult(poolName, entryName string, result ProbeResult) {
	if m == nil {
		return
	}
	poolKey := normalizePoolName(poolName)
	entryKey := normalizeEntryName(entryName)
	if poolKey == "" || entryKey == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.entries == nil {
		m.entries = make(map[string]map[string]ProbeResult)
	}
	if _, ok := m.entries[poolKey]; !ok {
		m.entries[poolKey] = make(map[string]ProbeResult)
	}
	m.entries[poolKey][entryKey] = result
}

func (m *HealthManager) Result(poolName, entryName string) (ProbeResult, bool) {
	if m == nil {
		return ProbeResult{}, false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	poolEntries, ok := m.entries[normalizePoolName(poolName)]
	if !ok {
		return ProbeResult{}, false
	}
	result, ok := poolEntries[normalizeEntryName(entryName)]
	return result, ok
}

func (m *HealthManager) Snapshot() map[string]map[string]ProbeResult {
	if m == nil {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.entries) == 0 {
		return nil
	}

	snapshot := make(map[string]map[string]ProbeResult, len(m.entries))
	for poolName, entryResults := range m.entries {
		entryCopy := make(map[string]ProbeResult, len(entryResults))
		for entryName, result := range entryResults {
			entryCopy[entryName] = result
		}
		snapshot[poolName] = entryCopy
	}
	return snapshot
}

func (m *HealthManager) IsUsable(poolName, entryName string) bool {
	result, ok := m.Result(poolName, entryName)
	if !ok || result.CheckedAt.IsZero() {
		return true
	}
	return result.Healthy
}

func (m *HealthManager) PoolStatuses(pools []config.ProxyPool, targetName string) []PoolStatus {
	if len(pools) == 0 {
		return nil
	}

	results := make([]PoolStatus, 0, len(pools))
	for _, pool := range pools {
		if targetName != "" && !strings.EqualFold(strings.TrimSpace(pool.Name), strings.TrimSpace(targetName)) {
			continue
		}
		results = append(results, m.poolStatus(pool))
	}
	return results
}

func (m *HealthManager) CheckPools(ctx context.Context, pools []config.ProxyPool, targetName string) []PoolStatus {
	if len(pools) == 0 {
		return nil
	}

	filtered := make([]config.ProxyPool, 0, len(pools))
	for _, pool := range pools {
		if targetName != "" && !strings.EqualFold(strings.TrimSpace(pool.Name), strings.TrimSpace(targetName)) {
			continue
		}
		filtered = append(filtered, pool)
	}
	if len(filtered) == 0 {
		return nil
	}

	results := make([]PoolStatus, len(filtered))
	var wg sync.WaitGroup
	for i, pool := range filtered {
		wg.Add(1)
		go func(index int, currentPool config.ProxyPool) {
			defer wg.Done()
			results[index] = m.CheckPool(ctx, currentPool)
		}(i, pool)
	}
	wg.Wait()
	return results
}

func (m *HealthManager) CheckPool(ctx context.Context, pool config.ProxyPool) PoolStatus {
	status := m.poolStatus(pool)
	lastCheckedAt := time.Time{}
	type probeOutcome struct {
		index  int
		entry  config.ProxyPoolEntry
		result ProbeResult
	}

	outcomes := make(chan probeOutcome, len(pool.Entries))
	var wg sync.WaitGroup

	for index, entry := range pool.Entries {
		if entry.Disabled || strings.TrimSpace(entry.URL) == "" {
			status.Entries[index].Disabled = entry.Disabled
			continue
		}
		wg.Add(1)
		go func(idx int, currentEntry config.ProxyPoolEntry) {
			defer wg.Done()
			outcomes <- probeOutcome{
				index:  idx,
				entry:  currentEntry,
				result: m.probeEntry(ctx, pool, currentEntry),
			}
		}(index, entry)
	}
	wg.Wait()
	close(outcomes)

	for outcome := range outcomes {
		m.StoreResult(pool.Name, outcome.entry.Name, outcome.result)
		status.Entries[outcome.index] = entryStatusFromProbe(outcome.entry, outcome.result)
		if outcome.result.CheckedAt.After(lastCheckedAt) {
			lastCheckedAt = outcome.result.CheckedAt
		}
	}
	if !lastCheckedAt.IsZero() {
		status.LastCheckedAt = &lastCheckedAt
	}
	return status
}

func (m *HealthManager) RunDueChecks(ctx context.Context, cfg *config.Config) {
	if m == nil || cfg == nil || len(cfg.ProxyPools) == 0 {
		return
	}

	now := time.Now()
	for _, pool := range cfg.ProxyPools {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}

		intervalSeconds := pool.HealthCheckIntervalSeconds
		if intervalSeconds <= 0 {
			continue
		}
		if last := m.lastCheckedAt(pool.Name); !last.IsZero() && now.Sub(last) < time.Duration(intervalSeconds)*time.Second {
			continue
		}
		m.CheckPool(ctx, pool)
	}
}

func (m *HealthManager) poolStatus(pool config.ProxyPool) PoolStatus {
	status := PoolStatus{
		Name:                pool.Name,
		FallbackToDirect:    pool.FallbackToDirect,
		HealthCheckURL:      healthCheckURLForPool(pool),
		HealthCheckMethod:   healthCheckMethodForPool(pool),
		HealthCheckTimeoutS: healthCheckTimeoutSecondsForPool(pool),
		ExpectedStatusCodes: expectedStatusCodesForPool(pool),
		Entries:             make([]EntryStatus, 0, len(pool.Entries)),
	}

	lastCheckedAt := time.Time{}
	for _, entry := range pool.Entries {
		statusEntry := EntryStatus{
			Name:     entry.Name,
			URL:      entry.URL,
			Disabled: entry.Disabled,
		}
		if result, ok := m.Result(pool.Name, entry.Name); ok && !result.CheckedAt.IsZero() {
			statusEntry = entryStatusFromProbe(entry, result)
			if result.CheckedAt.After(lastCheckedAt) {
				lastCheckedAt = result.CheckedAt
			}
		}
		status.Entries = append(status.Entries, statusEntry)
	}

	if !lastCheckedAt.IsZero() {
		status.LastCheckedAt = &lastCheckedAt
	}
	return status
}

func (m *HealthManager) lastCheckedAt(poolName string) time.Time {
	snapshot := m.Snapshot()
	if len(snapshot) == 0 {
		return time.Time{}
	}

	var last time.Time
	for _, result := range snapshot[normalizePoolName(poolName)] {
		if result.CheckedAt.After(last) {
			last = result.CheckedAt
		}
	}
	return last
}

func entryStatusFromProbe(entry config.ProxyPoolEntry, result ProbeResult) EntryStatus {
	status := EntryStatus{
		Name:       entry.Name,
		URL:        entry.URL,
		Disabled:   entry.Disabled,
		Checked:    !result.CheckedAt.IsZero(),
		Healthy:    result.Healthy,
		StatusCode: result.StatusCode,
		Error:      result.Error,
	}
	if result.Latency > 0 {
		status.Latency = result.Latency.Round(time.Millisecond).String()
	}
	if !result.CheckedAt.IsZero() {
		checkedAt := result.CheckedAt
		status.CheckedAt = &checkedAt
	}
	return status
}

func (m *HealthManager) probeEntry(ctx context.Context, pool config.ProxyPool, entry config.ProxyPoolEntry) ProbeResult {
	if m != nil && m.probeEntryFunc != nil {
		return m.probeEntryFunc(ctx, pool, entry)
	}
	return m.probePoolEntry(ctx, pool, entry)
}

func (m *HealthManager) probeClientForEntry(pool config.ProxyPool, entry config.ProxyPoolEntry) (*http.Client, error) {
	if m == nil {
		return nil, fmt.Errorf("health manager is nil")
	}

	timeout := time.Duration(healthCheckTimeoutSecondsForPool(pool)) * time.Second
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	key := probeClientKey{
		proxyURL: strings.TrimSpace(entry.URL),
		timeout:  timeout,
	}

	m.probeClientMu.Lock()
	defer m.probeClientMu.Unlock()

	if client, ok := m.probeClients[key]; ok && client != nil {
		return client, nil
	}

	transport, _, errBuild := proxyutil.BuildHTTPTransport(entry.URL)
	if errBuild != nil || transport == nil {
		if errBuild != nil {
			return nil, errBuild
		}
		return nil, fmt.Errorf("failed to build proxy transport")
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	m.probeClients[key] = client
	return client, nil
}

func (m *HealthManager) probePoolEntry(ctx context.Context, pool config.ProxyPool, entry config.ProxyPoolEntry) ProbeResult {
	url := healthCheckURLForPool(pool)
	method := healthCheckMethodForPool(pool)

	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return ProbeResult{Error: err.Error(), CheckedAt: time.Now()}
	}

	client, err := m.probeClientForEntry(pool, entry)
	if err != nil {
		return ProbeResult{Error: err.Error(), CheckedAt: time.Now()}
	}
	started := time.Now()
	resp, err := client.Do(req)
	checkedAt := time.Now()
	if err != nil {
		return ProbeResult{
			Error:     err.Error(),
			Latency:   checkedAt.Sub(started),
			CheckedAt: checkedAt,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	statusCode := resp.StatusCode
	for _, expected := range expectedStatusCodesForPool(pool) {
		if statusCode == expected {
			return ProbeResult{
				Healthy:    true,
				StatusCode: statusCode,
				Latency:    checkedAt.Sub(started),
				CheckedAt:  checkedAt,
			}
		}
	}
	return ProbeResult{
		StatusCode: statusCode,
		Latency:    checkedAt.Sub(started),
		Error:      fmt.Sprintf("unexpected status %d", statusCode),
		CheckedAt:  checkedAt,
	}
}

func healthCheckURLForPool(pool config.ProxyPool) string {
	if url := strings.TrimSpace(pool.HealthCheckURL); url != "" {
		return url
	}
	return "https://www.gstatic.com/generate_204"
}

func healthCheckMethodForPool(pool config.ProxyPool) string {
	if method := strings.ToUpper(strings.TrimSpace(pool.HealthCheckMethod)); method != "" {
		return method
	}
	return http.MethodGet
}

func healthCheckTimeoutSecondsForPool(pool config.ProxyPool) int {
	if pool.HealthCheckTimeoutSeconds > 0 {
		return pool.HealthCheckTimeoutSeconds
	}
	return 8
}

func expectedStatusCodesForPool(pool config.ProxyPool) []int {
	if len(pool.ExpectedStatusCodes) > 0 {
		return append([]int(nil), pool.ExpectedStatusCodes...)
	}
	return []int{http.StatusOK, http.StatusNoContent}
}

func normalizePoolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func normalizeEntryName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
