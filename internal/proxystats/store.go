package proxystats

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Attempt struct {
	Timestamp           time.Time `json:"timestamp"`
	StartedAt           time.Time `json:"started_at"`
	CompletedAt         time.Time `json:"completed_at"`
	ProxyURL            string    `json:"proxy_url"`
	ProxyDisplay        string    `json:"proxy_display"`
	ProxyProfile        string    `json:"proxy_profile,omitempty"`
	SelectionSource     string    `json:"selection_source,omitempty"`
	RoutingRule         string    `json:"routing_rule,omitempty"`
	Provider            string    `json:"provider,omitempty"`
	PlanType            string    `json:"plan_type,omitempty"`
	AuthKind            string    `json:"auth_kind,omitempty"`
	AuthID              string    `json:"auth_id,omitempty"`
	AuthIndex           string    `json:"auth_index,omitempty"`
	StatusCode          int       `json:"status_code,omitempty"`
	Success             bool      `json:"success"`
	ResponseReceived    bool      `json:"response_received"`
	FirstByteDurationMs int64     `json:"first_byte_duration_ms"`
	TotalDurationMs     int64     `json:"total_duration_ms"`
	Error               string    `json:"error,omitempty"`
}

type ProxySnapshot struct {
	Key                 string           `json:"key"`
	ProxyURL            string           `json:"proxy_url"`
	ProxyDisplay        string           `json:"proxy_display"`
	ProxyProfile        string           `json:"proxy_profile,omitempty"`
	TotalAttempts       int64            `json:"total_attempts"`
	SuccessCount        int64            `json:"success_count"`
	FailureCount        int64            `json:"failure_count"`
	ResponseCount       int64            `json:"response_count"`
	TransportErrorCount int64            `json:"transport_error_count"`
	HTTPErrorCount      int64            `json:"http_error_count"`
	SuccessRate         float64          `json:"success_rate"`
	FirstByteAvgMs      float64          `json:"first_byte_avg_ms"`
	TotalDurationAvgMs  float64          `json:"total_duration_avg_ms"`
	LastUsedAt          time.Time        `json:"last_used_at"`
	LastStatusCode      int              `json:"last_status_code,omitempty"`
	LastError           string           `json:"last_error,omitempty"`
	StatusCounts        map[string]int64 `json:"status_counts,omitempty"`
	Providers           map[string]int64 `json:"providers,omitempty"`
	PlanTypes           map[string]int64 `json:"plan_types,omitempty"`
	AuthKinds           map[string]int64 `json:"auth_kinds,omitempty"`
	SelectionSources    map[string]int64 `json:"selection_sources,omitempty"`
}

type Snapshot struct {
	TotalAttempts int64           `json:"total_attempts"`
	SuccessCount  int64           `json:"success_count"`
	FailureCount  int64           `json:"failure_count"`
	ResponseCount int64           `json:"response_count"`
	Proxies       []ProxySnapshot `json:"proxies"`
	Recent        []Attempt       `json:"recent"`
}

type Store struct {
	mu sync.RWMutex

	totalAttempts int64
	successCount  int64
	failureCount  int64
	responseCount int64

	proxies   map[string]*proxyMetrics
	recent    []Attempt
	maxRecent int
}

type proxyMetrics struct {
	ProxyURL            string
	ProxyDisplay        string
	ProxyProfile        string
	TotalAttempts       int64
	SuccessCount        int64
	FailureCount        int64
	ResponseCount       int64
	TransportErrorCount int64
	HTTPErrorCount      int64
	FirstByteSumMs      int64
	FirstByteCount      int64
	TotalDurationSumMs  int64
	TotalDurationCount  int64
	LastUsedAt          time.Time
	LastStatusCode      int
	LastError           string
	StatusCounts        map[string]int64
	Providers           map[string]int64
	PlanTypes           map[string]int64
	AuthKinds           map[string]int64
	SelectionSources    map[string]int64
	LastSuccessAt       time.Time
	LastTransportFailAt time.Time
	TransportFailStreak int
}

var defaultStore = NewStore(400)

func DefaultStore() *Store { return defaultStore }

const (
	transportFailureCooldownBase = 15 * time.Second
	transportFailureCooldownMax  = 2 * time.Minute
)

func NewStore(maxRecent int) *Store {
	if maxRecent <= 0 {
		maxRecent = 200
	}
	return &Store{proxies: make(map[string]*proxyMetrics), maxRecent: maxRecent}
}

func (s *Store) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalAttempts = 0
	s.successCount = 0
	s.failureCount = 0
	s.responseCount = 0
	s.proxies = make(map[string]*proxyMetrics)
	s.recent = nil
}

func (s *Store) Record(attempt Attempt) {
	if s == nil {
		return
	}
	attempt = normalizeAttempt(attempt)
	if attempt.ProxyDisplay == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalAttempts++
	if attempt.Success {
		s.successCount++
	} else {
		s.failureCount++
	}
	if attempt.ResponseReceived {
		s.responseCount++
	}

	metric, ok := s.proxies[attempt.ProxyDisplay]
	if !ok {
		metric = &proxyMetrics{
			ProxyURL:         attempt.ProxyURL,
			ProxyDisplay:     attempt.ProxyDisplay,
			ProxyProfile:     attempt.ProxyProfile,
			StatusCounts:     make(map[string]int64),
			Providers:        make(map[string]int64),
			PlanTypes:        make(map[string]int64),
			AuthKinds:        make(map[string]int64),
			SelectionSources: make(map[string]int64),
		}
		s.proxies[attempt.ProxyDisplay] = metric
	}

	metric.TotalAttempts++
	if attempt.Success {
		metric.SuccessCount++
	} else {
		metric.FailureCount++
	}
	if attempt.ResponseReceived {
		metric.ResponseCount++
		if !attempt.CompletedAt.IsZero() {
			metric.LastSuccessAt = attempt.CompletedAt
		}
		metric.TransportFailStreak = 0
	} else {
		metric.TransportErrorCount++
		if !attempt.CompletedAt.IsZero() {
			metric.LastTransportFailAt = attempt.CompletedAt
		}
		metric.TransportFailStreak++
	}
	if attempt.ResponseReceived && attempt.StatusCode >= 400 {
		metric.HTTPErrorCount++
	}
	if attempt.FirstByteDurationMs > 0 {
		metric.FirstByteSumMs += attempt.FirstByteDurationMs
		metric.FirstByteCount++
	}
	if attempt.TotalDurationMs > 0 {
		metric.TotalDurationSumMs += attempt.TotalDurationMs
		metric.TotalDurationCount++
	}
	if !attempt.CompletedAt.IsZero() {
		metric.LastUsedAt = attempt.CompletedAt
	}
	metric.LastStatusCode = attempt.StatusCode
	metric.LastError = attempt.Error
	incrementCounter(metric.StatusCounts, strconv.Itoa(attempt.StatusCode), attempt.StatusCode > 0)
	incrementCounter(metric.Providers, attempt.Provider, attempt.Provider != "")
	incrementCounter(metric.PlanTypes, attempt.PlanType, attempt.PlanType != "")
	incrementCounter(metric.AuthKinds, attempt.AuthKind, attempt.AuthKind != "")
	incrementCounter(metric.SelectionSources, attempt.SelectionSource, attempt.SelectionSource != "")

	if s.maxRecent > 0 {
		s.recent = append(s.recent, attempt)
		if len(s.recent) > s.maxRecent {
			s.recent = append([]Attempt(nil), s.recent[len(s.recent)-s.maxRecent:]...)
		}
	}
}

func (s *Store) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	snapshot := Snapshot{
		TotalAttempts: s.totalAttempts,
		SuccessCount:  s.successCount,
		FailureCount:  s.failureCount,
		ResponseCount: s.responseCount,
		Proxies:       make([]ProxySnapshot, 0, len(s.proxies)),
		Recent:        append([]Attempt(nil), s.recent...),
	}

	for _, metric := range s.proxies {
		if metric == nil {
			continue
		}
		item := ProxySnapshot{
			Key:                 metric.ProxyDisplay,
			ProxyURL:            metric.ProxyURL,
			ProxyDisplay:        metric.ProxyDisplay,
			ProxyProfile:        metric.ProxyProfile,
			TotalAttempts:       metric.TotalAttempts,
			SuccessCount:        metric.SuccessCount,
			FailureCount:        metric.FailureCount,
			ResponseCount:       metric.ResponseCount,
			TransportErrorCount: metric.TransportErrorCount,
			HTTPErrorCount:      metric.HTTPErrorCount,
			LastUsedAt:          metric.LastUsedAt,
			LastStatusCode:      metric.LastStatusCode,
			LastError:           metric.LastError,
			StatusCounts:        cloneCounters(metric.StatusCounts),
			Providers:           cloneCounters(metric.Providers),
			PlanTypes:           cloneCounters(metric.PlanTypes),
			AuthKinds:           cloneCounters(metric.AuthKinds),
			SelectionSources:    cloneCounters(metric.SelectionSources),
		}
		if item.TotalAttempts > 0 {
			item.SuccessRate = float64(item.SuccessCount) / float64(item.TotalAttempts)
		}
		if metric.FirstByteCount > 0 {
			item.FirstByteAvgMs = float64(metric.FirstByteSumMs) / float64(metric.FirstByteCount)
		}
		if metric.TotalDurationCount > 0 {
			item.TotalDurationAvgMs = float64(metric.TotalDurationSumMs) / float64(metric.TotalDurationCount)
		}
		snapshot.Proxies = append(snapshot.Proxies, item)
	}

	sort.Slice(snapshot.Proxies, func(i, j int) bool {
		if snapshot.Proxies[i].TotalAttempts == snapshot.Proxies[j].TotalAttempts {
			return snapshot.Proxies[i].ProxyDisplay < snapshot.Proxies[j].ProxyDisplay
		}
		return snapshot.Proxies[i].TotalAttempts > snapshot.Proxies[j].TotalAttempts
	})
	return snapshot
}

// PreferredProxyOrder returns the proxy list reordered by current health, while
// still rotating healthy peers to avoid hot-spotting a single proxy.
func (s *Store) PreferredProxyOrder(proxyURLs []string, seed uint64, now time.Time) []string {
	if len(proxyURLs) <= 1 {
		return append([]string(nil), proxyURLs...)
	}
	type candidate struct {
		index         int
		proxyURL      string
		cooldownUntil time.Time
		failStreak    int
		successRate   float64
		totalAttempts int64
	}

	candidates := make([]candidate, 0, len(proxyURLs))
	if now.IsZero() {
		now = time.Now()
	}

	s.mu.RLock()
	for idx, raw := range proxyURLs {
		display := strings.TrimSpace(RedactProxyURL(raw))
		item := candidate{
			index:    idx,
			proxyURL: raw,
		}
		if metric := s.proxies[display]; metric != nil {
			item.failStreak = metric.TransportFailStreak
			item.totalAttempts = metric.TotalAttempts
			if metric.TotalAttempts > 0 {
				item.successRate = float64(metric.SuccessCount) / float64(metric.TotalAttempts)
			}
			item.cooldownUntil = transportFailureCooldownUntil(metric)
		}
		candidates = append(candidates, item)
	}
	s.mu.RUnlock()

	sort.SliceStable(candidates, func(i, j int) bool {
		iCooling := candidates[i].cooldownUntil.After(now)
		jCooling := candidates[j].cooldownUntil.After(now)
		if iCooling != jCooling {
			return !iCooling
		}
		if iCooling && jCooling && !candidates[i].cooldownUntil.Equal(candidates[j].cooldownUntil) {
			return candidates[i].cooldownUntil.Before(candidates[j].cooldownUntil)
		}
		if candidates[i].failStreak != candidates[j].failStreak {
			return candidates[i].failStreak < candidates[j].failStreak
		}
		if candidates[i].successRate != candidates[j].successRate {
			return candidates[i].successRate > candidates[j].successRate
		}
		if candidates[i].totalAttempts != candidates[j].totalAttempts {
			return candidates[i].totalAttempts < candidates[j].totalAttempts
		}
		return candidates[i].index < candidates[j].index
	})

	healthy := make([]string, 0, len(candidates))
	cooling := make([]string, 0, len(candidates))
	for _, item := range candidates {
		if item.cooldownUntil.After(now) {
			cooling = append(cooling, item.proxyURL)
			continue
		}
		healthy = append(healthy, item.proxyURL)
	}
	healthy = rotateProxyURLs(healthy, seed)
	cooling = rotateProxyURLs(cooling, seed)
	ordered := make([]string, 0, len(proxyURLs))
	ordered = append(ordered, healthy...)
	ordered = append(ordered, cooling...)
	return ordered
}

func normalizeAttempt(attempt Attempt) Attempt {
	attempt.ProxyURL = strings.TrimSpace(RedactProxyURL(attempt.ProxyURL))
	attempt.ProxyDisplay = strings.TrimSpace(attempt.ProxyDisplay)
	if attempt.ProxyDisplay == "" {
		attempt.ProxyDisplay = attempt.ProxyURL
	}
	attempt.ProxyProfile = strings.TrimSpace(attempt.ProxyProfile)
	attempt.SelectionSource = normalizeIdentifier(attempt.SelectionSource)
	attempt.RoutingRule = strings.TrimSpace(attempt.RoutingRule)
	attempt.Provider = normalizeIdentifier(attempt.Provider)
	attempt.PlanType = normalizeIdentifier(attempt.PlanType)
	attempt.AuthKind = normalizeIdentifier(attempt.AuthKind)
	attempt.AuthID = strings.TrimSpace(attempt.AuthID)
	attempt.AuthIndex = strings.TrimSpace(attempt.AuthIndex)
	attempt.Error = strings.TrimSpace(attempt.Error)
	if attempt.Timestamp.IsZero() {
		if !attempt.CompletedAt.IsZero() {
			attempt.Timestamp = attempt.CompletedAt
		} else {
			attempt.Timestamp = time.Now()
		}
	}
	return attempt
}

func incrementCounter(target map[string]int64, key string, enabled bool) {
	if !enabled || target == nil {
		return
	}
	target[key]++
}

func cloneCounters(source map[string]int64) map[string]int64 {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]int64, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func RedactProxyURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	if parsed.User != nil {
		parsed.User = nil
	}
	return parsed.String()
}

func rotateProxyURLs(proxyURLs []string, seed uint64) []string {
	if len(proxyURLs) <= 1 {
		return append([]string(nil), proxyURLs...)
	}
	start := int(seed % uint64(len(proxyURLs)))
	rotated := make([]string, 0, len(proxyURLs))
	rotated = append(rotated, proxyURLs[start:]...)
	rotated = append(rotated, proxyURLs[:start]...)
	return rotated
}

func transportFailureCooldownUntil(metric *proxyMetrics) time.Time {
	if metric == nil || metric.TransportFailStreak <= 0 || metric.LastTransportFailAt.IsZero() {
		return time.Time{}
	}
	if !metric.LastSuccessAt.IsZero() && metric.LastSuccessAt.After(metric.LastTransportFailAt) {
		return time.Time{}
	}
	cooldown := transportFailureCooldownBase
	for step := 1; step < metric.TransportFailStreak; step++ {
		cooldown *= 2
		if cooldown >= transportFailureCooldownMax {
			cooldown = transportFailureCooldownMax
			break
		}
	}
	return metric.LastTransportFailAt.Add(cooldown)
}
