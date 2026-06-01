package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexroute"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/performance"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

const (
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshMaxConcurrency = 16
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	quotaBackoffBase      = time.Second
	quotaBackoffMax       = 30 * time.Minute
	plain429Cooldown      = 60 * time.Second
	revokedDeleteTimeout  = 10 * time.Second
)

const (
	codexColdRefreshInterval        = 2 * time.Hour
	codexHotRefreshInterval         = 5 * time.Minute
	codexFrequentRefreshInterval    = 45 * time.Second
	codexFrequentActivityWindow     = time.Minute
	codexFrequentActivityThreshold  = 3
	codexFiveHourQuotaRestThreshold = 0.20
	codexFiveHourQuotaLowReason     = "codex_5h_quota_low"
	codexWeeklyQuotaLowReason       = "codex_weekly_quota_low"
	codexSparkUsageLimitReason      = "codex_spark_usage_limit"
	codexStandardUsageLimitReason   = "codex_standard_usage_limit"
)

const (
	streamBootstrapMaxBufferedChunks = 4096
	streamBootstrapMaxBufferedBytes  = 4 << 20
)

const (
	defaultRequestRetry        = 2
	defaultMaxRetryCredentials = 5
	defaultMaxRetryInterval    = 120 * time.Second
)

const modelCapacityRetryCredentialCap = 10

const (
	defaultResponseCompactMaxBytes     = 256 << 20
	defaultResponseCompactMaxEntrySize = 16 << 20
	defaultResponseCompactMaxEntries   = 2048
	defaultSessionBindingMaxEntries    = 8192
)

const responseBindingPinnedAuthMetadataKey = "__cliproxy_response_binding_pinned_auth"
const sessionBindingPinnedAuthMetadataKey = "__cliproxy_session_binding_pinned_auth"
const selectionScopeAuthIDsMetadataKey = "__cliproxy_selection_scope_auth_ids"
const retryExcludedAuthIDsMetadataKey = "__cliproxy_retry_excluded_auth_ids"
const selectionScopeNoAuthID = "__cliproxy_no_auth__"

const defaultImageAuthConcurrencyLimit = 20

var quotaCooldownDisabled atomic.Bool

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Error describes the failure when Success is false.
	Error *Error
	// RequestPayload captures the downstream request body for failure auditing.
	RequestPayload []byte
	// SkipAccessTokenRefresh marks call sites that already cannot refresh/retry
	// access tokens and should treat terminal token errors immediately.
	SkipAccessTokenRefresh bool
}

type inflightLease struct {
	manager  *Manager
	provider string
	model    string
	authID   string
	once     sync.Once
}

func (l *inflightLease) Release() {
	if l == nil || l.manager == nil || l.manager.scheduler == nil {
		return
	}
	l.once.Do(func() {
		l.manager.scheduler.releaseInflight(l.provider, l.model, l.authID)
	})
}

type imageAuthLease struct {
	manager *Manager
	authID  string
	once    sync.Once
}

func (l *imageAuthLease) Release() {
	if l == nil || l.manager == nil {
		return
	}
	l.once.Do(func() {
		l.manager.releaseImageAuthLease(l.authID)
	})
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

// PerformanceScorer snapshots and ranks auth/model performance candidates.
type PerformanceScorer interface {
	SnapshotCandidate(provider, authID, model string, inflight int) performance.ScoreCandidate
	ScoreCandidates(candidates []performance.ScoreCandidate, cfg performance.Config) []performance.Score
}

type CodexRouteController interface {
	RoutePlan(authID string) (codexroute.AuthRoutePlan, bool)
	MarkHedgeWinner(authID string, route codexroute.RouteDescriptor, checkedAt time.Time)
}

type performanceScorerHolder struct {
	scorer PerformanceScorer
}

type codexRouteControllerHolder struct {
	controller CodexRouteController
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store     Store
	executors map[string]ProviderExecutor
	selector  Selector
	hook      Hook
	mu        sync.RWMutex
	auths     map[string]*Auth
	scheduler *authScheduler
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int

	// Retry controls request retry behavior.
	requestRetry        atomic.Int32
	maxRetryCredentials atomic.Int32
	maxRetryInterval    atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// modelPoolOffsets tracks per-auth alias pool rotation state.
	modelPoolOffsets map[string]int

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// performanceRoutingConfig stores the latest auth performance routing controls.
	performanceRoutingConfig atomic.Value
	performanceScorer        atomic.Value
	codexRouteController     atomic.Value

	responseBindingsMu  sync.Mutex
	responseBindings    map[string]string
	sessionBindingsMu   sync.Mutex
	sessionBindings     map[string]string
	routeAwareMu        sync.RWMutex
	routeAwareProviders map[string]bool
	responseCompacts    map[string][]byte
	responseCompactSeq  map[string]uint64

	responseCompactsTotalBytes  int
	responseCompactMaxBytes     int
	responseCompactMaxEntrySize int
	responseCompactMaxEntries   int
	responseCompactNextSeq      uint64

	imageInflightMu sync.Mutex
	imageInflight   map[string]int

	authActivityMu sync.Mutex
	authActivity   map[string][]time.Time

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel       context.CancelFunc
	refreshSemaphore    chan struct{}
	forcedRefreshCancel context.CancelFunc

	quotaFlusher *quotaFlusher

	// Async codex refresh job tracking (single-job model).
	refreshJobMu sync.Mutex
	refreshJob   *RefreshCodexJob // the most recent / currently-running refresh job
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:                       store,
		executors:                   make(map[string]ProviderExecutor),
		selector:                    selector,
		hook:                        hook,
		auths:                       make(map[string]*Auth),
		providerOffsets:             make(map[string]int),
		modelPoolOffsets:            make(map[string]int),
		responseBindings:            make(map[string]string),
		sessionBindings:             make(map[string]string),
		routeAwareProviders:         make(map[string]bool),
		responseCompacts:            make(map[string][]byte),
		responseCompactSeq:          make(map[string]uint64),
		responseCompactMaxBytes:     defaultResponseCompactMaxBytes,
		responseCompactMaxEntrySize: defaultResponseCompactMaxEntrySize,
		responseCompactMaxEntries:   defaultResponseCompactMaxEntries,
		imageInflight:               make(map[string]int),
		authActivity:                make(map[string][]time.Time),
		refreshSemaphore:            make(chan struct{}, refreshMaxConcurrency),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	performanceCfg := performance.DefaultConfig()
	manager.performanceRoutingConfig.Store(performanceCfg)
	manager.performanceScorer.Store(performanceScorerHolder{})
	manager.codexRouteController.Store(codexRouteControllerHolder{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.scheduler = newAuthScheduler(selector)
	manager.scheduler.setPerformanceRoutingConfig(performanceCfg)
	return manager
}

func isBuiltInSelector(selector Selector) bool {
	switch selector.(type) {
	case *RoundRobinSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.invalidateRouteAwareProviderCache()
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
}

func (m *Manager) acquireInflightLease(provider, model, authID string) *inflightLease {
	if m == nil || m.scheduler == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = canonicalModelKey(model)
	authID = strings.TrimSpace(authID)
	if provider == "" || authID == "" {
		return nil
	}
	m.scheduler.acquireInflight(provider, model, authID)
	return &inflightLease{
		manager:  m,
		provider: provider,
		model:    model,
		authID:   authID,
	}
}

func imageGenerationRequestFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.ImageGenerationRequestMetadataKey]
	if !ok {
		return false
	}
	value, ok := raw.(bool)
	return ok && value
}

func imageGenerationModelFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ImageGenerationModelMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func resultModelForOptions(opts cliproxyexecutor.Options, fallback string) string {
	if imageGenerationRequestFromMetadata(opts.Metadata) {
		if imageModel := imageGenerationModelFromMetadata(opts.Metadata); imageModel != "" {
			return imageModel
		}
	}
	return fallback
}

const imageAuthBusyRetryAfter = 10 * time.Second

type imageAuthBusyStatusError struct {
	inner      *Error
	retryAfter time.Duration
}

func newImageAuthBusyError() error {
	return &imageAuthBusyStatusError{
		inner: &Error{
			Code:       "image_auth_busy",
			Message:    "all eligible image generation auths are at concurrency limit",
			Retryable:  true,
			HTTPStatus: http.StatusServiceUnavailable,
		},
		retryAfter: imageAuthBusyRetryAfter,
	}
}

func (e *imageAuthBusyStatusError) Error() string {
	if e == nil || e.inner == nil {
		return ""
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": e.inner.Message,
			"type":    "server_error",
			"code":    e.inner.Code,
		},
	})
	if err != nil {
		return e.inner.Error()
	}
	return string(payload)
}

func (e *imageAuthBusyStatusError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.inner
}

func (e *imageAuthBusyStatusError) StatusCode() int {
	if e == nil || e.inner == nil {
		return 0
	}
	return e.inner.StatusCode()
}

func (e *imageAuthBusyStatusError) Headers() http.Header {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	headers := make(http.Header)
	headers.Set("Retry-After", strconv.Itoa(int(e.retryAfter/time.Second)))
	return headers
}

func (e *imageAuthBusyStatusError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	d := e.retryAfter
	return &d
}

func imageAuthConcurrencyLimit() int {
	limit := defaultImageAuthConcurrencyLimit
	for _, key := range []string{"CLIPROXY_IMAGE_GENERATION_PER_AUTH_LIMIT", "CODEX_GPTDRAW_PER_AUTH_LIMIT"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			continue
		}
		limit = parsed
		break
	}
	if limit > defaultImageAuthConcurrencyLimit {
		return defaultImageAuthConcurrencyLimit
	}
	return limit
}

func (m *Manager) imageAuthInflightCount(authID string) int {
	if m == nil {
		return 0
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	m.imageInflightMu.Lock()
	defer m.imageInflightMu.Unlock()
	return m.imageInflight[authID]
}

func (m *Manager) imageAuthHasCapacity(authID string) bool {
	return m.imageAuthInflightCount(authID) < imageAuthConcurrencyLimit()
}

func (m *Manager) tryAcquireImageAuthLease(authID string) (*imageAuthLease, bool) {
	if m == nil {
		return nil, true
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, true
	}
	m.imageInflightMu.Lock()
	defer m.imageInflightMu.Unlock()
	if m.imageInflight == nil {
		m.imageInflight = make(map[string]int)
	}
	if m.imageInflight[authID] >= imageAuthConcurrencyLimit() {
		return nil, false
	}
	m.imageInflight[authID]++
	return &imageAuthLease{manager: m, authID: authID}, true
}

func (m *Manager) releaseImageAuthLease(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	m.imageInflightMu.Lock()
	defer m.imageInflightMu.Unlock()
	current := m.imageInflight[authID]
	if current <= 1 {
		delete(m.imageInflight, authID)
		return
	}
	m.imageInflight[authID] = current - 1
}

func (m *Manager) recordAuthDispatch(authID string, now time.Time) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-codexFrequentActivityWindow)
	m.authActivityMu.Lock()
	defer m.authActivityMu.Unlock()
	if m.authActivity == nil {
		m.authActivity = make(map[string][]time.Time)
	}
	events := m.authActivity[authID]
	kept := events[:0]
	for _, ts := range events {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	m.authActivity[authID] = kept
}

func (m *Manager) authDispatchCount(authID string, now time.Time) int {
	if m == nil {
		return 0
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-codexFrequentActivityWindow)
	m.authActivityMu.Lock()
	defer m.authActivityMu.Unlock()
	events := m.authActivity[authID]
	kept := events[:0]
	for _, ts := range events {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(m.authActivity, authID)
		return 0
	}
	m.authActivity[authID] = kept
	return len(kept)
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	m.scheduler.upsertAuth(snapshot)
}

// RefreshAuthNow synchronously refreshes one auth through its provider executor.
func (m *Manager) RefreshAuthNow(ctx context.Context, id string) {
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.refreshAuthWithLimit(ctx, id)
}

// ReconcileRegistryModelStates aligns per-model runtime state with the current
// registry snapshot for one auth.
//
// Supported models are reset to a clean state because re-registration already
// cleared the registry-side cooldown/suspension snapshot. ModelStates for
// models that are no longer present in the registry are pruned entirely so
// renamed/removed models cannot keep auth-level status stale.
func (m *Manager) ReconcileRegistryModelStates(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}

	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	supported := make(map[string]struct{}, len(supportedModels))
	for _, model := range supportedModels {
		if model == nil {
			continue
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey == "" {
			continue
		}
		supported[modelKey] = struct{}{}
	}

	var snapshot *Auth
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil && len(auth.ModelStates) > 0 {
		changed := false
		for modelKey, state := range auth.ModelStates {
			baseModel := canonicalModelKey(modelKey)
			if baseModel == "" {
				baseModel = strings.TrimSpace(modelKey)
			}
			if _, supportedModel := supported[baseModel]; !supportedModel {
				// Drop state for models that disappeared from the current registry
				// snapshot. Keeping them around leaks stale errors into auth-level
				// status, management output, and websocket fallback checks.
				delete(auth.ModelStates, modelKey)
				changed = true
				continue
			}
			if state == nil {
				continue
			}
			if modelStateIsClean(state) {
				continue
			}
			resetModelState(state, now)
			changed = true
		}
		if len(auth.ModelStates) == 0 {
			auth.ModelStates = nil
		}
		if changed {
			updateAggregatedAvailability(auth, now)
			if !hasModelError(auth, now) {
				auth.LastError = nil
				auth.StatusMessage = ""
				auth.Status = StatusActive
			}
			auth.UpdatedAt = now
			if errPersist := m.persist(ctx, auth); errPersist != nil {
				logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth changes during model state reconciliation: %v", errPersist)
			}
			snapshot = auth.Clone()
		}
	}
	m.mu.Unlock()

	if m.scheduler != nil && snapshot != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	m.selector = selector
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
}

// SetPerformanceRoutingConfig updates auth performance routing controls.
func (m *Manager) SetPerformanceRoutingConfig(cfg performance.Config) {
	if m == nil {
		return
	}
	cfg = performance.NormalizeConfig(cfg)
	m.performanceRoutingConfig.Store(cfg)
	if m.scheduler != nil {
		m.scheduler.setPerformanceRoutingConfig(cfg)
	}
}

// SetPerformanceScorer updates the scorer used by auth performance routing.
func (m *Manager) SetPerformanceScorer(scorer PerformanceScorer) {
	if m == nil {
		return
	}
	m.performanceScorer.Store(performanceScorerHolder{scorer: scorer})
	if m.scheduler != nil {
		m.scheduler.setPerformanceScorer(scorer)
	}
}

func (m *Manager) SetCodexRouteController(controller CodexRouteController) {
	if m == nil {
		return
	}
	m.codexRouteController.Store(codexRouteControllerHolder{controller: controller})
}

func (m *Manager) codexRouteControllerValue() CodexRouteController {
	if m == nil {
		return nil
	}
	holder, _ := m.codexRouteController.Load().(codexRouteControllerHolder)
	return holder.controller
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.runtimeConfig.Store(cfg)
	m.SetResponseCompactLimits(cfg.ResponseCompact.MaxBytes, cfg.ResponseCompact.MaxEntryBytes, cfg.ResponseCompact.MaxEntries)
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.reconcileIPv6BindLeases(context.Background(), cfg)
}

// SetResponseCompactLimits updates compact transcript cache limits and evicts
// retained transcripts until the current cache fits the new budget.
func (m *Manager) SetResponseCompactLimits(maxBytes, maxEntrySize, maxEntries int) {
	if m == nil {
		return
	}
	if maxBytes <= 0 {
		maxBytes = defaultResponseCompactMaxBytes
	}
	if maxEntrySize <= 0 {
		maxEntrySize = defaultResponseCompactMaxEntrySize
	}
	if maxEntries <= 0 {
		maxEntries = defaultResponseCompactMaxEntries
	}
	m.responseBindingsMu.Lock()
	defer m.responseBindingsMu.Unlock()
	m.responseCompactMaxBytes = maxBytes
	m.responseCompactMaxEntrySize = maxEntrySize
	m.responseCompactMaxEntries = maxEntries
	for responseID, transcript := range m.responseCompacts {
		if len(transcript) > maxEntrySize {
			m.removeCompactTranscriptLocked(responseID)
			delete(m.responseBindings, responseID)
		}
	}
	m.evictCompactTranscriptsLocked(maxBytes, maxEntries)
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	return preserveRequestedModelSuffix(requestedModel, resolved)
}

func isAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func isOpenAICompatAPIKeyAuth(auth *Auth) bool {
	if !isAPIKeyAuth(auth) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	if auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

func openAICompatProviderKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if providerKey := strings.TrimSpace(auth.Attributes["provider_key"]); providerKey != "" {
			return strings.ToLower(providerKey)
		}
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			return strings.ToLower(compatName)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func openAICompatModelPoolKey(auth *Auth, requestedModel string) string {
	base := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if base == "" {
		base = strings.TrimSpace(requestedModel)
	}
	return strings.ToLower(strings.TrimSpace(auth.ID)) + "|" + openAICompatProviderKey(auth) + "|" + strings.ToLower(base)
}

func (m *Manager) nextModelPoolOffset(key string, size int) int {
	if m == nil || size <= 1 {
		return 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelPoolOffsets == nil {
		m.modelPoolOffsets = make(map[string]int)
	}
	offset := m.modelPoolOffsets[key]
	if offset >= 2_147_483_640 {
		offset = 0
	}
	m.modelPoolOffsets[key] = offset + 1
	if size <= 0 {
		return 0
	}
	return offset % size
}

func rotateStrings(values []string, offset int) []string {
	if len(values) <= 1 {
		return values
	}
	if offset <= 0 {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}
	offset = offset % len(values)
	out := make([]string, 0, len(values))
	out = append(out, values[offset:]...)
	out = append(out, values[:offset]...)
	return out
}

func (m *Manager) resolveOpenAICompatUpstreamModelPool(auth *Auth, requestedModel string) []string {
	if m == nil || !isOpenAICompatAPIKeyAuth(auth) {
		return nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return nil
	}
	return resolveModelAliasPoolFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func preserveRequestedModelSuffix(requestedModel, resolved string) string {
	return preserveResolvedModelSuffix(resolved, thinking.ParseSuffix(requestedModel))
}

func (m *Manager) executionModelCandidates(auth *Auth, routeModel string) []string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	requestedModel = m.applyOAuthModelAlias(auth, requestedModel)
	if pool := m.resolveOpenAICompatUpstreamModelPool(auth, requestedModel); len(pool) > 0 {
		if len(pool) == 1 {
			return pool
		}
		offset := m.nextModelPoolOffset(openAICompatModelPoolKey(auth, requestedModel), len(pool))
		return rotateStrings(pool, offset)
	}
	resolved := m.applyAPIKeyModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolved) == "" {
		resolved = requestedModel
	}
	return []string{resolved}
}

func (m *Manager) selectionModelForAuth(auth *Auth, routeModel string) string {
	requestedModel := rewriteModelForAuth(routeModel, auth)
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = strings.TrimSpace(routeModel)
	}
	resolvedModel := m.applyOAuthModelAlias(auth, requestedModel)
	if strings.TrimSpace(resolvedModel) == "" {
		resolvedModel = requestedModel
	}
	return resolvedModel
}

func (m *Manager) selectionModelKeyForAuth(auth *Auth, routeModel string) string {
	return canonicalModelKey(m.selectionModelForAuth(auth, routeModel))
}

func (m *Manager) stateModelForExecution(auth *Auth, routeModel, upstreamModel string, pooled bool) string {
	stateModel := executionResultModel(routeModel, upstreamModel, pooled)
	selectionModel := m.selectionModelForAuth(auth, routeModel)
	if canonicalModelKey(selectionModel) == canonicalModelKey(upstreamModel) && strings.TrimSpace(selectionModel) != "" {
		return strings.TrimSpace(upstreamModel)
	}
	return stateModel
}

func executionResultModel(routeModel, upstreamModel string, pooled bool) string {
	if pooled {
		if resolved := strings.TrimSpace(upstreamModel); resolved != "" {
			return resolved
		}
	}
	if requested := strings.TrimSpace(routeModel); requested != "" {
		return requested
	}
	return strings.TrimSpace(upstreamModel)
}

func (m *Manager) filterExecutionModels(auth *Auth, routeModel string, candidates []string, pooled bool) []string {
	if len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]string, 0, len(candidates))
	for _, upstreamModel := range candidates {
		stateModel := m.stateModelForExecution(auth, routeModel, upstreamModel, pooled)
		blocked, _, _ := isAuthBlockedForModel(auth, stateModel, now)
		if blocked {
			continue
		}
		out = append(out, upstreamModel)
	}
	return out
}

func (m *Manager) preparedExecutionModels(auth *Auth, routeModel string) ([]string, bool) {
	candidates := m.executionModelCandidates(auth, routeModel)
	pooled := len(candidates) > 1
	return m.filterExecutionModels(auth, routeModel, candidates, pooled), pooled
}

func (m *Manager) prepareExecutionModels(auth *Auth, routeModel string) []string {
	models, _ := m.preparedExecutionModels(auth, routeModel)
	return models
}

func (m *Manager) availableAuthsForRouteModel(auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority := make(map[int][]*Auth)
	cooldownCount := 0
	var earliest time.Time
	for _, candidate := range auths {
		checkModel := m.selectionModelForAuth(candidate, routeModel)
		blocked, reason, next := isAuthBlockedForModel(candidate, checkModel, now)
		if !blocked {
			priority := authPriority(candidate)
			availableByPriority[priority] = append(availableByPriority[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}

	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(routeModel, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}

	available := availableByPriority[bestPriority]
	if len(available) > 1 {
		sort.SliceStable(available, func(i, j int) bool { return authPreferredBefore(available[i], available[j]) })
	}
	return available, nil
}

func selectionArgForSelector(selector Selector, routeModel string) string {
	if isBuiltInSelector(selector) {
		return ""
	}
	return routeModel
}

func (m *Manager) authSupportsRouteModel(registryRef *registry.ModelRegistry, auth *Auth, routeModel string) bool {
	if registryRef == nil || auth == nil {
		return true
	}
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return true
	}
	if registryRef.ClientSupportsModel(auth.ID, routeKey) {
		return true
	}
	selectionKey := m.selectionModelKeyForAuth(auth, routeModel)
	return selectionKey != "" && selectionKey != routeKey && registryRef.ClientSupportsModel(auth.ID, selectionKey)
}

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func newStreamBootstrapBufferExceededError(bufferedChunks int, bufferedBytes int) error {
	return &Error{
		Code:       "stream_bootstrap_buffer_exceeded",
		Message:    fmt.Sprintf("stream bootstrap buffered too much replayable data before committed output: chunks=%d bytes=%d", bufferedChunks, bufferedBytes),
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	bufferedBytes := 0
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		if len(chunk.Payload) > 0 && !chunk.BootstrapReplayable {
			buffered = append(buffered, chunk)
			return buffered, false, nil
		}
		if len(buffered)+1 > streamBootstrapMaxBufferedChunks || bufferedBytes+len(chunk.Payload) > streamBootstrapMaxBufferedBytes {
			return nil, false, newStreamBootstrapBufferExceededError(len(buffered), bufferedBytes)
		}
		buffered = append(buffered, chunk)
		bufferedBytes += len(chunk.Payload)
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel string, requestPayload []byte, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, lease *inflightLease, imageLease *imageAuthLease) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer lease.Release()
		defer imageLease.Release()
		defer close(out)
		var failed bool
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := resultErrorFromError(chunk.Err)
				retryAfter := retryAfterFromError(chunk.Err)
				m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, RetryAfter: retryAfter, Error: rerr, RequestPayload: bytes.Clone(requestPayload)})
			}
			if !forward {
				return false
			}
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if !failed {
			m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: true})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

type hedgedStreamBootstrapResult struct {
	route    codexroute.RouteDescriptor
	result   *cliproxyexecutor.StreamResult
	buffered []cliproxyexecutor.StreamChunk
	closed   bool
	err      error
}

func streamBootstrapReady(result hedgedStreamBootstrapResult) bool {
	return result.err == nil && !(result.closed && len(result.buffered) == 0)
}

func (m *Manager) startStreamBootstrapAttempt(ctx context.Context, executor ProviderExecutor, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, route codexroute.RouteDescriptor) (context.CancelFunc, <-chan hedgedStreamBootstrapResult) {
	attemptCtx, cancel := context.WithCancel(ctx)
	attemptCtx = codexroute.WithRequestRoute(attemptCtx, route.RequestRoute())
	out := make(chan hedgedStreamBootstrapResult, 1)
	go func() {
		defer close(out)
		result, errStream := executor.ExecuteStream(attemptCtx, auth, req, opts)
		if errStream != nil {
			out <- hedgedStreamBootstrapResult{route: route, err: errStream}
			return
		}
		buffered, closed, bootstrapErr := readStreamBootstrap(attemptCtx, result.Chunks)
		out <- hedgedStreamBootstrapResult{
			route:    route,
			result:   result,
			buffered: buffered,
			closed:   closed,
			err:      bootstrapErr,
		}
	}()
	return cancel, out
}

func discardHedgedAttempt(result *hedgedStreamBootstrapResult) {
	if result == nil || result.result == nil || result.result.Chunks == nil {
		return
	}
	discardStreamChunks(result.result.Chunks)
}

func (m *Manager) finalizeStreamAttempt(ctx context.Context, auth *Auth, provider, resultModel string, requestPayload []byte, lease *inflightLease, imageLease *imageAuthLease, attempt hedgedStreamBootstrapResult) (*cliproxyexecutor.StreamResult, error) {
	if attempt.err != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			discardHedgedAttempt(&attempt)
			lease.Release()
			imageLease.Release()
			return nil, errCtx
		}
		rerr := resultErrorFromError(attempt.err)
		result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestPayload: bytes.Clone(requestPayload)}
		result.RetryAfter = retryAfterFromError(attempt.err)
		m.MarkResult(ctx, result)
		discardHedgedAttempt(&attempt)
		lease.Release()
		imageLease.Release()
		return nil, newStreamBootstrapError(attempt.err, attempt.resultHeaders())
	}
	if attempt.closed && len(attempt.buffered) == 0 {
		emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
		result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr, RequestPayload: bytes.Clone(requestPayload)}
		m.MarkResult(ctx, result)
		lease.Release()
		imageLease.Release()
		return nil, newStreamBootstrapError(emptyErr, attempt.resultHeaders())
	}
	remaining := attempt.result.Chunks
	if attempt.closed {
		closedCh := make(chan cliproxyexecutor.StreamChunk)
		close(closedCh)
		remaining = closedCh
	}
	return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, requestPayload, attempt.result.Headers, attempt.buffered, remaining, lease, imageLease), nil
}

func (r hedgedStreamBootstrapResult) resultHeaders() http.Header {
	if r.result == nil {
		return nil
	}
	return r.result.Headers
}

func (m *Manager) codexRouteHedgeSettings(auth *Auth, provider string, execModels []string) (codexroute.AuthRoutePlan, time.Duration, bool) {
	if m == nil || !isCodexAuth(auth) || !strings.EqualFold(strings.TrimSpace(provider), "codex") || len(execModels) != 1 {
		return codexroute.AuthRoutePlan{}, 0, false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.CodexRouteManagement.Enabled || cfg.CodexRouteManagement.FirstPayloadHedgeAfter <= 0 {
		return codexroute.AuthRoutePlan{}, 0, false
	}
	controller := m.codexRouteControllerValue()
	if controller == nil {
		return codexroute.AuthRoutePlan{}, 0, false
	}
	plan, ok := controller.RoutePlan(auth.ID)
	if !ok {
		return codexroute.AuthRoutePlan{}, 0, false
	}
	if strings.EqualFold(strings.TrimSpace(plan.Primary.Entry), strings.TrimSpace(plan.Standby.Entry)) && strings.EqualFold(strings.TrimSpace(plan.Primary.Pool), strings.TrimSpace(plan.Standby.Pool)) {
		return codexroute.AuthRoutePlan{}, 0, false
	}
	return plan, cfg.CodexRouteManagement.FirstPayloadHedgeAfter, true
}

func (m *Manager) executeStreamWithCodexRouteHedge(ctx context.Context, executor ProviderExecutor, auth *Auth, provider, resultModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, lease *inflightLease, imageLease *imageAuthLease, plan codexroute.AuthRoutePlan, hedgeAfter time.Duration) (*cliproxyexecutor.StreamResult, error) {
	primaryCancel, primaryCh := m.startStreamBootstrapAttempt(ctx, executor, auth, req, opts, plan.Primary)

	timer := time.NewTimer(hedgeAfter)
	defer timer.Stop()
	timerCh := timer.C

	var primaryResult *hedgedStreamBootstrapResult
	var standbyResult *hedgedStreamBootstrapResult
	var standbyCancel context.CancelFunc
	var standbyCh <-chan hedgedStreamBootstrapResult

	startStandby := func() {
		if standbyCh != nil {
			return
		}
		standbyCancel, standbyCh = m.startStreamBootstrapAttempt(ctx, executor, auth, req, opts, plan.Standby)
	}

	cancelPending := func() {
		primaryCancel()
		if standbyCancel != nil {
			standbyCancel()
		}
		discardHedgedAttempt(primaryResult)
		discardHedgedAttempt(standbyResult)
	}

	for {
		select {
		case <-ctx.Done():
			cancelPending()
			lease.Release()
			imageLease.Release()
			return nil, ctx.Err()
		case result, ok := <-primaryCh:
			if !ok {
				primaryCh = nil
				continue
			}
			primaryCh = nil
			primaryResult = &result
			if streamBootstrapReady(result) {
				if standbyCancel != nil {
					standbyCancel()
				}
				discardHedgedAttempt(standbyResult)
				return m.finalizeStreamAttempt(ctx, auth, provider, resultModel, req.Payload, lease, imageLease, result)
			}
			startStandby()
		case <-timerCh:
			timerCh = nil
			startStandby()
		case result, ok := <-standbyCh:
			if !ok {
				standbyCh = nil
				continue
			}
			standbyCh = nil
			standbyResult = &result
			if streamBootstrapReady(result) {
				primaryCancel()
				discardHedgedAttempt(primaryResult)
				if controller := m.codexRouteControllerValue(); controller != nil {
					controller.MarkHedgeWinner(auth.ID, plan.Standby, time.Now())
				}
				return m.finalizeStreamAttempt(ctx, auth, provider, resultModel, req.Payload, lease, imageLease, result)
			}
		}
		if primaryResult != nil && standbyCh == nil && standbyResult == nil && timerCh == nil {
			return m.finalizeStreamAttempt(ctx, auth, provider, resultModel, req.Payload, lease, imageLease, *primaryResult)
		}
		if primaryResult != nil && standbyResult != nil {
			if standbyResult.err != nil {
				return m.finalizeStreamAttempt(ctx, auth, provider, resultModel, req.Payload, lease, imageLease, *primaryResult)
			}
			return m.finalizeStreamAttempt(ctx, auth, provider, resultModel, req.Payload, lease, imageLease, *standbyResult)
		}
	}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel string, execModels []string, pooled bool, lease *inflightLease, imageLease *imageAuthLease) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		lease.Release()
		imageLease.Release()
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	var lastErr error
	for idx, execModel := range execModels {
		resultModel := resultModelForOptions(opts, m.stateModelForExecution(auth, routeModel, execModel, pooled))
		execReq := req
		execReq.Model = execModel
		if routePlan, hedgeAfter, ok := m.codexRouteHedgeSettings(auth, provider, execModels); ok {
			return m.executeStreamWithCodexRouteHedge(ctx, executor, auth, provider, resultModel, execReq, opts, lease, imageLease, routePlan, hedgeAfter)
		}
		streamResult, errStream := executor.ExecuteStream(ctx, auth, execReq, opts)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				lease.Release()
				imageLease.Release()
				return nil, errCtx
			}
			rerr := resultErrorFromError(errStream)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestPayload: bytes.Clone(execReq.Payload)}
			result.RetryAfter = retryAfterFromError(errStream)
			m.MarkResult(ctx, result)
			if isRequestInvalidError(errStream) {
				lease.Release()
				imageLease.Release()
				return nil, errStream
			}
			lastErr = errStream
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				lease.Release()
				imageLease.Release()
				return nil, errCtx
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestPayload: bytes.Clone(execReq.Payload)}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lease.Release()
				imageLease.Release()
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestPayload: bytes.Clone(execReq.Payload)}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.MarkResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, RequestPayload: bytes.Clone(execReq.Payload)}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			m.MarkResult(ctx, result)
			discardStreamChunks(streamResult.Chunks)
			lease.Release()
			imageLease.Release()
			return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr, RequestPayload: bytes.Clone(execReq.Payload)}
			m.MarkResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			lease.Release()
			imageLease.Release()
			return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, execReq.Payload, streamResult.Headers, buffered, remaining, lease, imageLease), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	lease.Release()
	imageLease.Release()
	return nil, lastErr
}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		byAlias := compileAPIKeyModelAliasForAuth(cfg, auth)
		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForAuth(cfg *internalconfig.Config, auth *Auth) map[string]string {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil
	}
	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return nil
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	byAlias := make(map[string]string)
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "gemini":
		if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
			compileAPIKeyModelAliasForModels(byAlias, entry.Models)
		}
	case "claude":
		if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
			compileAPIKeyModelAliasForModels(byAlias, entry.Models)
		}
	case "codex":
		if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
			compileAPIKeyModelAliasForModels(byAlias, entry.Models)
		}
	case "vertex":
		if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
			compileAPIKeyModelAliasForModels(byAlias, entry.Models)
		}
	default:
		// OpenAI-compat uses config selection from auth.Attributes.
		providerKey := ""
		compatName := ""
		if auth.Attributes != nil {
			providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
			compatName = strings.TrimSpace(auth.Attributes["compat_name"])
		}
		if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
			if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		}
	}

	if len(byAlias) == 0 {
		return nil
	}
	return byAlias
}

func cloneAPIKeyModelAliasTable(src apiKeyModelAliasTable) apiKeyModelAliasTable {
	if len(src) == 0 {
		return make(apiKeyModelAliasTable)
	}
	dst := make(apiKeyModelAliasTable, len(src))
	for authID, aliases := range src {
		if len(aliases) == 0 {
			dst[authID] = nil
			continue
		}
		copyAliases := make(map[string]string, len(aliases))
		for alias, model := range aliases {
			copyAliases[alias] = model
		}
		dst[authID] = copyAliases
	}
	return dst
}

func (m *Manager) syncAPIKeyModelAliasForAuthTransition(previous *Auth, next *Auth) {
	if m == nil {
		return
	}
	previousIsAPIKey := previous != nil && strings.TrimSpace(previous.ID) != "" && isAPIKeyAccount(previous)
	nextIsAPIKey := next != nil && strings.TrimSpace(next.ID) != "" && isAPIKeyAccount(next)
	if !previousIsAPIKey && !nextIsAPIKey {
		return
	}

	current, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	nextTable := cloneAPIKeyModelAliasTable(current)
	if previousIsAPIKey && (!nextIsAPIKey || !strings.EqualFold(strings.TrimSpace(previous.ID), strings.TrimSpace(next.ID))) {
		delete(nextTable, previous.ID)
	}
	if nextIsAPIKey {
		cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
		if cfg == nil {
			cfg = &internalconfig.Config{}
		}
		if aliases := compileAPIKeyModelAliasForAuth(cfg, next); len(aliases) > 0 {
			nextTable[next.ID] = aliases
		} else {
			delete(nextTable, next.ID)
		}
	}
	m.apiKeyModelAlias.Store(nextTable)
}

func isAPIKeyAccount(auth *Auth) bool {
	if auth == nil {
		return false
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "api_key")
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if hasRevokedAuthTombstoneMemory(auth, time.Now()) {
		return auth.Clone(), nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	auth.EnsureIndex()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	m.mu.Lock()
	_ = m.ensureIPv6BindLeaseForAuthLocked(cfg, auth)
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.invalidateRouteAwareProviderCacheForAuthTransition(nil, authClone)
	m.syncAPIKeyModelAliasForAuthTransition(nil, authClone)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	_ = m.persist(ctx, auth)
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	// Codex auths get a fire-and-forget immediate refresh so the real plan_type
	// (via /wham/usage) is known within seconds of import, not 5 minutes later.
	// Only codex needs this; other providers do not have the JWT/usage mismatch
	// problem this solves.
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		if m.executorFor(auth.Provider) != nil {
			go m.refreshAuth(context.Background(), authClone.ID)
		}
	}
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	if hasRevokedAuthTombstoneMemory(auth, time.Now()) {
		return auth.Clone(), nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	var previousClone *Auth
	m.mu.Lock()
	if existing, ok := m.auths[auth.ID]; ok && existing != nil {
		previousClone = existing.Clone()
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		preserveIPv6BindLeaseIfMissing(auth, existing)
		if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
			preserveRuntimeAvailabilityState(auth, existing, time.Now())
		}
	}
	auth.EnsureIndex()
	_ = m.ensureIPv6BindLeaseForAuthLocked(cfg, auth)
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.invalidateRouteAwareProviderCacheForAuthTransition(previousClone, authClone)
	m.syncAPIKeyModelAliasForAuthTransition(previousClone, authClone)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	_ = m.persist(ctx, auth)
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Remove deletes one auth from runtime state, scheduler state, and the global
// model registry. Persistence deletion is skipped when WithSkipPersist is set.
func (m *Manager) Remove(ctx context.Context, authID string) (*Auth, error) {
	if m == nil {
		return nil, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil
	}

	removed := m.removeAuthRuntime(authID)
	return removed, m.deletePersistedAuthRecord(ctx, removed, authID)
}

func (m *Manager) removeAuthRuntime(authID string) *Auth {
	if m == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}

	var removed *Auth
	m.mu.Lock()
	if current := m.auths[authID]; current != nil {
		ClearIPv6BindLease(current)
		removed = current.Clone()
		delete(m.auths, authID)
	}
	m.mu.Unlock()

	m.finalizeAuthRemoval(removed, authID)
	return removed
}

func (m *Manager) finalizeAuthRemoval(auth *Auth, authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if auth != nil {
		m.invalidateRouteAwareProviderCacheForAuthTransition(auth, nil)
		m.syncAPIKeyModelAliasForAuthTransition(auth, nil)
		if authID == "" {
			authID = strings.TrimSpace(auth.ID)
		}
	}
	if authID != "" {
		if m.scheduler != nil {
			m.scheduler.removeAuth(authID)
		}
		registry.GetGlobalRegistry().UnregisterClient(authID)
	}
}

func (m *Manager) deletePersistedAuthRecord(ctx context.Context, auth *Auth, authID string) error {
	if m == nil || m.store == nil || shouldSkipPersist(ctx) || auth == nil || !hasPersistedAuthRecord(auth) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	recordID := strings.TrimSpace(authID)
	if recordID == "" && auth != nil {
		recordID = strings.TrimSpace(auth.ID)
	}
	if recordID == "" {
		return nil
	}
	return m.store.Delete(ctx, recordID)
}

func preserveRuntimeAvailabilityState(auth, existing *Auth, now time.Time) {
	if auth == nil || existing == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if existing.UpdatedAt.After(auth.UpdatedAt) || shouldPreserveRuntimeUsageLimit(auth, existing, now) {
		if len(existing.ModelStates) > 0 {
			auth.ModelStates = cloneModelStates(existing.ModelStates)
		}
		if existing.Unavailable {
			auth.Unavailable = existing.Unavailable
			auth.NextRetryAfter = existing.NextRetryAfter
			auth.Quota = existing.Quota
		}
		if existing.LastError != nil {
			auth.LastError = cloneError(existing.LastError)
		}
		if existing.Status == StatusError || strings.TrimSpace(existing.StatusMessage) != "" {
			auth.Status = existing.Status
			auth.StatusMessage = existing.StatusMessage
		}
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
		if hasModelError(auth, now) && auth.Status != StatusDisabled {
			auth.Status = StatusError
		}
	}
}

func shouldPreserveRuntimeUsageLimit(auth, existing *Auth, now time.Time) bool {
	if auth == nil || existing == nil {
		return false
	}
	if !isCodexAuth(existing) {
		return false
	}
	if auth.Quota.Exceeded && (auth.Quota.Reason == codexFiveHourQuotaLowReason || auth.Quota.Reason == codexWeeklyQuotaLowReason) {
		return false
	}
	if !existing.Quota.Exceeded || existing.Quota.Reason != "usage_limit" {
		return false
	}
	next := existing.Quota.NextRecoverAt
	if next.IsZero() {
		next = existing.NextRetryAfter
	}
	if !next.After(now) {
		return false
	}
	return true
}

func cloneModelStates(states map[string]*ModelState) map[string]*ModelState {
	if len(states) == 0 {
		return nil
	}
	cloned := make(map[string]*ModelState, len(states))
	for key, state := range states {
		if state != nil {
			cloned[key] = state.Clone()
		}
	}
	return cloned
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		if hasRevokedAuthTombstoneMemory(auth, time.Now()) {
			continue
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	m.reconcileIPv6BindLeases(ctx, cfg)
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteCount performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, errExec := m.executeCountMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxRetryCredentials, maxWait := m.retrySettings()

	var lastErr error
	for attempt := 0; ; attempt++ {
		result, errStream := m.executeStreamMixedOnce(ctx, normalized, req, opts, maxRetryCredentials)
		if errStream == nil {
			return result, nil
		}
		lastErr = errStream
		wait, shouldRetry := m.shouldRetryAfterError(errStream, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return nil, errWait
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	opts = ensureResponseBindingMetadata(req, opts, m.lookupBoundAuthID)
	opts = ensureSessionBindingMetadata(opts, m.lookupSessionBoundAuthID)
	selectionScopeAuthIDs := m.selectionScopeAuthIDs(providers, routeModel, opts)
	opts = ensureSelectionScopeMetadata(opts, selectionScopeAuthIDs)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	usageLimitRetryCredentials := len(selectionScopeAuthIDs)
	imageRequest := imageGenerationRequestFromMetadata(opts.Metadata)
	imageFallbackActive := false
	skippedBusyImageAuth := false
	var lastErr error
	for {
		if credentialRetryLimitReached(maxRetryCredentials, usageLimitRetryCredentials, len(attempted), lastErr) {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" && sessionBindingPinnedAuthFromMetadata(opts.Metadata) {
				m.unbindSessionFromMetadata(opts.Metadata, pinnedAuthID)
				opts = clearSessionBindingPinnedAuthMetadata(opts, pinnedAuthID)
				continue
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			if imageRequest && skippedBusyImageAuth && !imageFallbackActive {
				fallbackScope := m.imageFallbackSelectionScopeAuthIDs(providers, routeModel, opts)
				if len(fallbackScope) > 0 {
					opts = ensureSelectionScopeMetadata(opts, fallbackScope)
					usageLimitRetryCredentials = len(fallbackScope)
					tried = make(map[string]struct{})
					imageFallbackActive = true
					skippedBusyImageAuth = false
					continue
				}
			}
			if imageRequest && skippedBusyImageAuth {
				return cliproxyexecutor.Response{}, newImageAuthBusyError()
			}
			return cliproxyexecutor.Response{}, errPick
		}

		var imageLease *imageAuthLease
		if imageRequest {
			var ok bool
			imageLease, ok = m.tryAcquireImageAuthLease(auth.ID)
			if !ok {
				skippedBusyImageAuth = true
				tried[auth.ID] = struct{}{}
				continue
			}
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)
		m.recordAuthDispatch(auth.ID, time.Now())

		// Opportunistic async re-probe for codex bound paid auths whose
		// binding is older than AsyncBindingProbeTTL. Fires in a goroutine
		// so the current dispatch isn't delayed; the probe result
		// protects subsequent requests from stale bindings.
		m.KickAsyncBindingProbe(auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}

		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			imageLease.Release()
			continue
		}
		attempted[auth.ID] = struct{}{}
		lease := m.acquireInflightLease(provider, routeModel, auth.ID)
		releaseLeases := func() {
			lease.Release()
			imageLease.Release()
		}
		var authErr error
		for _, upstreamModel := range models {
			resultModel := resultModelForOptions(opts, m.stateModelForExecution(auth, routeModel, upstreamModel, pooled))
			execReq := req
			execReq.Model = upstreamModel
			var errExpand error
			execReq, errExpand = m.expandPreviousResponseRequest(execReq, opts)
			if errExpand != nil {
				releaseLeases()
				return cliproxyexecutor.Response{}, errExpand
			}
			resp, errExec := executor.Execute(execCtx, auth, execReq, opts)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil, RequestPayload: bytes.Clone(execReq.Payload)}
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					releaseLeases()
					return cliproxyexecutor.Response{}, errCtx
				}
				setExecutionResultError(&result, errExec)
				if shouldAttemptAccessTokenRefresh(auth, result.Error) {
					// Don't block the request on a multi-second web re-login: kick it
					// in the background and fail over now. The dead auth is recoverable
					// (creds present), so leaving SkipAccessTokenRefresh false keeps
					// MarkResult from retiring it; a successful background login returns
					// it to rotation for subsequent requests.
					m.kickAsyncRelogin(execCtx, auth, result.Error)
				}
				if errExec == nil {
					m.MarkResult(execCtx, result)
					m.bindResponseFromPayload(auth.ID, execReq.Payload, resp.Payload)
					m.bindSessionFromMetadata(opts.Metadata, auth.ID)
					releaseLeases()
					return resp, nil
				}
				m.MarkResult(execCtx, result)
				if isTransientCooldownError(errExec) {
					opts = clearResponseBindingPinnedAuthMetadata(opts, auth.ID)
					m.unbindSessionFromMetadata(opts.Metadata, auth.ID)
					opts = clearSessionBindingPinnedAuthMetadata(opts, auth.ID)
				}
				if isRequestInvalidError(errExec) {
					releaseLeases()
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				continue
			}
			m.MarkResult(execCtx, result)
			m.bindResponseFromPayload(auth.ID, execReq.Payload, resp.Payload)
			m.bindSessionFromMetadata(opts.Metadata, auth.ID)
			releaseLeases()
			return resp, nil
		}
		releaseLeases()
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			lastErr = authErr
			continue
		}
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	opts = ensureResponseBindingMetadata(req, opts, m.lookupBoundAuthID)
	opts = ensureSessionBindingMetadata(opts, m.lookupSessionBoundAuthID)
	selectionScopeAuthIDs := m.selectionScopeAuthIDs(providers, routeModel, opts)
	opts = ensureSelectionScopeMetadata(opts, selectionScopeAuthIDs)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	usageLimitRetryCredentials := len(selectionScopeAuthIDs)
	var lastErr error
	for {
		if credentialRetryLimitReached(maxRetryCredentials, usageLimitRetryCredentials, len(attempted), lastErr) {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" && sessionBindingPinnedAuthFromMetadata(opts.Metadata) {
				m.unbindSessionFromMetadata(opts.Metadata, pinnedAuthID)
				opts = clearSessionBindingPinnedAuthMetadata(opts, pinnedAuthID)
				continue
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)
		m.recordAuthDispatch(auth.ID, time.Now())

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}

		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			continue
		}
		attempted[auth.ID] = struct{}{}
		lease := m.acquireInflightLease(provider, routeModel, auth.ID)
		var authErr error
		for _, upstreamModel := range models {
			resultModel := resultModelForOptions(opts, m.stateModelForExecution(auth, routeModel, upstreamModel, pooled))
			execReq := req
			execReq.Model = upstreamModel
			resp, errExec := executor.CountTokens(execCtx, auth, execReq, opts)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil, RequestPayload: bytes.Clone(execReq.Payload)}
			if errExec != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					lease.Release()
					return cliproxyexecutor.Response{}, errCtx
				}
				setExecutionResultError(&result, errExec)
				if shouldAttemptAccessTokenRefresh(auth, result.Error) {
					// Don't block the request on a multi-second web re-login: kick it
					// in the background and fail over now. The dead auth is recoverable
					// (creds present), so leaving SkipAccessTokenRefresh false keeps
					// MarkResult from retiring it; a successful background login returns
					// it to rotation for subsequent requests.
					m.kickAsyncRelogin(execCtx, auth, result.Error)
				}
				if errExec == nil {
					m.MarkResult(execCtx, result)
					m.bindSessionFromMetadata(opts.Metadata, auth.ID)
					lease.Release()
					return resp, nil
				}
				m.MarkResult(execCtx, result)
				if isTransientCooldownError(errExec) {
					opts = clearResponseBindingPinnedAuthMetadata(opts, auth.ID)
					m.unbindSessionFromMetadata(opts.Metadata, auth.ID)
					opts = clearSessionBindingPinnedAuthMetadata(opts, auth.ID)
				}
				if isRequestInvalidError(errExec) {
					lease.Release()
					return cliproxyexecutor.Response{}, errExec
				}
				authErr = errExec
				continue
			}
			m.MarkResult(execCtx, result)
			m.bindSessionFromMetadata(opts.Metadata, auth.ID)
			lease.Release()
			return resp, nil
		}
		lease.Release()
		if authErr != nil {
			if isRequestInvalidError(authErr) {
				return cliproxyexecutor.Response{}, authErr
			}
			lastErr = authErr
			continue
		}
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (*cliproxyexecutor.StreamResult, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	opts = ensureResponseBindingMetadata(req, opts, m.lookupBoundAuthID)
	opts = ensureSessionBindingMetadata(opts, m.lookupSessionBoundAuthID)
	selectionScopeAuthIDs := m.selectionScopeAuthIDs(providers, routeModel, opts)
	opts = ensureSelectionScopeMetadata(opts, selectionScopeAuthIDs)
	tried := make(map[string]struct{})
	attempted := make(map[string]struct{})
	usageLimitRetryCredentials := len(selectionScopeAuthIDs)
	imageRequest := imageGenerationRequestFromMetadata(opts.Metadata)
	imageFallbackActive := false
	skippedBusyImageAuth := false
	var lastErr error
	for {
		if credentialRetryLimitReached(maxRetryCredentials, usageLimitRetryCredentials, len(attempted), lastErr) {
			if lastErr != nil {
				var bootstrapErr *streamBootstrapError
				if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
					return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
				}
				return nil, lastErr
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" && sessionBindingPinnedAuthFromMetadata(opts.Metadata) {
				m.unbindSessionFromMetadata(opts.Metadata, pinnedAuthID)
				opts = clearSessionBindingPinnedAuthMetadata(opts, pinnedAuthID)
				continue
			}
			if lastErr != nil {
				var bootstrapErr *streamBootstrapError
				if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
					return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
				}
				return nil, lastErr
			}
			if imageRequest && skippedBusyImageAuth && !imageFallbackActive {
				fallbackScope := m.imageFallbackSelectionScopeAuthIDs(providers, routeModel, opts)
				if len(fallbackScope) > 0 {
					opts = ensureSelectionScopeMetadata(opts, fallbackScope)
					usageLimitRetryCredentials = len(fallbackScope)
					tried = make(map[string]struct{})
					imageFallbackActive = true
					skippedBusyImageAuth = false
					continue
				}
			}
			if imageRequest && skippedBusyImageAuth {
				return nil, newImageAuthBusyError()
			}
			return nil, errPick
		}

		var imageLease *imageAuthLease
		if imageRequest {
			var ok bool
			imageLease, ok = m.tryAcquireImageAuthLease(auth.ID)
			if !ok {
				skippedBusyImageAuth = true
				tried[auth.ID] = struct{}{}
				continue
			}
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)
		m.recordAuthDispatch(auth.ID, time.Now())

		// Opportunistic async re-probe mirrors the Execute path.
		m.KickAsyncBindingProbe(auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		models, pooled := m.preparedExecutionModels(auth, routeModel)
		if len(models) == 0 {
			imageLease.Release()
			continue
		}
		attempted[auth.ID] = struct{}{}
		lease := m.acquireInflightLease(provider, routeModel, auth.ID)
		execReq, errExpand := m.expandPreviousResponseRequest(req, opts)
		if errExpand != nil {
			lease.Release()
			imageLease.Release()
			return nil, errExpand
		}
		streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, execReq, opts, routeModel, models, pooled, lease, imageLease)
		if errStream != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if isTransientCooldownError(errStream) {
				opts = clearResponseBindingPinnedAuthMetadata(opts, auth.ID)
				m.unbindSessionFromMetadata(opts.Metadata, auth.ID)
				opts = clearSessionBindingPinnedAuthMetadata(opts, auth.ID)
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}
		m.bindSessionFromMetadata(opts.Metadata, auth.ID)
		return m.bindResponseFromStreamResult(auth.ID, execReq.Payload, streamResult), nil
	}
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func ensureResponseBindingMetadata(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, lookup func(string) string) cliproxyexecutor.Options {
	if len(opts.Metadata) > 0 && pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return opts
	}
	if strings.EqualFold(strings.TrimSpace(opts.Alt), "responses/compact") &&
		compactRequestHasInput(req, opts) &&
		preferBestAuthFromMetadata(opts.Metadata) {
		return opts
	}
	if lookup == nil {
		return opts
	}
	responseID := previousResponseIDFromRequest(req, opts)
	if responseID == "" {
		return opts
	}
	authID := strings.TrimSpace(lookup(responseID))
	if authID == "" {
		return opts
	}
	if externalRetryAttemptFromMetadata(opts.Metadata) > 0 {
		return appendRetryExcludedAuthMetadata(opts, authID)
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey: authID,
			responseBindingPinnedAuthMetadataKey:   true,
		}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	meta[responseBindingPinnedAuthMetadataKey] = true
	opts.Metadata = meta
	return opts
}

func ensureSessionBindingMetadata(opts cliproxyexecutor.Options, lookup func(string) string) cliproxyexecutor.Options {
	if len(opts.Metadata) > 0 && pinnedAuthIDFromMetadata(opts.Metadata) != "" {
		return opts
	}
	if lookup == nil {
		return opts
	}
	sessionID := affinitySessionIDFromMetadata(opts.Metadata)
	if sessionID == "" {
		return opts
	}
	authID := strings.TrimSpace(lookup(sessionID))
	if authID == "" {
		return opts
	}
	if externalRetryAttemptFromMetadata(opts.Metadata) > 0 {
		return appendRetryExcludedAuthMetadata(opts, authID)
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{
			cliproxyexecutor.PinnedAuthMetadataKey:  authID,
			cliproxyexecutor.AuthSessionMetadataKey: sessionID,
			sessionBindingPinnedAuthMetadataKey:     true,
		}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+2)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	meta[cliproxyexecutor.AuthSessionMetadataKey] = sessionID
	meta[sessionBindingPinnedAuthMetadataKey] = true
	opts.Metadata = meta
	return opts
}

func clearResponseBindingPinnedAuthMetadata(opts cliproxyexecutor.Options, authID string) cliproxyexecutor.Options {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(opts.Metadata) == 0 {
		return opts
	}
	if !responseBindingPinnedAuthFromMetadata(opts.Metadata) {
		return opts
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != authID {
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata))
	for k, v := range opts.Metadata {
		if k == cliproxyexecutor.PinnedAuthMetadataKey || k == responseBindingPinnedAuthMetadataKey {
			continue
		}
		meta[k] = v
	}
	opts.Metadata = meta
	return opts
}

func clearSessionBindingPinnedAuthMetadata(opts cliproxyexecutor.Options, authID string) cliproxyexecutor.Options {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(opts.Metadata) == 0 {
		return opts
	}
	if !sessionBindingPinnedAuthFromMetadata(opts.Metadata) {
		return opts
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) != authID {
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata))
	for k, v := range opts.Metadata {
		if k == cliproxyexecutor.PinnedAuthMetadataKey || k == sessionBindingPinnedAuthMetadataKey {
			continue
		}
		meta[k] = v
	}
	opts.Metadata = meta
	return opts
}

func appendRetryExcludedAuthMetadata(opts cliproxyexecutor.Options, authID string) cliproxyexecutor.Options {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return opts
	}
	if existing := retryExcludedAuthIDsFromMetadata(opts.Metadata); len(existing) > 0 {
		if _, ok := existing[authID]; ok {
			return opts
		}
	}
	var meta map[string]any
	if len(opts.Metadata) == 0 {
		meta = make(map[string]any, 1)
	} else {
		meta = make(map[string]any, len(opts.Metadata)+1)
		for k, v := range opts.Metadata {
			meta[k] = v
		}
	}
	excluded := retryExcludedAuthIDsFromMetadata(meta)
	if len(excluded) == 0 {
		excluded = make(map[string]struct{}, 1)
	}
	excluded[authID] = struct{}{}
	meta[retryExcludedAuthIDsMetadataKey] = excluded
	opts.Metadata = meta
	return opts
}

func ensureSelectionScopeMetadata(opts cliproxyexecutor.Options, allowed map[string]struct{}) cliproxyexecutor.Options {
	if allowed == nil {
		return opts
	}
	if len(allowed) == 0 {
		allowed = map[string]struct{}{selectionScopeNoAuthID: {}}
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{selectionScopeAuthIDsMetadataKey: allowed}
		return opts
	}
	if existing := selectionScopeAuthIDsFromMetadata(opts.Metadata); len(existing) == len(allowed) && len(existing) > 0 {
		match := true
		for authID := range allowed {
			if _, ok := existing[authID]; !ok {
				match = false
				break
			}
		}
		if match {
			return opts
		}
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[selectionScopeAuthIDsMetadataKey] = allowed
	opts.Metadata = meta
	return opts
}

func responseBindingPinnedAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[responseBindingPinnedAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(strings.TrimSpace(val), "true")
	default:
		return false
	}
}

func sessionBindingPinnedAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[sessionBindingPinnedAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(strings.TrimSpace(val), "true")
	default:
		return false
	}
}

func externalRetryAttemptFromMetadata(meta map[string]any) int {
	if len(meta) == 0 {
		return 0
	}
	raw, ok := meta[cliproxyexecutor.ExternalRetryAttemptMetadataKey]
	if !ok || raw == nil {
		return 0
	}
	switch v := raw.(type) {
	case int:
		if v > 0 {
			return v
		}
	case int32:
		if v > 0 {
			return int(v)
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

func retryExcludedAuthIDsFromMetadata(meta map[string]any) map[string]struct{} {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta[retryExcludedAuthIDsMetadataKey]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case map[string]struct{}:
		if len(v) == 0 {
			return nil
		}
		return v
	case []string:
		out := make(map[string]struct{}, len(v))
		for _, authID := range v {
			if trimmed := strings.TrimSpace(authID); trimmed != "" {
				out[trimmed] = struct{}{}
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make(map[string]struct{}, len(v))
		for _, item := range v {
			if authID, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(authID); trimmed != "" {
					out[trimmed] = struct{}{}
				}
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

func selectionScopeAuthIDsFromMetadata(meta map[string]any) map[string]struct{} {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta[selectionScopeAuthIDsMetadataKey]
	if !ok || raw == nil {
		return nil
	}
	switch val := raw.(type) {
	case map[string]struct{}:
		return val
	case map[string]bool:
		if len(val) == 0 {
			return nil
		}
		allowed := make(map[string]struct{}, len(val))
		for authID, enabled := range val {
			if enabled {
				allowed[strings.TrimSpace(authID)] = struct{}{}
			}
		}
		return allowed
	case []string:
		if len(val) == 0 {
			return nil
		}
		allowed := make(map[string]struct{}, len(val))
		for _, authID := range val {
			authID = strings.TrimSpace(authID)
			if authID != "" {
				allowed[authID] = struct{}{}
			}
		}
		return allowed
	default:
		return nil
	}
}

func authAllowedBySelectionScope(allowed map[string]struct{}, authID string) bool {
	if len(allowed) == 0 {
		return true
	}
	_, ok := allowed[strings.TrimSpace(authID)]
	return ok
}

func previousResponseIDFromRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	for _, payload := range [][]byte{req.Payload, opts.OriginalRequest} {
		if len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}
		if responseID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()); responseID != "" {
			return responseID
		}
	}
	return ""
}

func affinitySessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.AuthSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	sessionID, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(sessionID)
}

func responseIDFromResponsePayload(payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	if responseID := strings.TrimSpace(gjson.GetBytes(payload, "id").String()); responseID != "" {
		return responseID
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func responseIDFromStreamChunk(payload []byte) string {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return ""
	}
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType != "response.completed" && eventType != "response.done" {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func compactRequestHasInput(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	for _, payload := range [][]byte{req.Payload, opts.OriginalRequest} {
		if len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}
		input := gjson.GetBytes(payload, "input")
		if input.Exists() {
			switch input.Type {
			case gjson.String:
				if strings.TrimSpace(input.String()) != "" {
					return true
				}
			case gjson.JSON:
				if input.IsArray() && len(input.Array()) > 0 {
					return true
				}
			default:
				if strings.TrimSpace(input.Raw) != "" && input.Raw != "null" {
					return true
				}
			}
		}
	}
	return false
}

func compactTranscriptFromPayload(reqPayload, respPayload []byte) []byte {
	if len(reqPayload) == 0 || !gjson.ValidBytes(reqPayload) || len(respPayload) == 0 || !gjson.ValidBytes(respPayload) {
		return nil
	}

	reqInput := gjson.GetBytes(reqPayload, "input")
	respOutput := gjson.GetBytes(respPayload, "output")
	if !reqInput.Exists() || !respOutput.Exists() || !respOutput.IsArray() {
		return nil
	}

	var transcript bytes.Buffer
	transcript.Grow(len(reqInput.Raw) + len(respOutput.Raw) + 32)
	transcript.WriteByte('[')
	firstItem := true
	switch {
	case reqInput.IsArray():
		reqInput.ForEach(func(_, item gjson.Result) bool {
			if strings.TrimSpace(item.Raw) == "" {
				return true
			}
			appendCompactTranscriptItem(&transcript, &firstItem, item.Raw)
			return true
		})
	case reqInput.Type == gjson.String && strings.TrimSpace(reqInput.String()) != "":
		appendCompactTranscriptItemPrefix(&transcript, &firstItem)
		writeCompactInputTextMessage(&transcript, reqInput.String())
	default:
		return nil
	}
	respOutput.ForEach(func(_, item gjson.Result) bool {
		if strings.TrimSpace(item.Raw) == "" {
			return true
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType != "message" && itemType != "function_call" && itemType != "function_call_output" && itemType != "custom_tool_call" && itemType != "custom_tool_call_output" {
			return true
		}
		appendCompactTranscriptItem(&transcript, &firstItem, item.Raw)
		return true
	})
	if firstItem {
		return nil
	}
	transcript.WriteByte(']')
	return transcript.Bytes()
}

func appendCompactTranscriptItem(out *bytes.Buffer, first *bool, raw string) {
	appendCompactTranscriptItemPrefix(out, first)
	out.WriteString(raw)
}

func appendCompactTranscriptItemPrefix(out *bytes.Buffer, first *bool) {
	if !*first {
		out.WriteByte(',')
	}
	*first = false
}

func writeCompactInputTextMessage(out *bytes.Buffer, text string) {
	out.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":`)
	buf := out.AvailableBuffer()
	buf = strconv.AppendQuote(buf, text)
	out.Write(buf)
	out.WriteString(`}]}`)
}

func (m *Manager) lookupCompactTranscript(responseID string) []byte {
	if m == nil {
		return nil
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil
	}
	m.responseBindingsMu.Lock()
	defer m.responseBindingsMu.Unlock()
	if raw, ok := m.responseCompacts[responseID]; ok && len(raw) > 0 {
		m.touchCompactTranscriptLocked(responseID)
		return bytes.Clone(raw)
	}
	return nil
}

func (m *Manager) bindCompactTranscript(responseID string, transcript []byte) {
	if m == nil {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" || len(transcript) == 0 || !gjson.ValidBytes(transcript) {
		return
	}
	m.responseBindingsMu.Lock()
	defer m.responseBindingsMu.Unlock()
	maxBytes, maxEntrySize, maxEntries := m.responseCompactLimits()
	if len(transcript) > maxEntrySize {
		m.removeCompactTranscriptLocked(responseID)
		return
	}
	if m.responseCompacts == nil {
		m.responseCompacts = make(map[string][]byte)
	}
	if m.responseCompactSeq == nil {
		m.responseCompactSeq = make(map[string]uint64)
	}
	m.removeCompactTranscriptLocked(responseID)
	m.responseCompacts[responseID] = bytes.Clone(transcript)
	m.responseCompactsTotalBytes += len(transcript)
	m.touchCompactTranscriptLocked(responseID)
	m.evictCompactTranscriptsLocked(maxBytes, maxEntries)
}

func (m *Manager) responseCompactLimits() (int, int, int) {
	maxBytes := m.responseCompactMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultResponseCompactMaxBytes
	}
	maxEntrySize := m.responseCompactMaxEntrySize
	if maxEntrySize <= 0 {
		maxEntrySize = defaultResponseCompactMaxEntrySize
	}
	maxEntries := m.responseCompactMaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultResponseCompactMaxEntries
	}
	return maxBytes, maxEntrySize, maxEntries
}

func (m *Manager) touchCompactTranscriptLocked(responseID string) {
	if m.responseCompactSeq == nil {
		m.responseCompactSeq = make(map[string]uint64)
	}
	m.responseCompactNextSeq++
	m.responseCompactSeq[responseID] = m.responseCompactNextSeq
}

func (m *Manager) removeCompactTranscriptLocked(responseID string) bool {
	if raw, ok := m.responseCompacts[responseID]; ok {
		m.responseCompactsTotalBytes -= len(raw)
		if m.responseCompactsTotalBytes < 0 {
			m.responseCompactsTotalBytes = 0
		}
		delete(m.responseCompacts, responseID)
		delete(m.responseCompactSeq, responseID)
		return true
	}
	delete(m.responseCompactSeq, responseID)
	return false
}

func (m *Manager) evictCompactTranscriptsLocked(maxBytes, maxEntries int) {
	for (len(m.responseCompacts) > maxEntries || m.responseCompactsTotalBytes > maxBytes) && len(m.responseCompacts) > 0 {
		oldestID := ""
		var oldestSeq uint64
		for responseID := range m.responseCompacts {
			seq := m.responseCompactSeq[responseID]
			if oldestID == "" || seq < oldestSeq {
				oldestID = responseID
				oldestSeq = seq
			}
		}
		if oldestID == "" {
			return
		}
		m.removeCompactTranscriptLocked(oldestID)
		delete(m.responseBindings, oldestID)
	}
}

func (m *Manager) expandPreviousResponseRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Request, error) {
	responseID := previousResponseIDFromRequest(req, opts)
	if responseID == "" {
		return req, nil
	}
	transcript := m.lookupCompactTranscript(responseID)
	if len(transcript) == 0 {
		if strings.EqualFold(strings.TrimSpace(opts.Alt), "responses/compact") {
			return req, nil
		}
		return req, newPreviousResponseNotFoundError(responseID)
	}
	rewritten := req
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		payload = []byte(`{}`)
	} else {
		payload = bytes.Clone(payload)
	}
	currentInput := gjson.GetBytes(payload, "input")
	expandedInput := transcript
	if currentInput.Exists() {
		if merged, ok := mergeResponseTranscriptInput(transcript, currentInput); ok {
			expandedInput = merged
		} else if !strings.EqualFold(strings.TrimSpace(opts.Alt), "responses/compact") {
			return req, newPreviousResponseNotFoundError(responseID)
		}
	} else if !strings.EqualFold(strings.TrimSpace(opts.Alt), "responses/compact") {
		return req, newPreviousResponseNotFoundError(responseID)
	}
	payload, _ = sjson.DeleteBytes(payload, "previous_response_id")
	expandedInput = sanitizeResponseTranscriptToolPairs(expandedInput)
	payload, _ = sjson.SetRawBytes(payload, "input", expandedInput)
	rewritten.Payload = payload
	return rewritten, nil
}

func newPreviousResponseNotFoundError(responseID string) error {
	responseID = strings.TrimSpace(responseID)
	body := []byte(`{"error":{"code":"previous_response_not_found","message":"","type":"invalid_request_error","param":"previous_response_id"}}`)
	message := "Previous response not found."
	if responseID != "" {
		message = fmt.Sprintf("Previous response with id %q not found.", responseID)
	}
	body, _ = sjson.SetBytes(body, "error.message", message)
	return &Error{HTTPStatus: http.StatusBadRequest, Code: "previous_response_not_found", Message: string(body)}
}

func mergeResponseTranscriptInput(transcript []byte, input gjson.Result) ([]byte, bool) {
	if len(transcript) == 0 || !gjson.ValidBytes(transcript) || !input.Exists() {
		return nil, false
	}
	var merged bytes.Buffer
	merged.Grow(len(transcript) + len(input.Raw) + 32)
	merged.WriteByte('[')
	firstItem := true
	gjson.ParseBytes(transcript).ForEach(func(_, item gjson.Result) bool {
		if strings.TrimSpace(item.Raw) == "" {
			return true
		}
		appendCompactTranscriptItem(&merged, &firstItem, item.Raw)
		return true
	})
	switch {
	case input.IsArray():
		input.ForEach(func(_, item gjson.Result) bool {
			if strings.TrimSpace(item.Raw) == "" {
				return true
			}
			appendCompactTranscriptItem(&merged, &firstItem, item.Raw)
			return true
		})
	case input.Type == gjson.String && strings.TrimSpace(input.String()) != "":
		appendCompactTranscriptItemPrefix(&merged, &firstItem)
		writeCompactInputTextMessage(&merged, input.String())
	default:
		return nil, false
	}
	if firstItem {
		return nil, false
	}
	merged.WriteByte(']')
	return merged.Bytes(), true
}

func sanitizeResponseTranscriptToolPairs(input []byte) []byte {
	if len(input) == 0 {
		return input
	}
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return input
	}
	if len(items) == 0 {
		return input
	}

	callPresent := make(map[string]struct{}, len(items))
	outputPresent := make(map[string]struct{}, len(items))
	for _, item := range items {
		itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
		callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
		if callID == "" {
			continue
		}
		switch {
		case isResponseTranscriptToolCallType(itemType):
			callPresent[callID] = struct{}{}
		case isResponseTranscriptToolOutputType(itemType):
			outputPresent[callID] = struct{}{}
		}
	}
	if len(callPresent) == 0 && len(outputPresent) == 0 {
		return input
	}

	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
		switch {
		case isResponseTranscriptToolCallType(itemType):
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID == "" {
				continue
			}
			if _, ok := outputPresent[callID]; !ok {
				continue
			}
			filtered = append(filtered, item)
		case isResponseTranscriptToolOutputType(itemType):
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID == "" {
				continue
			}
			if _, ok := callPresent[callID]; !ok {
				continue
			}
			filtered = append(filtered, item)
		default:
			filtered = append(filtered, item)
		}
	}

	out, err := json.Marshal(filtered)
	if err != nil {
		return input
	}
	return out
}

func isResponseTranscriptToolCallType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func isResponseTranscriptToolOutputType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call_output", "custom_tool_call_output":
		return true
	default:
		return false
	}
}

func (m *Manager) lookupBoundAuthID(responseID string) string {
	if m == nil {
		return ""
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ""
	}
	m.responseBindingsMu.Lock()
	defer m.responseBindingsMu.Unlock()
	return strings.TrimSpace(m.responseBindings[responseID])
}

func (m *Manager) lookupSessionBoundAuthID(sessionID string) string {
	if m == nil {
		return ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	m.sessionBindingsMu.Lock()
	defer m.sessionBindingsMu.Unlock()
	return strings.TrimSpace(m.sessionBindings[sessionID])
}

func (m *Manager) bindResponseToAuth(responseID, authID string) {
	if m == nil {
		return
	}
	responseID = strings.TrimSpace(responseID)
	authID = strings.TrimSpace(authID)
	if responseID == "" || authID == "" {
		return
	}
	m.responseBindingsMu.Lock()
	if m.responseBindings == nil {
		m.responseBindings = make(map[string]string)
	}
	m.responseBindings[responseID] = authID
	m.responseBindingsMu.Unlock()
}

func (m *Manager) bindSessionToAuth(sessionID, authID string) {
	if m == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if sessionID == "" || authID == "" {
		return
	}
	m.sessionBindingsMu.Lock()
	if m.sessionBindings == nil {
		m.sessionBindings = make(map[string]string)
	}
	if _, exists := m.sessionBindings[sessionID]; !exists && len(m.sessionBindings) >= defaultSessionBindingMaxEntries {
		m.sessionBindings = make(map[string]string)
	}
	m.sessionBindings[sessionID] = authID
	m.sessionBindingsMu.Unlock()
}

func (m *Manager) bindSessionFromMetadata(meta map[string]any, authID string) {
	if m == nil {
		return
	}
	sessionID := affinitySessionIDFromMetadata(meta)
	if sessionID == "" {
		return
	}
	m.bindSessionToAuth(sessionID, authID)
}

func (m *Manager) unbindSessionFromMetadata(meta map[string]any, authID string) {
	if m == nil {
		return
	}
	sessionID := affinitySessionIDFromMetadata(meta)
	if sessionID == "" {
		return
	}
	m.unbindSessionIfAuthMatches(sessionID, authID)
}

func (m *Manager) unbindSessionIfAuthMatches(sessionID, authID string) {
	if m == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if sessionID == "" || authID == "" {
		return
	}
	m.sessionBindingsMu.Lock()
	defer m.sessionBindingsMu.Unlock()
	if strings.TrimSpace(m.sessionBindings[sessionID]) == authID {
		delete(m.sessionBindings, sessionID)
	}
}

func (m *Manager) unbindResponsesForAuth(authID string) int {
	if m == nil {
		return 0
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	m.responseBindingsMu.Lock()
	defer m.responseBindingsMu.Unlock()
	removed := 0
	for responseID, boundAuthID := range m.responseBindings {
		if strings.TrimSpace(boundAuthID) != authID {
			continue
		}
		delete(m.responseBindings, responseID)
		m.removeCompactTranscriptLocked(responseID)
		removed++
	}
	return removed
}

func (m *Manager) unpinResponsesForAuth(authID string) int {
	if m == nil {
		return 0
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	m.responseBindingsMu.Lock()
	defer m.responseBindingsMu.Unlock()
	removed := 0
	for responseID, boundAuthID := range m.responseBindings {
		if strings.TrimSpace(boundAuthID) != authID {
			continue
		}
		delete(m.responseBindings, responseID)
		removed++
	}
	return removed
}

func (m *Manager) bindResponseFromPayload(authID string, reqPayload []byte, respPayload []byte) {
	responseID := responseIDFromResponsePayload(respPayload)
	if responseID == "" {
		return
	}
	m.bindResponseToAuth(responseID, authID)
	if transcript := compactTranscriptFromPayload(reqPayload, respPayload); len(transcript) > 0 {
		m.bindCompactTranscript(responseID, transcript)
	}
}

func (m *Manager) bindResponseFromStreamResult(authID string, reqPayload []byte, result *cliproxyexecutor.StreamResult) *cliproxyexecutor.StreamResult {
	if m == nil || result == nil || result.Chunks == nil {
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		streamOutput := responseStreamTranscriptAccumulator{}
		for chunk := range result.Chunks {
			if chunk.Err == nil {
				payload := bytes.TrimSpace(chunk.Payload)
				if bytes.HasPrefix(payload, []byte("data:")) {
					payload = bytes.TrimSpace(payload[len("data:"):])
				}
				streamOutput.recordFromEvent(payload)
				if responseID := responseIDFromStreamChunk(chunk.Payload); responseID != "" {
					m.bindResponseToAuth(responseID, authID)
					responsePayload := []byte(gjson.GetBytes(payload, "response").Raw)
					responsePayload = streamOutput.mergeIntoResponse(responsePayload)
					if transcript := compactTranscriptFromPayload(reqPayload, responsePayload); len(transcript) > 0 {
						m.bindCompactTranscript(responseID, transcript)
					}
				}
			}
			out <- chunk
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

type responseStreamTranscriptAccumulator struct {
	items []json.RawMessage
	seen  map[string]int
}

func (a *responseStreamTranscriptAccumulator) recordFromEvent(payload []byte) {
	if a == nil || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() {
		return
	}
	itemType := strings.TrimSpace(item.Get("type").String())
	if itemType != "message" && !isResponseTranscriptToolCallType(itemType) && !isResponseTranscriptToolOutputType(itemType) {
		return
	}
	a.record(json.RawMessage(item.Raw))
}

func (a *responseStreamTranscriptAccumulator) mergeIntoResponse(responsePayload []byte) []byte {
	if a == nil || len(a.items) == 0 || len(responsePayload) == 0 || !gjson.ValidBytes(responsePayload) {
		return responsePayload
	}
	merged := responseStreamTranscriptAccumulator{}
	for _, item := range a.items {
		merged.record(item)
	}
	output := gjson.GetBytes(responsePayload, "output")
	if output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			if strings.TrimSpace(item.Raw) != "" {
				merged.record(json.RawMessage(item.Raw))
			}
			return true
		})
	}
	if len(merged.items) == 0 {
		return responsePayload
	}
	raw, err := json.Marshal(merged.items)
	if err != nil {
		return responsePayload
	}
	updated, err := sjson.SetRawBytes(responsePayload, "output", raw)
	if err != nil {
		return responsePayload
	}
	return updated
}

func (a *responseStreamTranscriptAccumulator) record(item json.RawMessage) {
	if a == nil || len(item) == 0 || !gjson.ValidBytes(item) {
		return
	}
	key := responseTranscriptOutputItemKey(item)
	if key == "" {
		key = string(item)
	}
	if a.seen == nil {
		a.seen = make(map[string]int)
	}
	if idx, ok := a.seen[key]; ok {
		a.items[idx] = bytes.Clone(item)
		return
	}
	a.seen[key] = len(a.items)
	a.items = append(a.items, bytes.Clone(item))
}

func responseTranscriptOutputItemKey(item []byte) string {
	itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
	id := strings.TrimSpace(gjson.GetBytes(item, "id").String())
	if id != "" {
		return itemType + "\x00id\x00" + id
	}
	callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
	if callID != "" {
		return itemType + "\x00call_id\x00" + callID
	}
	return ""
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	retry := int(m.requestRetry.Load())
	maxCreds := int(m.maxRetryCredentials.Load())
	maxWait := time.Duration(m.maxRetryInterval.Load())
	if retry == 0 && maxCreds == 0 && maxWait == 0 {
		return defaultRequestRetry, defaultMaxRetryCredentials, defaultMaxRetryInterval
	}
	return retry, maxCreds, maxWait
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		blocked, reason, next := isAuthBlockedForModel(auth, checkModel, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) retryAllowed(attempt int, providers []string) bool {
	if m == nil || attempt < 0 || len(providers) == 0 {
		return false
	}
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt < effectiveRetry {
			return true
		}
	}
	return false
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if isModelCapacityOrOverloadedError(err) {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	retryAfter := retryAfterFromError(err)
	if retryAfter == nil || *retryAfter <= 0 || *retryAfter > maxWait {
		return 0, false
	}
	return *retryAfter, true
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func credentialRetryLimitReached(maxRetryCredentials, usageLimitRetryCredentials, attempted int, lastErr error) bool {
	if isModelCapacityOrOverloadedError(lastErr) {
		capLimit := modelCapacityRetryCredentialCap
		if usageLimitRetryCredentials > 0 && usageLimitRetryCredentials < capLimit {
			capLimit = usageLimitRetryCredentials
		}
		return attempted >= capLimit
	}
	if shouldBypassCredentialRetryLimit(lastErr) {
		if usageLimitRetryCredentials > 0 {
			return attempted >= usageLimitRetryCredentials
		}
		return maxRetryCredentials > 0 && attempted >= maxRetryCredentials
	}
	return maxRetryCredentials > 0 && attempted >= maxRetryCredentials
}

func (m *Manager) usageLimitRetryCredentialCap(providers []string, routeModel string, opts cliproxyexecutor.Options) int {
	return len(m.selectionScopeAuthIDs(providers, routeModel, opts))
}

func (m *Manager) selectionScopeAuthIDs(providers []string, routeModel string, opts cliproxyexecutor.Options) map[string]struct{} {
	return m.selectionScopeAuthIDsFiltered(providers, routeModel, opts, false)
}

func (m *Manager) imageFallbackSelectionScopeAuthIDs(providers []string, routeModel string, opts cliproxyexecutor.Options) map[string]struct{} {
	return m.selectionScopeAuthIDsFiltered(providers, routeModel, opts, true)
}

func (m *Manager) selectionScopeAuthIDsFiltered(providers []string, routeModel string, opts cliproxyexecutor.Options, imageFallbackNormalOnly bool) map[string]struct{} {
	if m == nil || len(providers) == 0 {
		return nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	if responseBindingPinnedAuthFromMetadata(opts.Metadata) || sessionBindingPinnedAuthFromMetadata(opts.Metadata) {
		pinnedAuthID = ""
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil
	}

	modelKey := strings.TrimSpace(routeModel)
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}

	now := time.Now()
	registryRef := registry.GetGlobalRegistry()
	allowed := make(map[string]struct{})
	dedicatedImageAllowed := make(map[string]struct{})
	ordinaryImageAllowed := make(map[string]struct{})
	ordinaryIdleImageAllowed := make(map[string]struct{})
	imageRequest := imageGenerationRequestFromMetadata(opts.Metadata)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, routeModel) {
			continue
		}
		checkModel := m.selectionModelForAuth(candidate, routeModel)
		blocked, _, _ := isAuthBlockedForModel(candidate, checkModel, now)
		if blocked {
			continue
		}
		if imageRequest {
			imageInflight := m.imageAuthInflightCount(candidate.ID)
			if imageInflight >= imageAuthConcurrencyLimit() {
				continue
			}
			if candidate.ImageGenerationOnly() {
				if !imageFallbackNormalOnly {
					dedicatedImageAllowed[candidate.ID] = struct{}{}
				}
				continue
			}
			ordinaryImageAllowed[candidate.ID] = struct{}{}
			if imageInflight == 0 {
				ordinaryIdleImageAllowed[candidate.ID] = struct{}{}
			}
			continue
		}
		if candidate.ImageGenerationOnly() {
			continue
		}
		if freePlanModelBlocked(candidate, routeModel) {
			continue
		}
		allowed[candidate.ID] = struct{}{}
	}
	if imageRequest {
		switch {
		case !imageFallbackNormalOnly && len(dedicatedImageAllowed) > 0:
			allowed = dedicatedImageAllowed
		case len(ordinaryIdleImageAllowed) > 0:
			allowed = ordinaryIdleImageAllowed
		default:
			allowed = ordinaryImageAllowed
		}
	}
	if excluded := retryExcludedAuthIDsFromMetadata(opts.Metadata); len(excluded) > 0 && len(allowed) > 1 {
		remaining := len(allowed)
		for authID := range excluded {
			if _, ok := allowed[authID]; ok {
				remaining--
			}
		}
		if remaining > 0 {
			for authID := range excluded {
				delete(allowed, authID)
			}
		}
	}
	return allowed
}

func shouldBypassCredentialRetryLimit(err error) bool {
	if err == nil {
		return false
	}
	return isUsageLimitReachedError(resultErrorFromError(err))
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	modelQuotaRecoverAt := time.Time{}
	modelQuotaTargets := []string(nil)
	modelSupportFailure := false
	authChanged := false
	unbindResponseAuthID := ""
	rebindProxyAuthID := ""
	var authSnapshot *Auth
	terminalAuthSnapshot := (*Auth)(nil)
	terminalWarning := ""
	terminallyDisabled := false
	terminallyDeleted := false
	pendingActivationCooldownApplied := false
	var cyberAudit *cyberPolicyAuditRecord
	shouldPersist := false

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()

		if !result.Success {
			imageUnsupportedRegion := isImageUnsupportedRegionResult(result)
			if !imageUnsupportedRegion {
				if handled, deleteAuth, reason := codexPendingActivationUnauthorizedDecision(auth, result.Error, now); handled && deleteAuth {
					applyTerminalAuthDisabledState(auth, result.Error, reason, now)
					terminalAuthSnapshot = auth.Clone()
					terminalWarning = reason
					terminallyDisabled = true
					terminallyDeleted = true
					authChanged = true
					delete(m.auths, result.AuthID)
				} else if handled {
					applyCodexPendingActivationCooldown(auth, result.Error, now)
					pendingActivationCooldownApplied = true
					authChanged = true
				} else if deleteAuth, reason := shouldDeleteRevokedAuth(auth, result.Error, result.SkipAccessTokenRefresh); deleteAuth {
					applyTerminalAuthDisabledState(auth, result.Error, reason, now)
					terminalAuthSnapshot = auth.Clone()
					terminalWarning = reason
					terminallyDisabled = true
					terminallyDeleted = true
					authChanged = true
					delete(m.auths, result.AuthID)
				} else if disableAuth, reason := shouldDisableRevokedAuth(auth, result.Error, result.SkipAccessTokenRefresh); disableAuth {
					applyTerminalAuthDisabledState(auth, result.Error, reason, now)
					terminalAuthSnapshot = auth.Clone()
					terminalWarning = reason
					terminallyDisabled = true
					authChanged = true
				}
			}
		}

		if terminallyDisabled || pendingActivationCooldownApplied {
			// Auth already handled by terminal or pending-activation state.
		} else if result.Success {
			if result.Model != "" {
				modelStateChanged := false
				state := lookupModelState(auth, result.Model)
				if state == nil && hasModelError(auth, now) {
					state = ensureModelState(auth, result.Model)
					modelStateChanged = true
				}
				if state != nil && !modelStateIsClean(state) && !modelStateHasActiveUsageLimitQuota(state, now) {
					resetModelState(state, now)
					modelStateChanged = true
				}
				if state != nil && modelStateIsClean(state) && !hasModelError(auth, now) {
					deleteCleanModelState(auth, result.Model)
				}
				if modelStateChanged || authNeedsSuccessCleanup(auth) {
					clearAggregatedAvailability(auth)
					updateAggregatedAvailability(auth, now)
					if !hasModelError(auth, now) {
						auth.LastError = nil
						auth.StatusMessage = ""
						auth.Status = StatusActive
					}
					auth.UpdatedAt = now
					shouldResumeModel = true
					clearModelQuota = true
					authChanged = true
				}
			} else {
				if authNeedsSuccessCleanup(auth) {
					clearAuthStateOnSuccess(auth, now)
					authChanged = true
				}
			}
		} else {
			if result.Model != "" {
				if !isRequestScopedResultError(result.Error) {
					disableCooling := quotaCooldownDisabledForAuth(auth)
					state := ensureModelState(auth, result.Model)
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						auth.LastError = cloneError(result.Error)
						auth.StatusMessage = result.Error.Message
					}
					if isImageUnsupportedRegionResult(result) {
						SetBoundProxyEntry(auth, "")
						rebindProxyAuthID = result.AuthID
					}

					statusCode := statusCodeFromResult(result.Error)
					if isTransientCooldownStatus(statusCode) {
						unbindResponseAuthID = result.AuthID
					}
					if isCyberPolicyResultError(result.Error) {
						audit := recordCyberPolicyTriggerLocked(auth, result, now, ctx)
						cyberAudit = &audit
						count := authCyberPolicyTriggerCount(auth)
						next := now.Add(cyberPolicyCooldown(count))
						state.NextRetryAfter = next
						modelQuotaRecoverAt = next
						state.Quota = QuotaState{
							Exceeded:      true,
							Reason:        "cyber_policy",
							NextRecoverAt: next,
							BackoffLevel:  count,
							UpdatedAt:     now,
						}
						applyAggregatedQuotaCooldown(auth, next, count, "cyber_policy")
						suspendReason = "cyber_policy"
						shouldSuspendModel = true
						setModelQuota = true
					} else if isModelSupportResultError(result.Error) {
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "model_not_supported"
						shouldSuspendModel = true
						modelSupportFailure = true
					} else if isUsageLimitReachedError(result.Error) {
						var next time.Time
						backoffLevel := state.Quota.BackoffLevel
						quotaReason, modelScopedQuota := usageLimitQuotaReasonForModel(auth, result.Model, result.Error)
						retryAfter := normalizedRetryAfter(statusCode, result.RetryAfter, state.StatusMessage)
						if retryAfter != nil {
							next = now.Add(*retryAfter)
						} else {
							cooldown, nextLevel := nextQuotaCooldown(backoffLevel, false)
							if cooldown > 0 {
								next = now.Add(cooldown)
							}
							backoffLevel = nextLevel
						}
						state.NextRetryAfter = next
						modelQuotaRecoverAt = next
						state.Quota = QuotaState{
							Exceeded:      true,
							Reason:        quotaReason,
							NextRecoverAt: next,
							BackoffLevel:  backoffLevel,
							UpdatedAt:     now,
						}
						if modelScopedQuota {
							modelQuotaTargets = codexUsageLimitAffectedModels(auth, result.Model, quotaReason)
							applyModelScopedQuotaToStates(auth, modelQuotaTargets, result.Error, next, quotaReason, backoffLevel, now)
						} else {
							applyAggregatedQuotaCooldown(auth, next, backoffLevel, quotaReason)
							suspendReason = "quota"
							shouldSuspendModel = true
						}
						setModelQuota = true
					} else {
						switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(authFailureModelCooldown(auth, result.Error, 30*time.Minute))
								state.NextRetryAfter = next
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(authFailureModelCooldown(auth, result.Error, 30*time.Minute))
								state.NextRetryAfter = next
								suspendReason = "payment_required"
								shouldSuspendModel = true
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(12 * time.Hour)
								state.NextRetryAfter = next
								suspendReason = "not_found"
								shouldSuspendModel = true
							}
						case 429:
							var next time.Time
							backoffLevel := state.Quota.BackoffLevel
							retryAfter := normalizedRetryAfter(statusCode, result.RetryAfter, state.StatusMessage)
							quotaExhausted := isUsageLimitReachedError(result.Error)
							effectiveDisable := disableCooling && !quotaExhausted
							quotaReason := "rate_limit"
							modelScopedQuota := false
							if quotaExhausted {
								quotaReason, modelScopedQuota = usageLimitQuotaReasonForModel(auth, result.Model, result.Error)
							}
							if !effectiveDisable {
								if retryAfter != nil {
									next = now.Add(*retryAfter)
								} else {
									cooldown, nextLevel := nextQuotaCooldown(backoffLevel, effectiveDisable)
									if cooldown > 0 {
										next = now.Add(cooldown)
									}
									backoffLevel = nextLevel
								}
							}
							state.NextRetryAfter = next
							modelQuotaRecoverAt = next
							state.Quota = QuotaState{
								Exceeded:      true,
								Reason:        quotaReason,
								NextRecoverAt: next,
								BackoffLevel:  backoffLevel,
								UpdatedAt:     now,
							}
							if !effectiveDisable {
								if !modelScopedQuota {
									applyAggregatedQuotaCooldown(auth, next, backoffLevel, quotaReason)
									suspendReason = "quota"
									shouldSuspendModel = true
								} else {
									modelQuotaTargets = codexUsageLimitAffectedModels(auth, result.Model, quotaReason)
									applyModelScopedQuotaToStates(auth, modelQuotaTargets, result.Error, next, quotaReason, backoffLevel, now)
								}
								setModelQuota = true
							}
						case 408, 500, 502, 503, 504:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(1 * time.Minute)
								state.NextRetryAfter = next
							}
						default:
							state.NextRetryAfter = time.Time{}
						}
					}

					if modelSupportFailure {
						clearAggregatedAvailability(auth)
					} else {
						updateAggregatedAvailability(auth, now)
					}
					if modelSupportFailure {
						auth.Status = StatusActive
					} else {
						auth.Status = StatusError
					}
					auth.UpdatedAt = now
					authChanged = true
				}
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
				authChanged = true
			}
		}

		if authChanged && !terminallyDeleted {
			authSnapshot = auth.Clone()
			shouldPersist = true
		}
	}
	m.mu.Unlock()

	if shouldPersist && authSnapshot != nil {
		_ = m.persist(ctx, authSnapshot)
	}

	if terminalAuthSnapshot != nil {
		if terminallyDeleted {
			m.deleteRevokedAuth(terminalAuthSnapshot, terminalWarning, "request")
		} else {
			m.markRevokedAuthDisabled(terminalAuthSnapshot, terminalWarning, "request")
		}
		m.hook.OnResult(ctx, result)
		return
	}

	if cyberAudit != nil {
		cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
		if errAudit := appendCyberPolicyAudit(cfg, *cyberAudit); errAudit != nil {
			log.Warnf("failed to append cyber policy audit for auth %s: %v", result.AuthID, errAudit)
		}
	}

	if m.scheduler != nil && authSnapshot != nil {
		m.scheduler.upsertAuth(authSnapshot)
	}
	if unbindResponseAuthID != "" {
		m.unpinResponsesForAuth(unbindResponseAuthID)
	}
	if rebindProxyAuthID != "" {
		m.KickAsyncBindingProbe(rebindProxyAuthID)
	}

	if clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if setModelQuota && result.Model != "" {
		targets := modelQuotaTargetsOrDefault(modelQuotaTargets, result.Model)
		for _, targetModel := range targets {
			if !modelQuotaRecoverAt.IsZero() {
				registry.GetGlobalRegistry().SetModelQuotaExceededUntil(result.AuthID, targetModel, modelQuotaRecoverAt)
			} else {
				registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, targetModel)
			}
		}
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}

	if !result.Success && m.quotaFlusher != nil {
		if result.Error != nil && (result.Error.StatusCode() == http.StatusTooManyRequests || isUsageLimitReachedError(result.Error)) {
			m.quotaFlusher.notifyActivity()
		}
	}

	m.hook.OnResult(ctx, result)
}

func isImageUnsupportedRegionResult(result Result) bool {
	if result.Error == nil || len(result.RequestPayload) == 0 {
		return false
	}
	if !payloadContainsImageGenerationTool(result.RequestPayload) {
		return false
	}
	return resultErrorIsUnsupportedRegion(result.Error)
}

func payloadContainsImageGenerationTool(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	tools := gjson.GetBytes(payload, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
			return true
		}
	}
	return false
}

func resultErrorIsUnsupportedRegion(err *Error) bool {
	if err == nil {
		return false
	}
	message := strings.TrimSpace(err.Message)
	code := strings.TrimSpace(gjson.Get(message, "error.code").String())
	errType := strings.TrimSpace(gjson.Get(message, "error.type").String())
	return strings.EqualFold(code, "unsupported_country_region_territory") || strings.EqualFold(errType, "request_forbidden")
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func lookupModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" || len(auth.ModelStates) == 0 {
		return nil
	}
	return auth.ModelStates[model]
}

func deleteCleanModelState(auth *Auth, model string) {
	if auth == nil || model == "" || len(auth.ModelStates) == 0 {
		return
	}
	state, ok := auth.ModelStates[model]
	if !ok || !modelStateIsClean(state) {
		return
	}
	delete(auth.ModelStates, model)
	if len(auth.ModelStates) == 0 {
		auth.ModelStates = nil
	}
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func applyAggregatedQuotaCooldown(auth *Auth, next time.Time, backoffLevel int, reason ...string) {
	if auth == nil {
		return
	}
	auth.Unavailable = true
	auth.NextRetryAfter = next
	auth.Quota.Exceeded = true
	qr := "quota"
	if len(reason) > 0 && reason[0] != "" {
		qr = reason[0]
	}
	auth.Quota.Reason = qr
	auth.Quota.NextRecoverAt = next
	auth.Quota.BackoffLevel = backoffLevel
}

func modelStateIsClean(state *ModelState) bool {
	if state == nil {
		return true
	}
	if state.Status != StatusActive {
		return false
	}
	if state.Unavailable || state.StatusMessage != "" || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		return false
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		return false
	}
	return true
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	preservedAuthCooldown := auth.Unavailable && auth.NextRetryAfter.After(now) && auth.Quota.Exceeded
	preservedNextRetry := auth.NextRetryAfter
	preservedQuotaRecover := auth.Quota.NextRecoverAt
	preservedBackoffLevel := auth.Quota.BackoffLevel
	if len(auth.ModelStates) == 0 {
		if preservedAuthCooldown {
			applyAggregatedQuotaCooldown(auth, preservedNextRetry, preservedBackoffLevel, auth.Quota.Reason)
			if !preservedQuotaRecover.IsZero() {
				auth.Quota.NextRecoverAt = preservedQuotaRecover
			}
			return
		}
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := preservedAuthCooldown
	quotaReason := auth.Quota.Reason
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	if preservedAuthCooldown {
		if preservedNextRetry.After(now) {
			earliestRetry = preservedNextRetry
		}
		if !preservedQuotaRecover.IsZero() {
			quotaRecover = preservedQuotaRecover
		} else {
			quotaRecover = preservedNextRetry
		}
		maxBackoffLevel = preservedBackoffLevel
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if modelStateIsModelScopedOnly(state) {
			continue
		}
		hasState = true
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if state.Quota.Reason != "" {
				if quotaReason == "" || state.Quota.Reason == "usage_limit" {
					quotaReason = state.Quota.Reason
				}
			}
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		if preservedAuthCooldown {
			applyAggregatedQuotaCooldown(auth, preservedNextRetry, preservedBackoffLevel, auth.Quota.Reason)
			if !preservedQuotaRecover.IsZero() {
				auth.Quota.NextRecoverAt = preservedQuotaRecover
			}
			return
		}
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = preservedAuthCooldown || allUnavailable
	if auth.Unavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		if quotaReason != "" {
			auth.Quota.Reason = quotaReason
		} else {
			auth.Quota.Reason = "quota"
		}
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func modelStateIsModelScopedOnly(state *ModelState) bool {
	if state == nil {
		return false
	}
	return state.Quota.Exceeded && (state.Quota.Reason == codexSparkUsageLimitReason || state.Quota.Reason == codexStandardUsageLimitReason)
}

func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	if auth.Quota.Exceeded && (auth.Quota.Reason == codexFiveHourQuotaLowReason || auth.Quota.Reason == codexWeeklyQuotaLowReason) {
		return
	}
	if authHasActiveUsageLimitQuota(auth, time.Now()) {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
}

func authHasActiveUsageLimitQuota(auth *Auth, now time.Time) bool {
	if auth == nil || !auth.Quota.Exceeded || auth.Quota.Reason != "usage_limit" {
		return false
	}
	next := auth.Quota.NextRecoverAt
	if next.IsZero() {
		next = auth.NextRetryAfter
	}
	return !next.IsZero() && next.After(now)
}

func modelStateHasActiveUsageLimitQuota(state *ModelState, now time.Time) bool {
	if state == nil || !state.Quota.Exceeded {
		return false
	}
	switch state.Quota.Reason {
	case "usage_limit", codexSparkUsageLimitReason, codexStandardUsageLimitReason:
	default:
		return false
	}
	next := state.Quota.NextRecoverAt
	if next.IsZero() {
		next = state.NextRetryAfter
	}
	return !next.IsZero() && next.After(now)
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Quota.Exceeded && (auth.Quota.Reason == codexFiveHourQuotaLowReason || auth.Quota.Reason == codexWeeklyQuotaLowReason) {
		auth.UpdatedAt = now
		return
	}
	if authHasActiveUsageLimitQuota(auth, now) {
		auth.UpdatedAt = now
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func authNeedsSuccessCleanup(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Unavailable || auth.LastError != nil {
		return true
	}
	if status := strings.TrimSpace(string(auth.Status)); status != "" && Status(status) != StatusActive {
		return true
	}
	if strings.TrimSpace(auth.StatusMessage) != "" {
		return true
	}
	if !auth.NextRetryAfter.IsZero() {
		return true
	}
	if auth.Quota.Exceeded || auth.Quota.Reason != "" || !auth.Quota.NextRecoverAt.IsZero() || auth.Quota.BackoffLevel != 0 {
		return true
	}
	return false
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	rap, ok := err.(retryAfterProvider)
	if ok && rap != nil {
		return normalizedRetryAfter(statusCodeFromError(err), rap.RetryAfter(), err.Error())
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 {
		return nil
	}
	return normalizedRetryAfter(statusCode, nil, err.Error())
}

func normalizedRetryAfter(statusCode int, retryAfter *time.Duration, message string) *time.Duration {
	if retryAfter != nil && *retryAfter > 0 {
		d := *retryAfter
		return &d
	}
	if parsed := usageLimitRetryAfterFromMessage(message, time.Now()); parsed != nil {
		return parsed
	}
	if statusCode != http.StatusTooManyRequests {
		return nil
	}
	if parsed := fallbackRetryAfterFor429Message(message); parsed != nil {
		return parsed
	}
	// Any 429 without a recognized pattern gets a conservative fallback
	fallback := plain429Cooldown
	return &fallback
}

func usageLimitRetryAfterFromMessage(message string, now time.Time) *time.Duration {
	body := strings.TrimSpace(message)
	if body == "" || !gjson.Valid(body) {
		return nil
	}
	if strings.TrimSpace(gjson.Get(body, "error.type").String()) != "usage_limit_reached" {
		return nil
	}
	if resetsAt := gjson.Get(body, "error.resets_at").Int(); resetsAt > 0 {
		resetAt := time.Unix(resetsAt, 0)
		if resetAt.After(now) {
			retryAfter := resetAt.Sub(now)
			return &retryAfter
		}
	}
	if resetsInSeconds := gjson.Get(body, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		retryAfter := time.Duration(resetsInSeconds) * time.Second
		return &retryAfter
	}
	return nil
}

func fallbackRetryAfterFor429Message(message string) *time.Duration {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return nil
	}
	keywords := []string{
		"rate limit exceeded",
		"too many requests",
		"requests rate limit",
		"rate limited",
		"rate_limit_exceeded",
	}
	for _, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			retryAfter := plain429Cooldown
			return &retryAfter
		}
	}
	return nil
}

func isModelCapacityOrOverloadedError(err error) bool {
	if err == nil {
		return false
	}
	return isModelCapacityOrOverloadedMessage(err.Error())
}

// IsModelCapacityOrOverloadedError reports transient upstream capacity failures
// that are already retried inside the auth manager by rotating credentials.
func IsModelCapacityOrOverloadedError(err error) bool {
	return isModelCapacityOrOverloadedError(err)
}

func isModelCapacityOrOverloadedMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"selected model is at capacity",
		"model is at capacity. please try a different model",
		"servers are currently overloaded",
		"server_is_overloaded",
		"requested model is currently unavailable",
		"current model is unavailable",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return strings.Contains(lower, "model unavailable") && strings.Contains(lower, "switch model")
}

// isUsageLimitReachedError returns true when the error represents a quota
// exhaustion (5-hour window or weekly limit) rather than a plain rate limit.
// These errors carry resets_at / resets_in_seconds and must never be skipped
// by quotaCooldownDisabled.
func isUsageLimitReachedError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	message := strings.TrimSpace(resultErr.Message)
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "usage_limit_reached") {
		return true
	}
	// Match natural language form from ChatGPT: "The usage limit has been reached"
	if strings.Contains(lower, "usage limit") && strings.Contains(lower, "reached") {
		return true
	}
	if !gjson.Valid(message) {
		return false
	}
	errorType := strings.TrimSpace(gjson.Get(message, "error.type").String())
	if strings.EqualFold(errorType, "usage_limit_reached") {
		return true
	}
	if gjson.Get(message, "error.resets_at").Int() > 0 {
		return true
	}
	if gjson.Get(message, "error.resets_in_seconds").Int() > 0 {
		return true
	}
	return false
}

func usageLimitQuotaReasonForModel(auth *Auth, model string, resultErr *Error) (string, bool) {
	if codexUsageLimitIsModelScoped(auth, model, resultErr) {
		if isCodexSparkModel(model) {
			return codexSparkUsageLimitReason, true
		}
		return codexStandardUsageLimitReason, true
	}
	return "usage_limit", false
}

func codexUsageLimitIsModelScoped(auth *Auth, model string, resultErr *Error) bool {
	if auth == nil || !isCodexAuth(auth) || strings.TrimSpace(model) == "" || !isUsageLimitReachedError(resultErr) {
		return false
	}
	if !codexPlanAllowsSparkModels(codexPlanTypeForAuth(auth)) {
		return false
	}
	return true
}

func isCodexSparkModel(model string) bool {
	modelKey := strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	if modelKey == "" {
		return false
	}
	return strings.HasSuffix(modelKey, "-codex-spark")
}

func codexPlanTypeForAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if planType := strings.TrimSpace(auth.Attributes["plan_type"]); planType != "" {
			return planType
		}
	}
	if planType := strings.TrimSpace(probedPlanType(auth)); planType != "" {
		return planType
	}
	return strings.TrimSpace(submittedPlanType(auth))
}

func codexPlanAllowsSparkModels(planType string) bool {
	switch normalizedPlanTypeKey(planType) {
	case "pro", "prolite":
		return true
	default:
		return false
	}
}

func codexUsageLimitAffectedModels(auth *Auth, model, quotaReason string) []string {
	model = strings.TrimSpace(model)
	if auth == nil || model == "" {
		return nil
	}
	if quotaReason == codexSparkUsageLimitReason {
		return []string{model}
	}
	if quotaReason != codexStandardUsageLimitReason {
		return []string{model}
	}
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	targets := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models)+1)
	for _, registered := range models {
		if registered == nil {
			continue
		}
		id := strings.TrimSpace(registered.ID)
		if id == "" || isCodexSparkModel(id) {
			continue
		}
		key := strings.ToLower(canonicalModelKey(id))
		if key == "" {
			key = strings.ToLower(id)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, id)
	}
	if len(targets) == 0 && !isCodexSparkModel(model) {
		targets = append(targets, model)
	}
	return targets
}

func applyModelScopedQuotaToStates(auth *Auth, models []string, resultErr *Error, next time.Time, reason string, backoffLevel int, now time.Time) {
	if auth == nil {
		return
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		state := ensureModelState(auth, model)
		state.Unavailable = true
		state.Status = StatusError
		state.NextRetryAfter = next
		state.UpdatedAt = now
		if resultErr != nil {
			state.LastError = cloneError(resultErr)
			state.StatusMessage = resultErr.Message
		}
		state.Quota = QuotaState{
			Exceeded:      true,
			Reason:        reason,
			NextRecoverAt: next,
			BackoffLevel:  backoffLevel,
			UpdatedAt:     now,
		}
	}
}

func modelQuotaTargetsOrDefault(targets []string, fallback string) []string {
	if len(targets) == 0 {
		fallback = strings.TrimSpace(fallback)
		if fallback == "" {
			return nil
		}
		return []string{fallback}
	}
	out := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		key := strings.ToLower(canonicalModelKey(target))
		if key == "" {
			key = strings.ToLower(target)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, target)
	}
	return out
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func setExecutionResultError(result *Result, err error) {
	if result == nil || err == nil {
		return
	}
	result.Error = resultErrorFromError(err)
	if ra := retryAfterFromError(err); ra != nil {
		result.RetryAfter = ra
	}
}

// reloginRecoveryCooldown is the short per-model cooldown applied to a 401/403
// on a creds-bearing codex auth, for which the reactive path kicks a background
// web re-login. The auth re-enters rotation right after the login lands (~20s) —
// where the ensuing successful request clears the suspend — instead of sitting
// out the full 30-minute auth-failure sideline.
const reloginRecoveryCooldown = 30 * time.Second

// authFailureModelCooldown returns how long to sideline a model after an
// auth-level failure (401/403). A creds-bearing codex auth (one the reactive
// path will re-login in the background) gets the short relogin-recovery
// cooldown; everything else gets the supplied default.
func authFailureModelCooldown(auth *Auth, resultErr *Error, deflt time.Duration) time.Duration {
	if shouldAttemptAccessTokenRefresh(auth, resultErr) {
		return reloginRecoveryCooldown
	}
	return deflt
}

// asyncReloginTimeout bounds a background web re-login kicked off the request
// hot path, so a hung login can't leak a goroutine indefinitely.
const asyncReloginTimeout = 2 * time.Minute

// kickAsyncRelogin launches a codex web re-login for a dead-token auth in the
// background and returns immediately, so the request that hit the 401 fails over
// without blocking on a multi-second login. The login runs on a context detached
// from the request (request completion / failover must not cancel it) with a
// bounded timeout, and is de-duplicated per auth by the markRefreshPending gate
// inside refreshAuthAfterAccessTokenFailure. Callers leave
// result.SkipAccessTokenRefresh false so MarkResult treats the auth as
// refresh-able (cooled, not retired) while the relogin is in flight; a
// successful background login returns it to rotation for subsequent requests.
func (m *Manager) kickAsyncRelogin(ctx context.Context, auth *Auth, resultErr *Error) {
	if m == nil || auth == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), asyncReloginTimeout)
	go func() {
		defer cancel()
		m.refreshAuthAfterAccessTokenFailure(bg, auth, resultErr)
	}()
}

func (m *Manager) refreshAuthAfterAccessTokenFailure(ctx context.Context, auth *Auth, resultErr *Error) (*Auth, bool) {
	if m == nil || !shouldAttemptAccessTokenRefresh(auth, resultErr) {
		return nil, false
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return nil, false
	}
	// Gate entry so two concurrent 401s for the same auth don't both run a
	// full login (= double re-login). markRefreshPending atomically claims the
	// refresh slot (sets NextRefreshAfter into the future under m.mu) and
	// returns false if a refresh is already pending. On false, skip the login
	// and let the caller's else-arm set SkipAccessTokenRefresh so terminal
	// logic can still fire. refreshAuth (invoked below) does not gate on
	// NextRefreshAfter and resets it after the refresh, so claiming the slot
	// here does not block the refresh we are about to run.
	if !m.markRefreshPending(authID, time.Now()) {
		return nil, false
	}
	m.mu.Lock()
	if current := m.auths[authID]; current != nil {
		if current.Metadata == nil {
			current.Metadata = make(map[string]any)
		}
		current.Metadata[MetadataCodexForceTokenRefreshKey] = true
		m.auths[authID] = current
	}
	m.mu.Unlock()

	m.refreshAuthWithLimit(ctx, authID)
	refreshed, ok := m.GetByID(authID)
	if !ok || refreshed == nil || refreshed.Disabled || refreshed.Status == StatusDisabled {
		return nil, false
	}
	return refreshed, true
}

func isTransientCooldownError(err error) bool {
	if isModelCapacityOrOverloadedError(err) {
		return true
	}
	return isTransientCooldownStatus(statusCodeFromError(err))
}

func isTransientCooldownStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func resultErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	if resultErr, ok := err.(*Error); ok && resultErr != nil {
		return cloneError(resultErr)
	}
	return &Error{
		Message:    err.Error(),
		HTTPStatus: statusCodeFromError(err),
	}
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"missing scopes: api.model.read",
		"insufficient permissions for this operation",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusForbidden && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusForbidden && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Message)
}

func isRequestScopedNotFoundMessage(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return isRequestScopedNotFoundMessage(err.Message)
}

func isRequestScopedAuthUnavailableResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(err.Code), "request_scoped_auth_unavailable")
}

func isModerationBlockedResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isModerationBlockedErrorMessage(err.Message) || strings.EqualFold(strings.TrimSpace(err.Code), "moderation_blocked")
}

func isModerationBlockedErrorMessage(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		return false
	}
	if strings.Contains(strings.ToLower(message), "moderation_blocked") {
		return true
	}
	if !gjson.Valid(message) {
		return false
	}
	paths := []string{
		"error.code",
		"code",
		"response.error.code",
	}
	for _, path := range paths {
		if strings.EqualFold(strings.TrimSpace(gjson.Get(message, path).String()), "moderation_blocked") {
			return true
		}
	}
	return false
}

func isRequestScopedResultError(err *Error) bool {
	return isRequestInvalidError(err) || isRequestScopedAuthUnavailableResultError(err)
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it treats safety-policy
// blocks, 400 responses with "invalid_request_error", request-scoped 404 item
// misses caused by `store=false`, and all 422 responses as request-shape
// failures, where switching auths or pooled upstream models will not help.
// Model-support errors are excluded so routing can fall through to another auth
// or upstream.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if isModelSupportError(err) {
		return false
	}
	if isModerationBlockedErrorMessage(err.Error()) {
		return true
	}
	if isCyberPolicyErrorMessage(err.Error()) {
		return true
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest:
		return strings.Contains(err.Error(), "invalid_request_error")
	case http.StatusNotFound:
		return isRequestScopedNotFoundMessage(err.Error())
	case http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func terminalAuthFailureReason(resultErr *Error) string {
	if resultErr == nil {
		return ""
	}
	if isModelSupportResultError(resultErr) {
		return ""
	}
	message := strings.TrimSpace(resultErr.Message)
	lower := strings.ToLower(message)
	for _, needle := range []string{
		"deactivated_workspace",
		"authorization lost",
		"forbidden",
		"unauthorized",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"refresh token has been invalidated",
		"refresh token has already been used",
		"account_deactivated",
		"must be a member of an organization to use the api",
		"encountered invalidated oauth token for user",
		"your authentication token has been invalidated. please try signing in again.",
		"token_revoked",
		"token_invalidated",
		"your openai account has been deactivated",
		"account has been deactivated",
		"account deactivated",
	} {
		if strings.Contains(lower, needle) {
			if message != "" {
				return message
			}
			return needle
		}
	}
	switch resultErr.StatusCode() {
	case http.StatusUnauthorized:
		if message != "" {
			return message
		}
		return "unauthorized"
	case http.StatusForbidden:
		if message != "" {
			return message
		}
		return "forbidden"
	default:
		return ""
	}
}

func isRefreshTokenReusedResultError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(resultErr.Message))
	return strings.Contains(lower, "refresh_token_reused") || strings.Contains(lower, "refresh token has already been used")
}

func shouldAttemptAccessTokenRefresh(auth *Auth, resultErr *Error) bool {
	if auth == nil || resultErr == nil {
		return false
	}
	if !isCodexAuth(auth) {
		return false
	}
	// Only attempt for auth/token errors (401/403), not quota/5xx.
	switch resultErr.StatusCode() {
	case http.StatusUnauthorized, http.StatusForbidden:
	default:
		return false
	}
	// Must carry login creds to recover (creds embedded in the auth file).
	m := auth.Metadata
	if m == nil {
		return false
	}
	email, _ := m["email"].(string)
	pw, _ := m["openai_password"].(string)
	totp, _ := m["totp_secret"].(string)
	cid, _ := m["oauth2_client_id"].(string)
	rt, _ := m["oauth2_refresh_token"].(string)
	hasPwTotp := email != "" && pw != "" && totp != ""
	hasPwOnly := email != "" && pw != ""
	hasMail := email != "" && cid != "" && rt != ""
	return hasPwTotp || hasPwOnly || hasMail
}

func codexPendingActivationUnauthorizedDecision(auth *Auth, resultErr *Error, now time.Time) (bool, bool, string) {
	if !isCodexPendingActivationAuth(auth) || !isPlainUnauthorizedResultError(resultErr) {
		return false, false, ""
	}
	if shouldDeleteCodexPendingActivationAuth(auth, now, DefaultPendingActivationDeletionGrace) {
		return true, true, fmt.Sprintf("%s_expired: age>=%s", codexPendingActivationReason, DefaultPendingActivationDeletionGrace)
	}
	return true, false, codexPendingActivationReason
}

func isPlainUnauthorizedResultError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(resultErr.Message))
	for _, terminalNeedle := range []string{
		"refresh_token_reused",
		"refresh_token_invalidated",
		"refresh token has been invalidated",
		"refresh token has already been used",
		"token_revoked",
		"token_invalidated",
		"authorization lost",
		"forbidden",
		"deactivated_workspace",
		"account_deactivated",
		"account has been deactivated",
		"encountered invalidated oauth token for user",
		"must be a member of an organization to use the api",
	} {
		if strings.Contains(message, terminalNeedle) {
			return false
		}
	}
	if resultErr.StatusCode() == http.StatusUnauthorized {
		return true
	}
	return strings.Contains(message, `"detail":"unauthorized"`) ||
		strings.Contains(message, `"detail": "unauthorized"`) ||
		message == "unauthorized"
}

func shouldDeleteRevokedAuth(auth *Auth, resultErr *Error, skipAccessTokenRefresh bool) (bool, string) {
	if !isCodexAuth(auth) || !hasPersistedAuthRecord(auth) {
		return false, ""
	}
	if handled, deleteAuth, reason := codexPendingActivationUnauthorizedDecision(auth, resultErr, time.Now()); handled {
		return deleteAuth, reason
	}
	if !skipAccessTokenRefresh && shouldAttemptAccessTokenRefresh(auth, resultErr) {
		return false, ""
	}
	reason := terminalAuthFailureReason(resultErr)
	return reason != "", reason
}

func shouldDisableRevokedAuth(auth *Auth, resultErr *Error, skipAccessTokenRefresh bool) (bool, string) {
	if !isCodexAuth(auth) || auth == nil {
		return false, ""
	}
	if handled, _, _ := codexPendingActivationUnauthorizedDecision(auth, resultErr, time.Now()); handled {
		return false, ""
	}
	if !skipAccessTokenRefresh && shouldAttemptAccessTokenRefresh(auth, resultErr) {
		return false, ""
	}
	if isRefreshTokenReusedResultError(resultErr) && hasPersistedAuthRecord(auth) {
		return false, ""
	}
	reason := terminalAuthFailureReason(resultErr)
	return reason != "", reason
}

func isCodexAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return true
	}
	if auth.Attributes != nil {
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["provider_key"]), "codex") {
			return true
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["type"].(string); ok && strings.EqualFold(strings.TrimSpace(value), "codex") {
			return true
		}
	}
	return false
}

func hasPersistedAuthRecord(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			return true
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		return true
	}
	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return false
	}
	lowerID := strings.ToLower(id)
	return strings.HasSuffix(lowerID, ".json") || filepath.IsAbs(id) || strings.ContainsAny(id, `/\`)
}

func authPathSuffix(auth *Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	path := strings.TrimSpace(auth.Attributes["path"])
	if path == "" {
		return ""
	}
	return " (" + path + ")"
}

func applyTerminalAuthDisabledState(auth *Auth, resultErr *Error, reason string, now time.Time) {
	if auth == nil {
		return
	}
	ClearIPv6BindLease(auth)
	auth.Disabled = true
	auth.Unavailable = false
	auth.Status = StatusDisabled
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
	auth.UpdatedAt = now
	auth.LastError = cloneError(resultErr)
	if reason != "" {
		auth.StatusMessage = reason
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		resetModelState(state, now)
	}
}

func (m *Manager) deleteRevokedAuth(auth *Auth, warning string, source string) {
	if m == nil || auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	if source == "" {
		source = "request"
	}
	ClearIPv6BindLease(auth)
	RegisterRevokedAuthTombstone(auth, warning, time.Now())
	deleteCtx, cancel := context.WithTimeout(context.Background(), revokedDeleteTimeout)
	removed := m.removeAuthRuntime(authID)
	if removed == nil {
		m.finalizeAuthRemoval(auth, authID)
		removed = auth.Clone()
	}
	deleteErr := m.deletePersistedAuthRecord(deleteCtx, removed, authID)
	cancel()
	if removed != nil {
		auth = removed
	}
	if deleteErr != nil {
		log.Warnf("removed revoked auth %s%s from runtime after terminal auth failure during %s, but failed to delete from store: %v", authID, authPathSuffix(auth), source, deleteErr)
		return
	}
	if err := appendBanRecord(auth, warning, source, time.Now()); err != nil {
		log.Warnf("deleted revoked auth %s%s after terminal auth failure during %s, but failed to append ban record: %v", authID, authPathSuffix(auth), source, err)
	}
	log.Warnf("deleted revoked auth %s%s after terminal auth failure during %s: %s", authID, authPathSuffix(auth), source, warning)
}

func (m *Manager) markRevokedAuthDisabled(auth *Auth, warning string, source string) {
	if m == nil || auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	if source == "" {
		source = "request"
	}
	RegisterRevokedAuthTombstone(auth, warning, time.Now())
	m.invalidateRouteAwareProviderCacheForAuthTransition(auth, nil)
	m.syncAPIKeyModelAliasForAuthTransition(auth, nil)
	if m.scheduler != nil {
		m.scheduler.removeAuth(authID)
	}
	registry.GetGlobalRegistry().UnregisterClient(authID)
	if err := appendBanRecord(auth, warning, source, time.Now()); err != nil {
		log.Warnf("disabled revoked auth %s%s after terminal auth failure during %s, but failed to append ban record: %v", authID, authPathSuffix(auth), source, err)
	}
	log.Warnf("disabled revoked auth %s%s after terminal auth failure during %s: %s", authID, authPathSuffix(auth), source, warning)
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	if isRequestScopedNotFoundResultError(resultErr) {
		return
	}
	disableCooling := quotaCooldownDisabledForAuth(auth)
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	if isUsageLimitReachedError(resultErr) {
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "usage_limit"
		var next time.Time
		message := auth.StatusMessage
		if resultErr != nil && strings.TrimSpace(resultErr.Message) != "" {
			message = resultErr.Message
		}
		retryAfter = normalizedRetryAfter(statusCode, retryAfter, message)
		if retryAfter != nil {
			next = now.Add(*retryAfter)
		} else {
			cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, false)
			if cooldown > 0 {
				next = now.Add(cooldown)
			}
			auth.Quota.BackoffLevel = nextLevel
		}
		auth.Quota.NextRecoverAt = next
		auth.Quota.UpdatedAt = now
		auth.NextRetryAfter = next
		return
	}
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 402, 403:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		quotaExhausted := isUsageLimitReachedError(resultErr)
		effectiveDisable := disableCooling && !quotaExhausted
		if quotaExhausted {
			auth.Quota.Reason = "usage_limit"
		} else {
			auth.Quota.Reason = "rate_limit"
		}
		var next time.Time
		message := auth.StatusMessage
		if resultErr != nil && strings.TrimSpace(resultErr.Message) != "" {
			message = resultErr.Message
		}
		retryAfter = normalizedRetryAfter(statusCode, retryAfter, message)
		if !effectiveDisable {
			if retryAfter != nil {
				next = now.Add(*retryAfter)
			} else {
				cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, effectiveDisable)
				if cooldown > 0 {
					next = now.Add(cooldown)
				}
				auth.Quota.BackoffLevel = nextLevel
			}
		}
		auth.Quota.NextRecoverAt = next
		auth.Quota.UpdatedAt = now
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

func applyCodexPendingActivationCooldown(auth *Auth, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	next := now.Add(DefaultPendingActivationProbeInterval)
	auth.Unavailable = true
	if auth.Status != StatusDisabled {
		auth.Status = StatusError
	}
	auth.StatusMessage = codexPendingActivationReason
	auth.NextRetryAfter = next
	auth.Quota = QuotaState{
		Exceeded:      true,
		Reason:        codexPendingActivationReason,
		NextRecoverAt: next,
		UpdatedAt:     now,
	}
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
	}
	auth.UpdatedAt = now
}

func applyCodexFiveHourQuotaRest(auth *Auth, now time.Time) {
	if auth == nil || !isCodexAuth(auth) {
		return
	}
	if isCodexPendingActivationAuth(auth) {
		applyCodexPendingActivationCooldown(auth, nil, now)
		return
	}
	if ratio, okWeekly := codexWeeklyRemainingRatio(auth); okWeekly && ratio <= 0 {
		next := codexWeeklyResetAt(auth)
		if next.IsZero() || !next.After(now) {
			next = now.Add(codexColdRefreshInterval)
		}
		auth.Unavailable = true
		auth.Status = StatusError
		auth.StatusMessage = "codex_weekly_quota_low: remaining_ratio=" + strconv.FormatFloat(ratio, 'f', 2, 64)
		auth.NextRetryAfter = next
		auth.Quota = QuotaState{
			Exceeded:      true,
			Reason:        codexWeeklyQuotaLowReason,
			NextRecoverAt: next,
			UpdatedAt:     now,
		}
		return
	}
	ratio, ok := codexFiveHourRemainingRatio(auth)
	if ok && ratio < codexFiveHourQuotaRestThreshold {
		next := codexFiveHourResetAt(auth)
		if next.IsZero() || !next.After(now) {
			next = now.Add(codexColdRefreshInterval)
		}
		auth.Unavailable = true
		auth.Status = StatusError
		auth.StatusMessage = "codex_5h_quota_low: remaining_ratio=" + strconv.FormatFloat(ratio, 'f', 2, 64)
		auth.NextRetryAfter = next
		auth.Quota = QuotaState{
			Exceeded:      true,
			Reason:        codexFiveHourQuotaLowReason,
			NextRecoverAt: next,
			UpdatedAt:     now,
		}
		return
	}
	if codexQuotaRefreshRecoveredState(auth, ok, now) {
		auth.Unavailable = false
		auth.Status = StatusActive
		auth.StatusMessage = ""
		auth.NextRetryAfter = time.Time{}
		auth.Quota = QuotaState{}
	}
}

func codexQuotaRefreshRecoveredState(auth *Auth, hasFiveHourRatio bool, now time.Time) bool {
	if auth == nil {
		return false
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == codexWeeklyQuotaLowReason {
		return true
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == codexPendingActivationReason {
		return !isCodexPendingActivationAuth(auth)
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == codexFiveHourQuotaLowReason {
		return hasFiveHourRatio
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "usage_limit" {
		next := auth.Quota.NextRecoverAt
		if next.IsZero() {
			next = auth.NextRetryAfter
		}
		return !next.IsZero() && !next.After(now)
	}
	return auth.Unavailable && auth.Status == StatusError && strings.Contains(strings.ToLower(strings.TrimSpace(auth.StatusMessage)), "quota")
}

func codexFiveHourRemainingRatio(auth *Auth) (float64, bool) {
	if auth == nil || len(auth.Metadata) == 0 {
		return 0, false
	}
	return floatFromMetadata(auth.Metadata,
		MetadataCodexFiveHourQuotaRemainingRatioKey,
		"codex_5h_remaining_ratio",
		"five_hour_remaining_ratio",
		"5h_remaining_ratio",
	)
}

func codexFiveHourResetAt(auth *Auth) time.Time {
	if auth == nil || len(auth.Metadata) == 0 {
		return time.Time{}
	}
	if ts, ok := lookupMetadataTime(auth.Metadata,
		MetadataCodexFiveHourQuotaResetAtKey,
		"codex_5h_reset_at",
		"five_hour_reset_at",
		"5h_reset_at",
	); ok {
		return ts
	}
	return time.Time{}
}

func codexWeeklyRemainingRatio(auth *Auth) (float64, bool) {
	if auth == nil || len(auth.Metadata) == 0 {
		return 0, false
	}
	return floatFromMetadata(auth.Metadata,
		MetadataCodexWeeklyQuotaRemainingRatioKey,
		"codex_weekly_remaining_ratio",
		"weekly_remaining_ratio",
		"week_remaining_ratio",
		"7d_remaining_ratio",
	)
}

func codexWeeklyResetAt(auth *Auth) time.Time {
	if auth == nil || len(auth.Metadata) == 0 {
		return time.Time{}
	}
	if ts, ok := lookupMetadataTime(auth.Metadata,
		MetadataCodexWeeklyQuotaResetAtKey,
		"codex_weekly_reset_at",
		"weekly_reset_at",
		"week_reset_at",
		"7d_reset_at",
	); ok {
		return ts
	}
	return time.Time{}
}

func floatFromMetadata(meta map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if parsed, okParse := parseFloatAny(val); okParse {
				return parsed, true
			}
		}
	}
	return 0, false
}

func parseFloatAny(val any) (float64, bool) {
	switch typed := val.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	m.mu.RUnlock()

	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.RLock()
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.RUnlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	return isBuiltInSelector(m.selector)
}

func authMayNeedRouteAwareSelection(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.TrimSpace(auth.Prefix) != "" {
		return true
	}
	kind, _ := auth.AccountInfo()
	return strings.EqualFold(strings.TrimSpace(kind), "oauth")
}

func (m *Manager) invalidateRouteAwareProviderCache() {
	if m == nil {
		return
	}
	m.routeAwareMu.Lock()
	m.routeAwareProviders = make(map[string]bool)
	m.routeAwareMu.Unlock()
}

func (m *Manager) invalidateRouteAwareProviderCacheForAuthTransition(previous *Auth, next *Auth) {
	if m == nil {
		return
	}
	providers := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	appendProvider := func(auth *Auth) {
		if auth == nil || !authMayNeedRouteAwareSelection(auth) {
			return
		}
		providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
		if providerKey == "" {
			return
		}
		if _, exists := seen[providerKey]; exists {
			return
		}
		seen[providerKey] = struct{}{}
		providers = append(providers, providerKey)
	}
	appendProvider(previous)
	appendProvider(next)
	if len(providers) == 0 {
		return
	}
	m.routeAwareMu.Lock()
	for _, providerKey := range providers {
		delete(m.routeAwareProviders, providerKey)
	}
	m.routeAwareMu.Unlock()
}

func (m *Manager) providerMayNeedRouteAwareSelection(provider string) bool {
	if m == nil {
		return false
	}
	providerKey := strings.TrimSpace(strings.ToLower(provider))
	if providerKey == "" {
		return false
	}
	if m.providerHasOAuthAlias(providerKey) {
		return true
	}
	m.routeAwareMu.RLock()
	required, ok := m.routeAwareProviders[providerKey]
	m.routeAwareMu.RUnlock()
	if ok {
		return required
	}
	required = false
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled {
			continue
		}
		if strings.TrimSpace(strings.ToLower(auth.Provider)) != providerKey {
			continue
		}
		if authMayNeedRouteAwareSelection(auth) {
			required = true
			break
		}
	}
	m.mu.RUnlock()
	m.routeAwareMu.Lock()
	if m.routeAwareProviders == nil {
		m.routeAwareProviders = make(map[string]bool)
	}
	m.routeAwareProviders[providerKey] = required
	m.routeAwareMu.Unlock()
	return required
}

func (m *Manager) providerHasOAuthAlias(provider string) bool {
	if m == nil {
		return false
	}
	providerKey := strings.TrimSpace(strings.ToLower(provider))
	if providerKey == "" {
		return false
	}
	value := m.oauthModelAlias.Load()
	table, ok := value.(*oauthModelAliasTable)
	if !ok || table == nil || len(table.reverse) == 0 {
		return false
	}
	_, ok = table.reverse[providerKey]
	return ok
}

func (m *Manager) providersMayNeedRouteAwareSelection(providers []string) bool {
	for _, provider := range providers {
		if m.providerMayNeedRouteAwareSelection(provider) {
			return true
		}
	}
	return false
}

func shouldRetrySchedulerPick(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
}

func (m *Manager) routeAwareSelectionRequired(auth *Auth, routeModel string) bool {
	if auth == nil || strings.TrimSpace(routeModel) == "" {
		return false
	}
	return m.selectionModelKeyForAuth(auth, routeModel) != canonicalModelKey(routeModel)
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	allowedAuthIDs := selectionScopeAuthIDsFromMetadata(opts.Metadata)

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate.Provider != provider || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if !authAllowedBySelectionScope(allowedAuthIDs, candidate.ID) {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		if freePlanModelBlocked(candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForRouteModel(candidates, provider, model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		return nil, nil, errAvailable
	}
	selected := (*Auth)(nil)
	if preferBestAuthFromMetadata(opts.Metadata) {
		if len(available) > 0 {
			selected = available[0]
		}
	} else {
		var errPick error
		selected, errPick = m.selector.Pick(ctx, provider, selectionArgForSelector(m.selector, model), opts, available)
		if errPick != nil {
			m.mu.RUnlock()
			return nil, nil, errPick
		}
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if !m.useSchedulerFastPath() {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	if strings.TrimSpace(model) != "" && m.providerMayNeedRouteAwareSelection(provider) {
		allowedAuthIDs := selectionScopeAuthIDsFromMetadata(opts.Metadata)
		m.mu.RLock()
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Provider != provider || candidate.Disabled {
				continue
			}
			if !authAllowedBySelectionScope(allowedAuthIDs, candidate.ID) {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextLegacy(ctx, provider, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	selected, errPick := m.scheduler.pickSingle(ctx, provider, model, opts, tried)
	if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
		m.syncScheduler()
		selected, errPick = m.scheduler.pickSingle(ctx, provider, model, opts, tried)
	}
	if errPick != nil {
		return nil, nil, errPick
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	allowedAuthIDs := selectionScopeAuthIDsFromMetadata(opts.Metadata)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if !authAllowedBySelectionScope(allowedAuthIDs, candidate.ID) {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		if freePlanModelBlocked(candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, errAvailable := m.availableAuthsForRouteModel(candidates, "mixed", model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		return nil, nil, "", errAvailable
	}
	selected := (*Auth)(nil)
	if preferBestAuthFromMetadata(opts.Metadata) {
		if len(available) > 0 {
			selected = available[0]
		}
	} else {
		var errPick error
		selected, errPick = m.selector.Pick(ctx, "mixed", selectionArgForSelector(m.selector, model), opts, available)
		if errPick != nil {
			m.mu.RUnlock()
			return nil, nil, "", errPick
		}
	}
	if selected == nil {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor, okExecutor := m.executors[providerKey]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if !m.useSchedulerFastPath() {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}

	eligibleProviders := make([]string, 0, len(providers))
	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		if _, seen := seenProviders[providerKey]; seen {
			continue
		}
		if _, okExecutor := m.Executor(providerKey); !okExecutor {
			continue
		}
		seenProviders[providerKey] = struct{}{}
		eligibleProviders = append(eligibleProviders, providerKey)
	}
	if len(eligibleProviders) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if strings.TrimSpace(model) != "" && m.providersMayNeedRouteAwareSelection(eligibleProviders) {
		providerSet := make(map[string]struct{}, len(eligibleProviders))
		allowedAuthIDs := selectionScopeAuthIDsFromMetadata(opts.Metadata)
		for _, providerKey := range eligibleProviders {
			providerSet[providerKey] = struct{}{}
		}
		m.mu.RLock()
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Disabled {
				continue
			}
			if _, ok := providerSet[strings.TrimSpace(strings.ToLower(candidate.Provider))]; !ok {
				continue
			}
			if !authAllowedBySelectionScope(allowedAuthIDs, candidate.ID) {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}

	selected, providerKey, errPick := m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
	if errPick != nil && model != "" && shouldRetrySchedulerPick(errPick) {
		m.syncScheduler()
		selected, providerKey, errPick = m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
	}
	if errPick != nil {
		return nil, nil, "", errPick
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	executor, okExecutor := m.Executor(providerKey)
	if !okExecutor {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.refreshCancel = cancel
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		m.checkRefreshes(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkRefreshes(ctx)
			}
		}
	}()
}

// StopAutoRefresh cancels the background refresh loop, if running.
func (m *Manager) StopAutoRefresh() {
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
}

// StartQuotaFlusher starts the background quota persistence loop.
func (m *Manager) StartQuotaFlusher(ctx context.Context, persister QuotaPersister) {
	if m == nil || persister == nil {
		return
	}
	f := newQuotaFlusher(m, persister)
	m.quotaFlusher = f
	go f.Run(ctx)
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	// log.Debugf("checking refreshes")
	now := time.Now()
	snapshot := m.snapshotAuths()
	for _, a := range snapshot {
		typ, _ := a.AccountInfo()
		if typ != "api_key" {
			if !m.shouldRefresh(a, now) {
				continue
			}
			log.Debugf("checking refresh for %s, %s, %s", a.Provider, a.ID, typ)

			if exec := m.executorFor(a.Provider); exec == nil {
				continue
			}
			if !m.markRefreshPending(a.ID, now) {
				continue
			}
			go m.refreshAuthWithLimit(ctx, a.ID)
		}
	}
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, id string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.refreshSemaphore == nil {
		m.refreshAuth(ctx, id)
		return
	}
	select {
	case m.refreshSemaphore <- struct{}{}:
		defer func() { <-m.refreshSemaphore }()
	case <-ctx.Done():
		return
	}
	m.refreshAuth(ctx, id)
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	if hasRevokedAuthTombstoneMemory(a, now) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if isCodexAuth(a) {
		if m.shouldRefreshCodexQuotaProbe(a, now) {
			return true
		}
	}
	if a.Disabled {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func (m *Manager) shouldRefreshCodexQuotaProbe(a *Auth, now time.Time) bool {
	if a == nil || !isCodexAuth(a) {
		return false
	}
	lastRefresh := effectiveLastRefresh(a)
	interval := codexHotRefreshInterval
	if codexAuthNeedsColdRefresh(a, now) {
		interval = codexColdRefreshInterval
	} else if m.authDispatchCount(a.ID, now) >= codexFrequentActivityThreshold {
		interval = codexFrequentRefreshInterval
	}
	if lastRefresh.IsZero() {
		return true
	}
	return now.Sub(lastRefresh) >= interval
}

func codexAuthNeedsColdRefresh(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	if a.Disabled || a.Status == StatusDisabled {
		return true
	}
	if a.Quota.Exceeded {
		return true
	}
	if a.Unavailable && (!a.NextRetryAfter.IsZero() || a.Status == StatusError) {
		return true
	}
	return false
}

func effectiveLastRefresh(a *Auth) time.Time {
	if a == nil {
		return time.Time{}
	}
	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}
	return lastRefresh
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		return false
	}
	if hasRevokedAuthTombstoneMemory(auth, now) {
		return false
	}
	if auth.Disabled && !(isCodexAuth(auth) && codexAuthNeedsColdRefresh(auth, now)) {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	return true
}

// AsyncBindingProbeTTL bounds how often the dispatch-time kick can fire
// for the same auth. 60s means: up to one async re-probe per auth per
// minute under sustained traffic, zero when idle. Short enough to catch
// OpenAI's per-edge plan_type flips (observed 2-3 min intervals on
// freshly-upgraded accounts), long enough not to overlap with the 5min
// forced_refresh sweep that covers the whole pool.
const AsyncBindingProbeTTL = 60 * time.Second

// KickAsyncBindingProbe is called from the dispatch hot path right after
// the selector commits an auth. It asynchronously kicks a full refresh +
// multi-path probe when the bound codex auth's last refresh is older than
// AsyncBindingProbeTTL. Dispatch is NOT blocked; the current request
// still goes out through the existing (possibly stale) binding, but the
// probe result arrives in time to protect subsequent requests from
// burning free quota on the same stale binding.
//
// Gates (all must hold to fire):
//   - auth is codex and probed=paid (free/unprobed auths are handled by
//     the 5min forced_refresh scope)
//   - last_refresh >= TTL ago (both in-memory LastRefreshedAt AND the
//     file-hydrated Metadata["last_refresh"] fallback are consulted)
//   - markRefreshPending succeeds (not disabled, no in-flight refresh)
//
// Concurrency: markRefreshPending acquires m.mu.Lock exclusively, so two
// concurrent dispatches of the same auth race to set NextRefreshAfter;
// only the first wins. The other becomes a no-op. Refresh_token rotation
// thus never runs twice in parallel for the same auth.
func (m *Manager) KickAsyncBindingProbe(authID string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	m.mu.RUnlock()
	if !ok || auth == nil {
		return
	}
	if hasRevokedAuthTombstoneMemory(auth, time.Now()) {
		return
	}
	if !isCodexAuth(auth) {
		return
	}
	if !isPaidPlan(probedPlanType(auth)) {
		// Unprobed or free accounts: the 5min forced_refresh scope
		// already covers them. No point firing an extra probe here.
		return
	}

	now := time.Now()
	last := auth.LastRefreshedAt
	if last.IsZero() {
		if ts, okTs := authLastRefreshTimestamp(auth); okTs {
			last = ts
		}
	}
	if !last.IsZero() && now.Sub(last) < AsyncBindingProbeTTL {
		return
	}

	if !m.markRefreshPending(authID, now) {
		return
	}

	// refreshAuthWithLimit acquires m.refreshSemaphore, so the full pool
	// of async probes shares the existing 16-slot pipeline with forced
	// refreshes and the 15min auto-refresh loop. If the pool is
	// saturated, this goroutine parks until a slot opens while remaining
	// non-blocking for the caller thanks to the `go` prefix.
	go m.refreshAuthWithLimit(context.Background(), authID)
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	if auth != nil {
		exec = m.executors[auth.Provider]
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return
	}
	cloned := auth.Clone()
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		resultErr := resultErrorFromError(err)
		if handled, deleteAuth, reason := codexPendingActivationUnauthorizedDecision(auth, resultErr, now); handled {
			if deleteAuth {
				var authSnapshot *Auth
				m.mu.Lock()
				if current := m.auths[id]; current != nil {
					applyTerminalAuthDisabledState(current, resultErr, reason, now)
					current.NextRefreshAfter = time.Time{}
					authSnapshot = current.Clone()
					delete(m.auths, id)
				}
				m.mu.Unlock()
				if authSnapshot != nil {
					m.deleteRevokedAuth(authSnapshot, reason, "refresh")
				}
				return
			}
			var authSnapshot *Auth
			var persistErr error
			m.mu.Lock()
			if current := m.auths[id]; current != nil {
				applyCodexPendingActivationCooldown(current, resultErr, now)
				current.NextRefreshAfter = now.Add(DefaultPendingActivationProbeInterval)
				m.auths[id] = current
				authSnapshot = current.Clone()
				persistErr = m.persist(ctx, current)
			}
			m.mu.Unlock()
			if authSnapshot != nil && m.scheduler != nil {
				m.scheduler.upsertAuth(authSnapshot)
			}
			if persistErr != nil {
				log.Warnf("failed to persist pending activation cooldown for auth %s%s: %v", authSnapshot.ID, authPathSuffix(authSnapshot), persistErr)
			}
			return
		}
		if deleteAuth, reason := shouldDeleteRevokedAuth(auth, resultErr, false); deleteAuth {
			var authSnapshot *Auth
			m.mu.Lock()
			if current := m.auths[id]; current != nil {
				applyTerminalAuthDisabledState(current, resultErr, reason, now)
				current.NextRefreshAfter = time.Time{}
				authSnapshot = current.Clone()
				delete(m.auths, id)
			}
			m.mu.Unlock()
			if authSnapshot != nil {
				m.deleteRevokedAuth(authSnapshot, reason, "refresh")
			}
			return
		}
		if disableAuth, reason := shouldDisableRevokedAuth(auth, resultErr, false); disableAuth {
			var authSnapshot *Auth
			var persistErr error
			m.mu.Lock()
			if current := m.auths[id]; current != nil {
				applyTerminalAuthDisabledState(current, resultErr, reason, now)
				current.NextRefreshAfter = time.Time{}
				m.auths[id] = current
				authSnapshot = current.Clone()
				persistErr = m.persist(ctx, current)
			}
			m.mu.Unlock()
			if authSnapshot != nil && m.scheduler != nil {
				m.scheduler.upsertAuth(authSnapshot)
			}
			if persistErr != nil {
				log.Warnf("disabled revoked auth %s%s after terminal auth failure during refresh, but failed to persist disabled state: %v", authSnapshot.ID, authPathSuffix(authSnapshot), persistErr)
			}
			return
		}
		// For 429 errors during refresh, feed them through MarkResult so that
		// cooldown tiering (usage_limit vs rate_limit) is applied consistently.
		statusCode := statusCodeFromResult(resultErr)
		if statusCode == http.StatusTooManyRequests {
			retryAfter := retryAfterFromError(err)
			m.MarkResult(ctx, Result{
				AuthID:     id,
				Provider:   auth.Provider,
				Success:    false,
				RetryAfter: retryAfter,
				Error:      resultErr,
			})
			m.mu.Lock()
			if current := m.auths[id]; current != nil {
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
				m.auths[id] = current
			}
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			current.Unavailable = true
			current.Status = StatusError
			current.NextRetryAfter = now.Add(refreshFailureBackoff)
			current.LastError = resultErr
			if strings.TrimSpace(current.StatusMessage) == "" || strings.Contains(strings.ToLower(strings.TrimSpace(current.StatusMessage)), "pending") {
				current.StatusMessage = resultErr.Message
			}
			m.auths[id] = current
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		return
	}
	if updated == nil {
		updated = cloned
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	applyCodexFiveHourQuotaRest(updated, now)
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	_, _ = m.Update(ctx, updated)
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[executorKeyFromAuth(a)]
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}
