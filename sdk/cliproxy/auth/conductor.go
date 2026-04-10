package auth

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
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
	revokedDeleteTimeout  = 30 * time.Second
	revokedDeleteQueueCap = 1024

	quotaProbeBatchInterval       = 15 * time.Second
	quotaProbeBatchSize           = 20
	quotaProbeCooldown            = 15 * time.Minute
	quotaProbeRetryLimit          = 2
	quotaProbeTimeout             = 45 * time.Second
	codexUsageRefreshDelay        = 5 * time.Minute
	codexLowRemainingThresholdPct = 10.0
	codexRateLimitCooldown        = 60 * time.Second
	quotaProbeMaxConcurrency      = 20
	quotaPriorityPenaltyStep      = 2
	quotaPriorityPenaltyMax       = 8
	quotaPriorityPenaltyRecover   = 1
	slowRequestThresholdDefaultMs = 20000
	slowRequestTriggerDefault     = 2
	slowRequestPenaltyStepDefault = 2
	slowRequestPenaltyMaxDefault  = 8
	slowRequestPenaltyRecover     = 1
	streamBootstrapPayloadTimeout = 30 * time.Second
	defaultExecuteAttemptTimeout  = 90 * time.Second
	defaultCountAttemptTimeout    = 30 * time.Second
	defaultStreamConnectTimeout   = 30 * time.Second
	sameAuthRetryMinWait          = 500 * time.Millisecond
	sameAuthRetryMaxWait          = 1500 * time.Millisecond
	streamDisconnectRetryMinWait  = 300 * time.Millisecond
	streamDisconnectRetryMaxWait  = 1200 * time.Millisecond
	streamDisconnectRetryLimit    = 2
	defaultTransientSameAuthRetry = 2
	executionSessionAffinityTTL   = time.Hour
	freeCodexAuthTTL              = time.Hour
	freeAuthExpiryBatchSize       = 128
	freeAuthExpiryDrainDelay      = 100 * time.Millisecond
	slowRequestWindowDefault      = 5 * time.Minute
	slowRequestCooldownDefault    = 5 * time.Minute
	gpt54ModelPrefix              = "gpt-5.4"
	gpt53CodexModelPrefix         = "gpt-5.3-codex"
	fallbackRetryableErrorCode    = "fallback_retryable"
)

const (
	metadataQuotaProbeLastKey        = "cliproxy_quota_probe_last"
	metadataQuotaProbeAfterKey       = "cliproxy_quota_probe_after"
	metadataAutoDisabledReasonKey    = "cliproxy_auto_disabled_reason"
	autoDisabledReasonQuotaExhausted = "quota_exhausted"
	autoDisabledReasonQuotaLowBalance = "quota_low_balance"
	metadataFreeFirstUsedAtKey       = "cliproxy_free_first_used_at"
	metadataLastUsedAtKey            = "cliproxy_last_used_at"
	metadataCodexUsageLastKey        = "cliproxy_codex_usage_last"
	metadataCodexUsageAfterKey       = "cliproxy_codex_usage_after"
	metadataCodexUsagePayloadKey     = "cliproxy_codex_usage_payload"
	metadataCodexUsageRemainingKey   = "cliproxy_codex_usage_remaining_percent"
)

var quotaCooldownDisabled atomic.Bool

var errQuotaProbeSkippedNoSupportedModel = errors.New("quota refresh probe skipped: no supported model for auth")

var sameAuthRetryDelayFunc = nextSameAuthRetryDelay
var streamDisconnectRetryDelayFunc = nextStreamDisconnectRetryDelay
var executionSessionAffinityNowFunc = time.Now
var freeAuthExpiryAfterFunc = time.AfterFunc
var modelNotFoundPattern = regexp.MustCompile(`(?i)model\s+not\s+found\s+([A-Za-z0-9._:-]+)`)

type streamBootstrapTimeoutContextKey struct{}
type executeAttemptTimeoutContextKey struct{}
type disableExecuteAttemptTimeoutContextKey struct{}

func streamBootstrapTimeout(ctx context.Context) time.Duration {
	if ctx != nil {
		if override, ok := ctx.Value(streamBootstrapTimeoutContextKey{}).(time.Duration); ok && override > 0 {
			return override
		}
	}
	if cfg := runtimeConfigFromContext(ctx); cfg != nil && cfg.Streaming.BootstrapTimeoutSeconds > 0 {
		return time.Duration(cfg.Streaming.BootstrapTimeoutSeconds) * time.Second
	}
	return streamBootstrapPayloadTimeout
}

func attemptTimeout(ctx context.Context, fallback time.Duration, selector func(*internalconfig.Config) int) time.Duration {
	if ctx != nil {
		if disabled, ok := ctx.Value(disableExecuteAttemptTimeoutContextKey{}).(bool); ok && disabled && fallback == defaultExecuteAttemptTimeout {
			return 0
		}
		if override, ok := ctx.Value(streamBootstrapTimeoutContextKey{}).(time.Duration); ok && override > 0 && fallback == streamBootstrapPayloadTimeout {
			return override
		}
		if override, ok := ctx.Value(executeAttemptTimeoutContextKey{}).(time.Duration); ok && override > 0 && fallback == defaultExecuteAttemptTimeout {
			return override
		}
	}
	if selector != nil {
		if cfg, ok := ctx.Value(managerRuntimeConfigContextKey{}).(*internalconfig.Config); ok && cfg != nil {
			if seconds := selector(cfg); seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return fallback
}

type managerRuntimeConfigContextKey struct{}

func runtimeConfigFromContext(ctx context.Context) *internalconfig.Config {
	if ctx == nil {
		return nil
	}
	cfg, _ := ctx.Value(managerRuntimeConfigContextKey{}).(*internalconfig.Config)
	return cfg
}

func attachManagerRuntimeConfig(ctx context.Context, cfg *internalconfig.Config) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	return context.WithValue(ctx, managerRuntimeConfigContextKey{}, cfg)
}

// WithExecuteAttemptTimeout overrides the manager-level non-streaming execution timeout for a single request.
func WithExecuteAttemptTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return context.WithValue(ctx, disableExecuteAttemptTimeoutContextKey{}, true)
	}
	return context.WithValue(ctx, executeAttemptTimeoutContextKey{}, timeout)
}

func executeAttemptTimeout(ctx context.Context) time.Duration {
	return attemptTimeout(ctx, defaultExecuteAttemptTimeout, func(cfg *internalconfig.Config) int { return cfg.ExecuteTimeoutSeconds })
}

func countAttemptTimeout(ctx context.Context) time.Duration {
	return attemptTimeout(ctx, defaultCountAttemptTimeout, func(cfg *internalconfig.Config) int { return cfg.CountTimeoutSeconds })
}

func streamConnectAttemptTimeout(ctx context.Context) time.Duration {
	return attemptTimeout(ctx, defaultStreamConnectTimeout, func(cfg *internalconfig.Config) int { return cfg.StreamConnectTimeoutSeconds })
}

func withAttemptTimeout(ctx context.Context, timeout time.Duration, errFactory func(time.Duration) *Error) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return ctx, func() {}
	}
	if errFactory == nil {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithTimeoutCause(ctx, timeout, errFactory(timeout))
}

type streamConnectAttemptResult struct {
	result *cliproxyexecutor.StreamResult
	err    error
}

func executeStreamConnectAttempt(ctx context.Context, timeout time.Duration, fn func(context.Context) (*cliproxyexecutor.StreamResult, error)) (*cliproxyexecutor.StreamResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return fn(ctx)
	}

	attemptCtx, cancel := context.WithCancel(ctx)
	done := make(chan streamConnectAttemptResult, 1)
	go func() {
		streamResult, err := fn(attemptCtx)
		done <- streamConnectAttemptResult{result: streamResult, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case outcome := <-done:
		if outcome.err != nil {
			cancel()
		}
		return outcome.result, outcome.err
	case <-timer.C:
		cancel()
		return nil, newStreamConnectTimeoutError(timeout)
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

func newStreamBootstrapTimeoutError(timeout time.Duration) *Error {
	if timeout <= 0 {
		timeout = streamBootstrapPayloadTimeout
	}
	return &Error{
		Code:       "stream_bootstrap_timeout",
		Message:    "upstream stream timed out waiting for first payload after " + timeout.String(),
		Retryable:  true,
		HTTPStatus: http.StatusRequestTimeout,
	}
}

func newExecuteAttemptTimeoutError(timeout time.Duration) *Error {
	if timeout <= 0 {
		timeout = defaultExecuteAttemptTimeout
	}
	return &Error{Code: "execute_timeout", Message: "upstream request timed out after " + timeout.String(), Retryable: true, HTTPStatus: http.StatusRequestTimeout}
}

func newCountAttemptTimeoutError(timeout time.Duration) *Error {
	if timeout <= 0 {
		timeout = defaultCountAttemptTimeout
	}
	return &Error{Code: "count_timeout", Message: "upstream token count timed out after " + timeout.String(), Retryable: true, HTTPStatus: http.StatusRequestTimeout}
}

func newStreamConnectTimeoutError(timeout time.Duration) *Error {
	if timeout <= 0 {
		timeout = defaultStreamConnectTimeout
	}
	return &Error{Code: "stream_connect_timeout", Message: "upstream stream setup timed out after " + timeout.String(), Retryable: true, HTTPStatus: http.StatusRequestTimeout}
}

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
	// Latency measures upstream time. For streams it is the time to first payload.
	Latency time.Duration
	// Error describes the failure when Success is false.
	Error *Error
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
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

	// authInflight tracks how many live requests currently hold each auth slot.
	authInflightMu sync.Mutex
	authInflight   map[string]int

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel    context.CancelFunc
	refreshSemaphore chan struct{}

	// Auto quota refresh state
	quotaRefreshCancel    context.CancelFunc
	quotaRefreshSemaphore chan struct{}
	quotaRefreshPending   map[string]time.Time
	revokedDeleteQueue    chan revokedDeleteTask
	revokedDeletePending  map[string]struct{}
	freeAuthExpiryHeap    freeAuthExpiryHeap
	freeAuthExpiryIndex   map[string]*freeAuthExpiryItem
	freeAuthExpiryWake    chan struct{}
	executionSessionAuth  map[string]executionSessionBinding
}

type revokedDeleteTask struct {
	auth    *Auth
	warning string
	source  string
}

type executionSessionBinding struct {
	AuthID    string
	ExpiresAt time.Time
}

type freeAuthExpiryItem struct {
	authID   string
	expireAt time.Time
	index    int
}

type freeAuthExpiryHeap []*freeAuthExpiryItem

func (h freeAuthExpiryHeap) Len() int { return len(h) }

func (h freeAuthExpiryHeap) Less(i, j int) bool {
	if h[i] == nil {
		return false
	}
	if h[j] == nil {
		return true
	}
	return h[i].expireAt.Before(h[j].expireAt)
}

func (h freeAuthExpiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	if h[i] != nil {
		h[i].index = i
	}
	if h[j] != nil {
		h[j].index = j
	}
}

func (h *freeAuthExpiryHeap) Push(x any) {
	item, _ := x.(*freeAuthExpiryItem)
	if item == nil {
		return
	}
	item.index = len(*h)
	*h = append(*h, item)
}

func (h *freeAuthExpiryHeap) Pop() any {
	old := *h
	n := len(old)
	if n == 0 {
		return nil
	}
	item := old[n-1]
	old[n-1] = nil
	if item != nil {
		item.index = -1
	}
	*h = old[:n-1]
	return item
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
		store:                 store,
		executors:             make(map[string]ProviderExecutor),
		selector:              selector,
		hook:                  hook,
		auths:                 make(map[string]*Auth),
		authInflight:          make(map[string]int),
		providerOffsets:       make(map[string]int),
		modelPoolOffsets:      make(map[string]int),
		refreshSemaphore:      make(chan struct{}, refreshMaxConcurrency),
		quotaRefreshSemaphore: make(chan struct{}, quotaProbeMaxConcurrency),
		quotaRefreshPending:   make(map[string]time.Time),
		revokedDeleteQueue:    make(chan revokedDeleteTask, revokedDeleteQueueCap),
		revokedDeletePending:  make(map[string]struct{}),
		freeAuthExpiryIndex:   make(map[string]*freeAuthExpiryItem),
		freeAuthExpiryWake:    make(chan struct{}, 1),
		executionSessionAuth:  make(map[string]executionSessionBinding),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.scheduler = newAuthScheduler(selector)
	go manager.runRevokedDeleteWorker()
	go manager.runFreeAuthExpiryWorker()
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
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
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
	if m.scheduler != nil {
		m.scheduler.setNewAuthFirst(cfg.Routing.PreferNewAuthFirst())
		m.syncScheduler()
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
}

func (m *Manager) CurrentConfig() *internalconfig.Config {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg
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

func cloneExecutionOptions(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if len(opts.Metadata) == 0 {
		return opts
	}
	cloned := opts
	meta := make(map[string]any, len(opts.Metadata))
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	cloned.Metadata = meta
	return cloned
}

func setRequestedModelMetadata(opts cliproxyexecutor.Options, model string) cliproxyexecutor.Options {
	model = strings.TrimSpace(model)
	if model == "" {
		return opts
	}
	cloned := cloneExecutionOptions(opts)
	if len(cloned.Metadata) == 0 {
		cloned.Metadata = map[string]any{}
	}
	cloned.Metadata[cliproxyexecutor.RequestedModelMetadataKey] = model
	return cloned
}

func resolveGPT54FallbackModelName(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	if normalized == "" || !strings.HasPrefix(normalized, gpt54ModelPrefix) {
		return ""
	}
	return gpt53CodexModelPrefix + strings.TrimPrefix(normalized, gpt54ModelPrefix)
}

func shouldFallbackGPT54ByError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	return strings.Contains(message, "selected model is at capacity")
}

func shouldFallbackToGPT53Codex(model string, provider string, err error) bool {
	if strings.ToLower(strings.TrimSpace(provider)) != "codex" {
		return false
	}
	if resolveGPT54FallbackModelName(model) == "" {
		return false
	}
	return shouldFallbackGPT54ByError(err)
}

func shouldAbortRetryLoop(ctx context.Context, err error) error {
	if ctx != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return nil
	}
	if strings.Contains(message, "context canceled") || strings.Contains(message, "context deadline exceeded") {
		return err
	}
	return nil
}

func shouldTreatAsNoAvailableModel(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*modelCooldownError](err); ok {
		return true
	}
	if authErr, ok := errors.AsType[*Error](err); ok && authErr != nil {
		code := strings.ToLower(strings.TrimSpace(authErr.Code))
		if code == "auth_not_found" || code == "provider_not_found" {
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	return strings.Contains(message, "no upstream model available") ||
		strings.Contains(message, "no auth available")
}

func withGPT53CodexFallbackSkipRegistry(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	cloned := cloneExecutionOptions(opts)
	if len(cloned.Metadata) == 0 {
		cloned.Metadata = map[string]any{}
	}
	cloned.Metadata[cliproxyexecutor.SkipModelRegistryCheckMetadataKey] = true
	return cloned
}

func downgradeGPT53CodexVariantToBase(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	if strings.HasPrefix(normalized, gpt53CodexModelPrefix+"-") {
		return gpt53CodexModelPrefix
	}
	return normalized
}

func shouldFallbackGPT53VariantToBase(currentModel string, err error) bool {
	current := strings.ToLower(strings.TrimSpace(canonicalModelKey(currentModel)))
	if !strings.HasPrefix(current, gpt53CodexModelPrefix+"-") {
		return false
	}
	return shouldTreatAsNoAvailableModel(err)
}

func registryIndicatesModelUnavailable(authID string, model string) bool {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(canonicalModelKey(model))
	if authID == "" || model == "" {
		return false
	}
	reg := registry.GetGlobalRegistry()
	if reg == nil {
		return false
	}
	knownModels := reg.GetModelsForClient(authID)
	if len(knownModels) == 0 {
		return false
	}
	return !reg.ClientSupportsModel(authID, model)
}

func sanitizeResultForFallbackRetry(result Result) Result {
	sanitized := result
	sanitized.RetryAfter = nil
	if sanitized.Error == nil {
		sanitized.Error = &Error{Code: fallbackRetryableErrorCode, Retryable: true}
		return sanitized
	}
	if sanitized.Error.Code == "" {
		sanitized.Error.Code = fallbackRetryableErrorCode
	} else {
		sanitized.Error.Code = fallbackRetryableErrorCode + "_" + sanitized.Error.Code
	}
	sanitized.Error.HTTPStatus = 0
	sanitized.Error.Retryable = true
	return sanitized
}

func adjustResultForPotentialGPT54Fallback(provider, model string, execErr error, result Result) Result {
	if !shouldFallbackToGPT53Codex(model, provider, execErr) {
		return result
	}
	return sanitizeResultForFallbackRetry(result)
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
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
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

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	timeout := streamBootstrapTimeout(ctx)
	var timeoutCh <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutCh = timer.C
		defer timer.Stop()
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-timeoutCh:
				return nil, false, newStreamBootstrapTimeoutError(timeout)
			case chunk, ok = <-ch:
			}
		} else {
			select {
			case <-timeoutCh:
				return nil, false, newStreamBootstrapTimeoutError(timeout)
			case chunk, ok = <-ch:
			}
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, routeModel string, latency time.Duration, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var failed bool
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := &Error{Message: chunk.Err.Error()}
				if se, ok := errors.AsType[cliproxyexecutor.StatusError](chunk.Err); ok && se != nil {
					rerr.HTTPStatus = se.StatusCode()
				}
				result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: rerr, Latency: latency}
				if ra := retryAfterFromError(chunk.Err); ra != nil {
					result.RetryAfter = ra
				}
				m.recordResult(ctx, result)
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
			m.recordResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: true, Latency: latency})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel string, execModels []string, pooled bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	var lastErr error
	revokedRefreshAttempted := false
	for idx, execModel := range execModels {
		resultModel := executionResultModel(routeModel, execModel, pooled)
		_ = m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		attemptExecStream := func() (*cliproxyexecutor.StreamResult, error, time.Time) {
			m.noteFreeCodexUse(ctx, auth.ID)
			attemptStartedAt := time.Now()
			streamResult, errStream := executeStreamConnectAttempt(ctx, streamConnectAttemptTimeout(ctx), func(attemptCtx context.Context) (*cliproxyexecutor.StreamResult, error) {
				return executor.ExecuteStream(attemptCtx, auth, execReq, opts)
			})
			return streamResult, errStream, attemptStartedAt
		}

		streamResult, errStream, attemptStartedAt := attemptExecStream()
		if errStream != nil && !revokedRefreshAttempted {
			rerr := &Error{Message: errStream.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errStream); ok && se != nil {
				rerr.HTTPStatus = se.StatusCode()
			}
			if deleteAuth, _ := shouldDeleteRevokedAuth(auth, rerr); deleteAuth && hasRefreshTokenMetadata(auth) {
				revokedRefreshAttempted = true
				refreshed, errRefresh := executor.Refresh(ctx, auth.Clone())
				if errRefresh == nil && refreshed != nil {
					if refreshed.Runtime == nil {
						refreshed.Runtime = auth.Runtime
					}
					auth = refreshed
					_, _ = m.Update(ctx, refreshed.Clone())
					streamResult, errStream, attemptStartedAt = attemptExecStream()
				} else if errRefresh != nil {
					log.WithError(errRefresh).Debugf("auth refresh after revoked token failed: %s", auth.ID)
				}
			}
		}
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			rerr := &Error{Message: errStream.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errStream); ok && se != nil {
				rerr.HTTPStatus = se.StatusCode()
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr, Latency: time.Since(attemptStartedAt)}
			result.RetryAfter = retryAfterFromError(errStream)
			m.recordResult(ctx, result)
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		firstPayloadLatency := time.Since(attemptStartedAt)
		if bootstrapErr != nil && len(buffered) == 0 && isStreamDisconnectedBeforeCompletionError(bootstrapErr) {
			discardStreamChunks(streamResult.Chunks)
			streamResult, errStream, attemptStartedAt = attemptExecStream()
			if errStream == nil {
				buffered, closed, bootstrapErr = readStreamBootstrap(ctx, streamResult.Chunks)
				firstPayloadLatency = time.Since(attemptStartedAt)
			} else {
				bootstrapErr = errStream
				buffered = nil
				closed = false
				firstPayloadLatency = time.Since(attemptStartedAt)
			}
		}
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCtx
			}
			resultErr := &Error{Message: bootstrapErr.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](bootstrapErr); ok && se != nil {
				resultErr.HTTPStatus = se.StatusCode()
			}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: resultErr, Latency: firstPayloadLatency}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			if isRequestInvalidError(bootstrapErr) {
				m.recordResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if isModelSupportError(bootstrapErr) {
				m.recordResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			if idx < len(execModels)-1 {
				m.recordResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			if timeoutErr, ok := bootstrapErr.(*Error); ok && timeoutErr != nil && timeoutErr.Code == "stream_bootstrap_timeout" {
				m.recordResult(ctx, result)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			errCh := make(chan cliproxyexecutor.StreamChunk, 1)
			errCh <- cliproxyexecutor.StreamChunk{Err: bootstrapErr}
			close(errCh)
			return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, firstPayloadLatency, streamResult.Headers, nil, errCh), nil
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr, Latency: firstPayloadLatency}
			m.recordResult(ctx, result)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			errCh := make(chan cliproxyexecutor.StreamChunk, 1)
			errCh <- cliproxyexecutor.StreamChunk{Err: emptyErr}
			close(errCh)
			return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, firstPayloadLatency, streamResult.Headers, nil, errCh), nil
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, firstPayloadLatency, streamResult.Headers, buffered, remaining), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
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

func (m *Manager) removeAPIKeyModelAlias(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if len(table) == 0 {
		return
	}
	if _, exists := table[authID]; !exists {
		return
	}
	updated := make(apiKeyModelAliasTable, len(table)-1)
	for id, aliases := range table {
		if id == authID {
			continue
		}
		updated[id] = aliases
	}
	m.apiKeyModelAlias.Store(updated)
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
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
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

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
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
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = now
	}
	if auth.UpdatedAt.IsZero() {
		auth.UpdatedAt = auth.CreatedAt
	}
	auth.EnsureIndex()
	hydrateCodexPlanType(auth)
	hydrateCodexAccountID(auth)
	authClone := auth.Clone()
	m.mu.Lock()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	_ = m.persist(ctx, auth)
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	m.syncFreeAuthExpiryTimer(authClone)
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	now := time.Now().UTC()
	wasDisabled := false
	m.mu.Lock()
	if existing, ok := m.auths[auth.ID]; ok && existing != nil {
		wasDisabled = existing.Disabled
		if auth.CreatedAt.IsZero() {
			auth.CreatedAt = existing.CreatedAt
		}
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		if !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled {
			if len(auth.ModelStates) == 0 && len(existing.ModelStates) > 0 {
				auth.ModelStates = existing.ModelStates
			}
		}
	}
	if auth.CreatedAt.IsZero() {
		auth.CreatedAt = now
	}
	auth.UpdatedAt = now
	auth.EnsureIndex()
	hydrateCodexPlanType(auth)
	hydrateCodexAccountID(auth)
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	if authClone.Disabled && !wasDisabled {
		m.unbindExecutionSessionsForAuthLocked(auth.ID)
	}
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	_ = m.persist(ctx, auth)
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	m.syncFreeAuthExpiryTimer(authClone)
	return auth.Clone(), nil
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	items, err := store.List(ctx)
	if err != nil {
		return err
	}
	loaded := make([]*Auth, 0, len(items))
	m.mu.Lock()
	m.freeAuthExpiryHeap = nil
	m.freeAuthExpiryIndex = make(map[string]*freeAuthExpiryItem)
	m.executionSessionAuth = make(map[string]executionSessionBinding)
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		hydrateCodexPlanType(auth)
		hydrateCodexAccountID(auth)
		auth.EnsureIndex()
		cloned := auth.Clone()
		m.auths[auth.ID] = cloned
		loaded = append(loaded, cloned.Clone())
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	for _, auth := range loaded {
		m.syncFreeAuthExpiryTimer(auth)
	}
	m.signalFreeAuthExpiryWorker()
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = attachManagerRuntimeConfig(ctx, m.CurrentConfig())
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
		if abortErr := shouldAbortRetryLoop(ctx, errExec); abortErr != nil {
			return cliproxyexecutor.Response{}, abortErr
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
	ctx = attachManagerRuntimeConfig(ctx, m.CurrentConfig())
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
		if abortErr := shouldAbortRetryLoop(ctx, errExec); abortErr != nil {
			return cliproxyexecutor.Response{}, abortErr
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
	ctx = attachManagerRuntimeConfig(ctx, m.CurrentConfig())
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
		if abortErr := shouldAbortRetryLoop(ctx, errStream); abortErr != nil {
			return nil, abortErr
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
	tried := make(map[string]struct{})
	countedTried := make(map[string]struct{})
	var lastErr error
	fallbackActive := false
	fallbackRouteModel := ""
	fallbackVariantNoModel := false
	fallbackVariantAuthAttempts := 0
	fallbackVariantTried := make(map[string]struct{})
	fallbackVariantCountedTried := make(map[string]struct{})
	for {
		currentRouteModel := routeModel
		currentOpts := opts
		currentTried := tried
		currentCountedTried := countedTried
		if fallbackActive {
			currentRouteModel = fallbackRouteModel
			currentOpts = setRequestedModelMetadata(opts, fallbackRouteModel)
			currentTried = fallbackVariantTried
			currentCountedTried = fallbackVariantCountedTried
		}
		if maxRetryCredentials > 0 && len(currentCountedTried) >= maxRetryCredentials {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, release, errPick := m.pickNextMixedReserved(ctx, providers, currentRouteModel, currentOpts, currentTried)
		if errPick != nil {
			if lastErr != nil && isPoolExhaustedPickError(errPick) {
				if cooldownErr := m.triedModelCooldownError(currentTried, providers, currentRouteModel); cooldownErr != nil {
					return cliproxyexecutor.Response{}, cooldownErr
				}
				if fallbackActive && shouldFallbackGPT53VariantToBase(currentRouteModel, lastErr) {
					baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
					if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
						entry := logEntryWithRequestID(ctx)
						entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
						fallbackRouteModel = baseModel
						opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
						fallbackVariantNoModel = false
						fallbackVariantAuthAttempts = 0
						fallbackVariantTried = make(map[string]struct{})
						fallbackVariantCountedTried = make(map[string]struct{})
						continue
					}
				}
				return cliproxyexecutor.Response{}, lastErr
			}
			if fallbackActive && shouldFallbackGPT53VariantToBase(currentRouteModel, errPick) {
				baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
				if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
					entry := logEntryWithRequestID(ctx)
					entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
					fallbackRouteModel = baseModel
					opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
					fallbackVariantNoModel = false
					fallbackVariantAuthAttempts = 0
					fallbackVariantTried = make(map[string]struct{})
					fallbackVariantCountedTried = make(map[string]struct{})
					continue
				}
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		releaseReserved := release
		releaseSlot := func() {
			if releaseReserved != nil {
				releaseReserved()
				releaseReserved = nil
			}
		}

		currentTried[auth.ID] = struct{}{}
		preparedAuth, errPrepare := m.preparePickedAuth(auth, provider, currentRouteModel)
		if errPrepare != nil {
			releaseSlot()
			lastErr = errPrepare
			continue
		}
		auth = preparedAuth

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, currentRouteModel)
		if fallbackActive && strings.HasPrefix(strings.ToLower(strings.TrimSpace(canonicalModelKey(currentRouteModel))), gpt53CodexModelPrefix+"-") {
			if registryIndicatesModelUnavailable(auth.ID, currentRouteModel) {
				baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
				if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
					entry.Infof("fallback model %s not registered for auth, retrying with base model %s", currentRouteModel, baseModel)
					fallbackRouteModel = baseModel
					opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
					fallbackVariantNoModel = false
					fallbackVariantAuthAttempts = 0
					fallbackVariantTried = make(map[string]struct{})
					fallbackVariantCountedTried = make(map[string]struct{})
					releaseSlot()
					continue
				}
			}
		}
		publishSelectedAuthMetadata(currentOpts.Metadata, auth.ID)
		m.bindExecutionSessionAuth(executionSessionIDFromMetadata(currentOpts.Metadata), auth.ID)

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}

		sameAuthRetries := 0
		revokedRefreshAttempted := false
		for authAttempt := 0; ; authAttempt++ {
			if authAttempt > 0 {
				preparedAuth, errPrepare = m.preparePickedAuth(auth, provider, currentRouteModel)
				if errPrepare != nil {
					lastErr = errPrepare
					break
				}
				auth = preparedAuth
			}
			models, pooled := m.preparedExecutionModels(auth, currentRouteModel)
			if len(models) == 0 {
				releaseSlot()
				goto nextExecuteAttempt
			}
			var (
				authErr        error
				restrictionErr error
			)

			for idx, upstreamModel := range models {
				resultModel := executionResultModel(currentRouteModel, upstreamModel, pooled)
				execReq := req
				execReq.Model = upstreamModel
				m.noteFreeCodexUse(execCtx, auth.ID)
				attemptStartedAt := time.Now()
				attemptCtx, cancel := withAttemptTimeout(execCtx, executeAttemptTimeout(execCtx), newExecuteAttemptTimeoutError)
				resp, errExec := executor.Execute(attemptCtx, auth, execReq, currentOpts)
				cancel()
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil, Latency: time.Since(attemptStartedAt)}
				if errExec != nil {
					if errCtx := execCtx.Err(); errCtx != nil {
						releaseSlot()
						return cliproxyexecutor.Response{}, errCtx
					}
					result.Error = &Error{Message: errExec.Error()}
					if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
						result.Error.HTTPStatus = se.StatusCode()
					}
					if ra := retryAfterFromError(errExec); ra != nil {
						result.RetryAfter = ra
					}

					if deleteAuth, _ := shouldDeleteRevokedAuth(auth, result.Error); deleteAuth && !revokedRefreshAttempted && hasRefreshTokenMetadata(auth) {
						revokedRefreshAttempted = true
						refreshed, errRefresh := executor.Refresh(execCtx, auth.Clone())
						if errRefresh == nil && refreshed != nil {
							if refreshed.Runtime == nil {
								refreshed.Runtime = auth.Runtime
							}
							auth = refreshed
							_, _ = m.Update(execCtx, refreshed.Clone())
							m.noteFreeCodexUse(execCtx, auth.ID)

							retryAttemptCtx, retryCancel := withAttemptTimeout(execCtx, executeAttemptTimeout(execCtx), newExecuteAttemptTimeoutError)
							retryResp, retryErr := executor.Execute(retryAttemptCtx, auth, execReq, currentOpts)
							retryCancel()
							if retryErr == nil {
								m.recordResult(execCtx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: true, Latency: time.Since(attemptStartedAt)})
								releaseSlot()
								return retryResp, nil
							}

							errExec = retryErr
							result.Success = false
							result.Error = &Error{Message: retryErr.Error()}
							if se, ok := errors.AsType[cliproxyexecutor.StatusError](retryErr); ok && se != nil {
								result.Error.HTTPStatus = se.StatusCode()
							}
							if ra := retryAfterFromError(retryErr); ra != nil {
								result.RetryAfter = ra
							} else {
								result.RetryAfter = nil
							}
						} else if errRefresh != nil {
							log.WithError(errRefresh).Debugf("auth refresh after revoked token failed: %s", auth.ID)
						}
					}

					result = adjustResultForPotentialGPT54Fallback(provider, currentRouteModel, errExec, result)
					m.recordResult(execCtx, result)
					if isRequestInvalidError(errExec) {
						releaseSlot()
						return cliproxyexecutor.Response{}, errExec
					}

					authErr = errExec
					if abortErr := shouldAbortRetryLoop(execCtx, errExec); abortErr != nil {
						releaseSlot()
						return cliproxyexecutor.Response{}, abortErr
					}
					sameAuthRetries = transientSameAuthRetryBudget(auth, errExec)
					if shouldFallbackToGPT53Codex(currentRouteModel, provider, errExec) {
						fallbackModel := resolveGPT54FallbackModelName(currentRouteModel)
						if fallbackModel != "" && !strings.EqualFold(fallbackModel, currentRouteModel) {
							entry := logEntryWithRequestID(execCtx)
							entry.Infof("stream disconnected detected on gpt-5.4, retrying with fallback model %s", fallbackModel)
							fallbackActive = true
							fallbackRouteModel = fallbackModel
							opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, fallbackModel))
							fallbackVariantNoModel = false
							fallbackVariantAuthAttempts = 0
							fallbackVariantTried = make(map[string]struct{})
							fallbackVariantCountedTried = make(map[string]struct{})
							releaseSlot()
							goto nextExecuteAttempt
						}
					}
					if restrictionErr == nil && shouldRetryAcrossAuths(errExec) {
						restrictionErr = errExec
					}
					if idx < len(models)-1 {
						continue
					}
					break
				}
				m.recordResult(execCtx, result)
				releaseSlot()
				return resp, nil
			}

			if authErr == nil {
				break
			}
			if restrictionErr != nil {
				lastErr = restrictionErr
				if fallbackActive {
					if shouldTreatAsNoAvailableModel(restrictionErr) {
						fallbackVariantNoModel = true
					}
					fallbackVariantAuthAttempts++
					if shouldFallbackGPT53VariantToBase(currentRouteModel, restrictionErr) || (fallbackVariantNoModel && fallbackVariantAuthAttempts > 0) {
						baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
						if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
							entry := logEntryWithRequestID(execCtx)
							entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
							fallbackRouteModel = baseModel
							opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
							fallbackVariantNoModel = false
							fallbackVariantAuthAttempts = 0
							fallbackVariantTried = make(map[string]struct{})
							fallbackVariantCountedTried = make(map[string]struct{})
							releaseSlot()
							goto nextExecuteAttempt
						}
					}
				}
				break
			}
			lastErr = authErr
			if abortErr := shouldAbortRetryLoop(execCtx, authErr); abortErr != nil {
				releaseSlot()
				return cliproxyexecutor.Response{}, abortErr
			}
			if isStreamDisconnectedBeforeCompletionError(authErr) && authAttempt >= streamDisconnectRetryLimit {
				break
			}
			if authAttempt < sameAuthRetries {
				if errWait := waitForCooldown(execCtx, sameAuthRetryDelayFunc()); errWait != nil {
					releaseSlot()
					return cliproxyexecutor.Response{}, errWait
				}
				continue
			}
			break
		}
		if shouldCountRetryCredentialBudget(lastErr) {
			currentCountedTried[auth.ID] = struct{}{}
		}
		releaseSlot()
	nextExecuteAttempt:
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	countedTried := make(map[string]struct{})
	var lastErr error
	fallbackActive := false
	fallbackRouteModel := ""
	fallbackVariantNoModel := false
	fallbackVariantAuthAttempts := 0
	fallbackVariantTried := make(map[string]struct{})
	fallbackVariantCountedTried := make(map[string]struct{})
	for {
		currentRouteModel := routeModel
		currentOpts := opts
		currentTried := tried
		currentCountedTried := countedTried
		if fallbackActive {
			currentRouteModel = fallbackRouteModel
			currentOpts = setRequestedModelMetadata(opts, fallbackRouteModel)
			currentTried = fallbackVariantTried
			currentCountedTried = fallbackVariantCountedTried
		}
		if maxRetryCredentials > 0 && len(currentCountedTried) >= maxRetryCredentials {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, release, errPick := m.pickNextMixedReserved(ctx, providers, currentRouteModel, currentOpts, currentTried)
		if errPick != nil {
			if lastErr != nil && isPoolExhaustedPickError(errPick) {
				if cooldownErr := m.triedModelCooldownError(currentTried, providers, currentRouteModel); cooldownErr != nil {
					return cliproxyexecutor.Response{}, cooldownErr
				}
				if fallbackActive && shouldFallbackGPT53VariantToBase(currentRouteModel, lastErr) {
					baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
					if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
						entry := logEntryWithRequestID(ctx)
						entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
						fallbackRouteModel = baseModel
						opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
						fallbackVariantNoModel = false
						fallbackVariantAuthAttempts = 0
						fallbackVariantTried = make(map[string]struct{})
						fallbackVariantCountedTried = make(map[string]struct{})
						continue
					}
				}
				return cliproxyexecutor.Response{}, lastErr
			}
			if fallbackActive && shouldFallbackGPT53VariantToBase(currentRouteModel, errPick) {
				baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
				if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
					entry := logEntryWithRequestID(ctx)
					entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
					fallbackRouteModel = baseModel
					opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
					fallbackVariantNoModel = false
					fallbackVariantAuthAttempts = 0
					fallbackVariantTried = make(map[string]struct{})
					fallbackVariantCountedTried = make(map[string]struct{})
					continue
				}
			}
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		releaseReserved := release
		releaseSlot := func() {
			if releaseReserved != nil {
				releaseReserved()
				releaseReserved = nil
			}
		}

		currentTried[auth.ID] = struct{}{}
		preparedAuth, errPrepare := m.preparePickedAuth(auth, provider, currentRouteModel)
		if errPrepare != nil {
			releaseSlot()
			lastErr = errPrepare
			continue
		}
		auth = preparedAuth

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, currentRouteModel)
		if fallbackActive && strings.HasPrefix(strings.ToLower(strings.TrimSpace(canonicalModelKey(currentRouteModel))), gpt53CodexModelPrefix+"-") {
			if registryIndicatesModelUnavailable(auth.ID, currentRouteModel) {
				baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
				if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
					entry.Infof("fallback model %s not registered for auth, retrying with base model %s", currentRouteModel, baseModel)
					fallbackRouteModel = baseModel
					opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
					fallbackVariantNoModel = false
					fallbackVariantAuthAttempts = 0
					fallbackVariantTried = make(map[string]struct{})
					fallbackVariantCountedTried = make(map[string]struct{})
					releaseSlot()
					continue
				}
			}
		}
		publishSelectedAuthMetadata(currentOpts.Metadata, auth.ID)
		m.bindExecutionSessionAuth(executionSessionIDFromMetadata(currentOpts.Metadata), auth.ID)

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}

		sameAuthRetries := 0
		for authAttempt := 0; ; authAttempt++ {
			if authAttempt > 0 {
				preparedAuth, errPrepare = m.preparePickedAuth(auth, provider, currentRouteModel)
				if errPrepare != nil {
					if lastErr == nil {
						lastErr = errPrepare
					}
					break
				}
				auth = preparedAuth
			}
			models, pooled := m.preparedExecutionModels(auth, currentRouteModel)
			if len(models) == 0 {
				releaseSlot()
				goto nextCountAttempt
			}
			var (
				authErr        error
				restrictionErr error
			)

			for idx, upstreamModel := range models {
				resultModel := executionResultModel(currentRouteModel, upstreamModel, pooled)
				execReq := req
				execReq.Model = upstreamModel
				m.noteFreeCodexUse(execCtx, auth.ID)
				attemptStartedAt := time.Now()
				attemptCtx, cancel := withAttemptTimeout(execCtx, countAttemptTimeout(execCtx), newCountAttemptTimeoutError)
				resp, errExec := executor.CountTokens(attemptCtx, auth, execReq, currentOpts)
				cancel()
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: errExec == nil, Latency: time.Since(attemptStartedAt)}
				if errExec != nil {
					if errCtx := execCtx.Err(); errCtx != nil {
						releaseSlot()
						return cliproxyexecutor.Response{}, errCtx
					}
					result.Error = &Error{Message: errExec.Error()}
					if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
						result.Error.HTTPStatus = se.StatusCode()
					}
					if ra := retryAfterFromError(errExec); ra != nil {
						result.RetryAfter = ra
					}
					result = adjustResultForPotentialGPT54Fallback(provider, currentRouteModel, errExec, result)
					m.recordResult(execCtx, result)
					if isRequestInvalidError(errExec) {
						releaseSlot()
						return cliproxyexecutor.Response{}, errExec
					}

					authErr = errExec
					if abortErr := shouldAbortRetryLoop(execCtx, errExec); abortErr != nil {
						releaseSlot()
						return cliproxyexecutor.Response{}, abortErr
					}
					sameAuthRetries = transientSameAuthRetryBudget(auth, errExec)
					if shouldFallbackToGPT53Codex(currentRouteModel, provider, errExec) {
						fallbackModel := resolveGPT54FallbackModelName(currentRouteModel)
						if fallbackModel != "" && !strings.EqualFold(fallbackModel, currentRouteModel) {
							entry := logEntryWithRequestID(execCtx)
							entry.Infof("stream disconnected detected on gpt-5.4, retrying with fallback model %s", fallbackModel)
							fallbackActive = true
							fallbackRouteModel = fallbackModel
							opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, fallbackModel))
							fallbackVariantNoModel = false
							fallbackVariantAuthAttempts = 0
							fallbackVariantTried = make(map[string]struct{})
							fallbackVariantCountedTried = make(map[string]struct{})
							releaseSlot()
							goto nextCountAttempt
						}
					}
					if restrictionErr == nil && shouldRetryAcrossAuths(errExec) {
						restrictionErr = errExec
					}
					if idx < len(models)-1 {
						continue
					}
					break
				}
				m.recordResult(execCtx, result)
				releaseSlot()
				return resp, nil
			}

			if authErr == nil {
				break
			}
			if restrictionErr != nil {
				lastErr = restrictionErr
				if fallbackActive {
					if shouldTreatAsNoAvailableModel(restrictionErr) {
						fallbackVariantNoModel = true
					}
					fallbackVariantAuthAttempts++
					if shouldFallbackGPT53VariantToBase(currentRouteModel, restrictionErr) || (fallbackVariantNoModel && fallbackVariantAuthAttempts > 0) {
						baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
						if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
							entry := logEntryWithRequestID(execCtx)
							entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
							fallbackRouteModel = baseModel
							opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
							fallbackVariantNoModel = false
							fallbackVariantAuthAttempts = 0
							fallbackVariantTried = make(map[string]struct{})
							fallbackVariantCountedTried = make(map[string]struct{})
							releaseSlot()
							goto nextCountAttempt
						}
					}
				}
				break
			}
			lastErr = authErr
			if abortErr := shouldAbortRetryLoop(execCtx, authErr); abortErr != nil {
				releaseSlot()
				return cliproxyexecutor.Response{}, abortErr
			}
			if isStreamDisconnectedBeforeCompletionError(authErr) && authAttempt >= streamDisconnectRetryLimit {
				break
			}
			if authAttempt < sameAuthRetries {
				if errWait := waitForCooldown(execCtx, sameAuthRetryDelayFunc()); errWait != nil {
					releaseSlot()
					return cliproxyexecutor.Response{}, errWait
				}
				continue
			}
			break
		}
		if shouldCountRetryCredentialBudget(lastErr) {
			currentCountedTried[auth.ID] = struct{}{}
		}
		releaseSlot()
	nextCountAttempt:
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, maxRetryCredentials int) (*cliproxyexecutor.StreamResult, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	countedTried := make(map[string]struct{})
	var lastErr error
	fallbackActive := false
	fallbackRouteModel := ""
	fallbackVariantNoModel := false
	fallbackVariantAuthAttempts := 0
	fallbackVariantTried := make(map[string]struct{})
	fallbackVariantCountedTried := make(map[string]struct{})
	for {
		currentRouteModel := routeModel
		currentOpts := opts
		currentTried := tried
		currentCountedTried := countedTried
		if fallbackActive {
			currentRouteModel = fallbackRouteModel
			currentOpts = setRequestedModelMetadata(opts, fallbackRouteModel)
			currentTried = fallbackVariantTried
			currentCountedTried = fallbackVariantCountedTried
		}
		if maxRetryCredentials > 0 && len(currentCountedTried) >= maxRetryCredentials {
			if lastErr != nil {
				var bootstrapErr *streamBootstrapError
				if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
					return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
				}
				return nil, lastErr
			}
			return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		auth, executor, provider, release, errPick := m.pickNextMixedReserved(ctx, providers, currentRouteModel, currentOpts, currentTried)
		if errPick != nil {
			if lastErr != nil && isPoolExhaustedPickError(errPick) {
				if cooldownErr := m.triedModelCooldownError(currentTried, providers, currentRouteModel); cooldownErr != nil {
					return nil, cooldownErr
				}
				if fallbackActive && shouldFallbackGPT53VariantToBase(currentRouteModel, lastErr) {
					baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
					if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
						entry := logEntryWithRequestID(ctx)
						entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
						fallbackRouteModel = baseModel
						opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
						fallbackVariantNoModel = false
						fallbackVariantAuthAttempts = 0
						fallbackVariantTried = make(map[string]struct{})
						fallbackVariantCountedTried = make(map[string]struct{})
						continue
					}
				}
				return nil, lastErr
			}
			if fallbackActive && shouldFallbackGPT53VariantToBase(currentRouteModel, errPick) {
				baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
				if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
					entry := logEntryWithRequestID(ctx)
					entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
					fallbackRouteModel = baseModel
					opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
					fallbackVariantNoModel = false
					fallbackVariantAuthAttempts = 0
					fallbackVariantTried = make(map[string]struct{})
					fallbackVariantCountedTried = make(map[string]struct{})
					continue
				}
			}
			if lastErr != nil {
				var bootstrapErr *streamBootstrapError
				if errors.As(lastErr, &bootstrapErr) && bootstrapErr != nil {
					return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
				}
				return nil, lastErr
			}
			return nil, errPick
		}

		releaseReserved := release
		releaseSlot := func() {
			if releaseReserved != nil {
				releaseReserved()
				releaseReserved = nil
			}
		}

		currentTried[auth.ID] = struct{}{}
		preparedAuth, errPrepare := m.preparePickedAuth(auth, provider, currentRouteModel)
		if errPrepare != nil {
			releaseSlot()
			lastErr = errPrepare
			continue
		}
		auth = preparedAuth

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, currentRouteModel)
		if fallbackActive && strings.HasPrefix(strings.ToLower(strings.TrimSpace(canonicalModelKey(currentRouteModel))), gpt53CodexModelPrefix+"-") {
			if registryIndicatesModelUnavailable(auth.ID, currentRouteModel) {
				baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
				if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
					entry.Infof("fallback model %s not registered for auth, retrying with base model %s", currentRouteModel, baseModel)
					fallbackRouteModel = baseModel
					opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
					fallbackVariantNoModel = false
					fallbackVariantAuthAttempts = 0
					fallbackVariantTried = make(map[string]struct{})
					fallbackVariantCountedTried = make(map[string]struct{})
					releaseSlot()
					continue
				}
			}
		}
		publishSelectedAuthMetadata(currentOpts.Metadata, auth.ID)
		m.bindExecutionSessionAuth(executionSessionIDFromMetadata(currentOpts.Metadata), auth.ID)

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}

		sameAuthRetries := 0
		for authAttempt := 0; ; authAttempt++ {
			if authAttempt > 0 {
				preparedAuth, errPrepare = m.preparePickedAuth(auth, provider, currentRouteModel)
				if errPrepare != nil {
					lastErr = errPrepare
					break
				}
				auth = preparedAuth
			}
			streamExecModels, streamPooled := m.preparedExecutionModels(auth, currentRouteModel)
			if len(streamExecModels) == 0 {
				releaseSlot()
				goto nextStreamAttempt
			}
			streamResult, errStream := m.executeStreamWithModelPool(execCtx, executor, auth, provider, req, currentOpts, currentRouteModel, streamExecModels, streamPooled)
			if errStream != nil {
				if errCtx := execCtx.Err(); errCtx != nil {
					releaseSlot()
					return nil, errCtx
				}
				if abortErr := shouldAbortRetryLoop(execCtx, errStream); abortErr != nil {
					releaseSlot()
					return nil, abortErr
				}
				if isRequestInvalidError(errStream) {
					releaseSlot()
					return nil, errStream
				}
				sameAuthRetries = transientSameAuthRetryBudget(auth, errStream)
				if shouldFallbackToGPT53Codex(currentRouteModel, provider, errStream) {
					fallbackModel := resolveGPT54FallbackModelName(currentRouteModel)
					if fallbackModel != "" && !strings.EqualFold(fallbackModel, currentRouteModel) {
						entry := logEntryWithRequestID(execCtx)
						entry.Infof("stream disconnected detected on gpt-5.4, retrying with fallback model %s", fallbackModel)
						fallbackActive = true
						fallbackRouteModel = fallbackModel
						opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, fallbackModel))
						fallbackVariantNoModel = false
						fallbackVariantAuthAttempts = 0
						fallbackVariantTried = make(map[string]struct{})
						fallbackVariantCountedTried = make(map[string]struct{})
						releaseSlot()
						goto nextStreamAttempt
					}
				}
				if isStreamDisconnectedBeforeCompletionError(errStream) && authAttempt < streamDisconnectRetryLimit {
					if errWait := waitForCooldown(execCtx, streamDisconnectRetryDelayFunc()); errWait != nil {
						releaseSlot()
						return nil, errWait
					}
					lastErr = errStream
					break
				}
				if shouldRetryAcrossAuthPool(errStream) {
					lastErr = errStream
					if fallbackActive {
						if shouldTreatAsNoAvailableModel(errStream) {
							fallbackVariantNoModel = true
						}
						fallbackVariantAuthAttempts++
						if shouldFallbackGPT53VariantToBase(currentRouteModel, errStream) || (fallbackVariantNoModel && fallbackVariantAuthAttempts > 0) {
							baseModel := downgradeGPT53CodexVariantToBase(currentRouteModel)
							if baseModel != "" && !strings.EqualFold(baseModel, currentRouteModel) {
								entry := logEntryWithRequestID(execCtx)
								entry.Infof("fallback model %s has no available channel, retrying with base model %s", currentRouteModel, baseModel)
								fallbackRouteModel = baseModel
								opts = withGPT53CodexFallbackSkipRegistry(setRequestedModelMetadata(opts, baseModel))
								fallbackVariantNoModel = false
								fallbackVariantAuthAttempts = 0
								fallbackVariantTried = make(map[string]struct{})
								fallbackVariantCountedTried = make(map[string]struct{})
								releaseSlot()
								goto nextStreamAttempt
							}
						}
					}
					break
				}
				if authAttempt < sameAuthRetries {
					if errWait := waitForCooldown(execCtx, sameAuthRetryDelayFunc()); errWait != nil {
						releaseSlot()
						return nil, errWait
					}
					continue
				}
				if isStreamSetupTimeoutError(errStream) {
					releaseSlot()
					return nil, errStream
				}
				lastErr = errStream
				break
			}
			streamResult = attachStreamResultRelease(execCtx, streamResult, releaseReserved)
			releaseReserved = nil
			return streamResult, nil
		}
		if shouldCountRetryCredentialBudget(lastErr) {
			currentCountedTried[auth.ID] = struct{}{}
		}
		releaseSlot()
	nextStreamAttempt:
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

func executionSessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ExecutionSessionMetadataKey]
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

func metadataBool(meta map[string]any, key string) bool {
	if len(meta) == 0 {
		return false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	raw, ok := meta[key]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes" || s == "y"
	case int:
		return v != 0
	case int32:
		return v != 0
	case int64:
		return v != 0
	case float32:
		return v != 0
	case float64:
		return v != 0
	default:
		return false
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

func (m *Manager) bindExecutionSessionAuth(sessionID string, authID string) {
	if m == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if sessionID == "" || authID == "" {
		return
	}
	m.mu.Lock()
	if m.executionSessionAuth == nil {
		m.executionSessionAuth = make(map[string]executionSessionBinding)
	}
	now := executionSessionAffinityNowFunc()
	binding, ok := m.executionSessionAuth[sessionID]
	if ok && strings.TrimSpace(binding.AuthID) == authID && (binding.ExpiresAt.IsZero() || binding.ExpiresAt.After(now)) {
		m.mu.Unlock()
		return
	}
	m.executionSessionAuth[sessionID] = executionSessionBinding{
		AuthID:    authID,
		ExpiresAt: now.Add(executionSessionAffinityTTL),
	}
	m.mu.Unlock()
}

func recordAuthLastUsedAt(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataLastUsedAtKey] = now.UTC().Format(time.RFC3339Nano)
}

func (m *Manager) executionSessionPreferredAuthID(sessionID string) string {
	if m == nil {
		return ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	now := executionSessionAffinityNowFunc()
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, ok := m.executionSessionAuth[sessionID]
	if !ok {
		return ""
	}
	if !binding.ExpiresAt.IsZero() && !binding.ExpiresAt.After(now) {
		delete(m.executionSessionAuth, sessionID)
		return ""
	}
	authID := strings.TrimSpace(binding.AuthID)
	if authID == "" {
		delete(m.executionSessionAuth, sessionID)
		return ""
	}
	return authID
}

func (m *Manager) unbindExecutionSession(sessionID string) {
	if m == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	m.unbindExecutionSessionLocked(sessionID)
	m.mu.Unlock()
}

func (m *Manager) unbindExecutionSessionsForAuth(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	m.mu.Lock()
	m.unbindExecutionSessionsForAuthLocked(authID)
	m.mu.Unlock()
}

func (m *Manager) unbindExecutionSessionLocked(sessionID string) {
	if m == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	delete(m.executionSessionAuth, sessionID)
}

func (m *Manager) unbindExecutionSessionsForAuthLocked(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	for sessionID, binding := range m.executionSessionAuth {
		if strings.TrimSpace(binding.AuthID) == authID {
			delete(m.executionSessionAuth, sessionID)
		}
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
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func effectiveRequestRetry(defaultRetry int, auth *Auth) int {
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			defaultRetry = override
		}
	}
	if defaultRetry < 0 {
		return 0
	}
	return defaultRetry
}

func effectiveSameAuthRetry(auth *Auth) int {
	if auth != nil {
		if override, ok := auth.SameAuthRetryOverride(); ok {
			if override < 0 {
				return 0
			}
			return override
		}
	}
	return 0
}

func compactExecuteAttemptTimeout(cfg *internalconfig.Config) time.Duration {
	_ = cfg
	return 0
}

// CompactExecuteAttemptTimeout returns the recommended non-streaming timeout budget for /responses/compact.
func CompactExecuteAttemptTimeout(cfg *internalconfig.Config) time.Duration {
	return compactExecuteAttemptTimeout(cfg)
}

func transientSameAuthRetryBudget(auth *Auth, err error) int {
	retries := effectiveSameAuthRetry(auth)
	if retries > 0 {
		return retries
	}
	if isStreamDisconnectedBeforeCompletionError(err) {
		return defaultTransientSameAuthRetry
	}
	return 0
}

func isCompactExecuteTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		code := strings.ToLower(strings.TrimSpace(authErr.Code))
		message := strings.ToLower(strings.TrimSpace(authErr.Message))
		if code == "execute_timeout" && strings.Contains(message, "context deadline exceeded") {
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(message, "context deadline exceeded") &&
		strings.Contains(message, "/responses/compact")
}

func (m *Manager) maxInflightPerAuth(ctx context.Context, auth *Auth) int {
	if auth != nil {
		if override, ok := auth.MaxInflightOverride(); ok {
			if override < 0 {
				return 0
			}
			return override
		}
	}
	cfg := runtimeConfigFromContext(ctx)
	if cfg == nil {
		cfg = m.CurrentConfig()
	}
	if cfg == nil {
		return 0
	}
	if cfg.Routing.MaxInflightPerAuth < 0 {
		return 0
	}
	return cfg.Routing.MaxInflightPerAuth
}

func (m *Manager) tryAcquireAuthExecutionSlot(ctx context.Context, auth *Auth) (func(), bool, int, int) {
	if m == nil || auth == nil {
		return nil, false, 0, 0
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return nil, false, 0, 0
	}
	limit := m.maxInflightPerAuth(ctx, auth)
	if limit <= 0 {
		return func() {}, true, 0, 0
	}

	m.authInflightMu.Lock()
	current := m.authInflight[authID]
	if current >= limit {
		m.authInflightMu.Unlock()
		return nil, false, current, limit
	}
	current++
	m.authInflight[authID] = current
	m.authInflightMu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			m.releaseAuthExecutionSlot(authID)
		})
	}
	return release, true, current, limit
}

func (m *Manager) forceAcquireAuthExecutionSlot(auth *Auth) (func(), int) {
	if m == nil || auth == nil {
		return nil, 0
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return nil, 0
	}

	m.authInflightMu.Lock()
	current := m.authInflight[authID] + 1
	m.authInflight[authID] = current
	m.authInflightMu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			m.releaseAuthExecutionSlot(authID)
		})
	}
	return release, current
}

func (m *Manager) allowInflightOverflowWhenExhausted(ctx context.Context) bool {
	if m == nil {
		return false
	}
	cfg := runtimeConfigFromContext(ctx)
	if cfg == nil {
		cfg = m.CurrentConfig()
	}
	if cfg == nil {
		return false
	}
	return cfg.Routing.AllowInflightOverflowWhenExhausted
}

func (m *Manager) releaseAuthExecutionSlot(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	m.authInflightMu.Lock()
	defer m.authInflightMu.Unlock()
	current := m.authInflight[authID]
	if current <= 1 {
		delete(m.authInflight, authID)
		return
	}
	m.authInflight[authID] = current - 1
}

func mergeTriedAuthSets(base map[string]struct{}, extra map[string]struct{}) map[string]struct{} {
	if len(extra) == 0 {
		return base
	}
	merged := make(map[string]struct{}, len(base)+len(extra))
	for authID := range base {
		merged[authID] = struct{}{}
	}
	for authID := range extra {
		merged[authID] = struct{}{}
	}
	return merged
}

func newAuthBusyError() *Error {
	return &Error{
		Code:       "auth_busy",
		Message:    "all eligible auths are busy under in-flight limit",
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
}

func logAuthInflightLimited(ctx context.Context, auth *Auth, provider string, current int, limit int) {
	entry := logEntryWithRequestID(ctx)
	if auth != nil {
		entry = entry.WithField("auth_id", auth.ID)
		if auth.Index != "" {
			entry = entry.WithField("auth_index", auth.Index)
		}
	}
	if strings.TrimSpace(provider) != "" {
		entry = entry.WithField("provider", provider)
	}
	entry.WithFields(log.Fields{
		"inflight": current,
		"limit":    limit,
	}).Debug("auth in-flight limit reached, repicking auth")
}

func logAuthInflightExhausted(ctx context.Context, providers []string, model string, skipped int) {
	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"providers":     strings.Join(providers, ","),
		"busy_auths":    skipped,
		"routing_model": strings.TrimSpace(model),
	}).Warn("all eligible auths are busy under in-flight limit")
}

func logAuthInflightOverflow(ctx context.Context, auth *Auth, provider string, current int, limit int) {
	entry := logEntryWithRequestID(ctx)
	if auth != nil {
		entry = entry.WithField("auth_id", auth.ID)
		if auth.Index != "" {
			entry = entry.WithField("auth_index", auth.Index)
		}
	}
	if strings.TrimSpace(provider) != "" {
		entry = entry.WithField("provider", provider)
	}
	entry.WithFields(log.Fields{
		"inflight": current,
		"limit":    limit,
	}).Warn("temporarily exceeded auth in-flight limit because all eligible auths were busy")
}

func (m *Manager) pickNextMixedReserved(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, func(), error) {
	if m == nil {
		return nil, nil, "", nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) == "" {
		sessionID := executionSessionIDFromMetadata(opts.Metadata)
		if sessionID != "" {
			if auth, executor, provider, release, ok := m.pickPreferredExecutionSessionAuthReserved(ctx, sessionID, providers, model, opts, tried, false); ok {
				return auth, executor, provider, release, nil
			}
		}
	}
	busySkipped := make(map[string]struct{})
	for {
		pickTried := mergeTriedAuthSets(tried, busySkipped)
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, model, opts, pickTried)
		if errPick != nil {
			if len(busySkipped) > 0 && isPoolExhaustedPickError(errPick) {
				if m.allowInflightOverflowWhenExhausted(ctx) {
					if overflowAuth, overflowExecutor, overflowProvider, overflowRelease, ok := m.pickNextMixedReservedWithOverflow(ctx, providers, model, opts, tried); ok {
						return overflowAuth, overflowExecutor, overflowProvider, overflowRelease, nil
					}
				}
				logAuthInflightExhausted(ctx, providers, model, len(busySkipped))
				return nil, nil, "", nil, newAuthBusyError()
			}
			return nil, nil, "", nil, errPick
		}
		release, ok, current, limit := m.tryAcquireAuthExecutionSlot(ctx, auth)
		if ok {
			return auth, executor, provider, release, nil
		}
		busySkipped[auth.ID] = struct{}{}
		logAuthInflightLimited(ctx, auth, provider, current, limit)
	}
}

func (m *Manager) pickNextMixedReservedWithOverflow(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, func(), bool) {
	if m == nil {
		return nil, nil, "", nil, false
	}
	if pinnedAuthIDFromMetadata(opts.Metadata) == "" {
		sessionID := executionSessionIDFromMetadata(opts.Metadata)
		if sessionID != "" {
			if auth, executor, provider, release, ok := m.pickPreferredExecutionSessionAuthReserved(ctx, sessionID, providers, model, opts, tried, true); ok {
				return auth, executor, provider, release, true
			}
		}
	}
	auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, model, opts, tried)
	if errPick != nil || auth == nil || executor == nil {
		return nil, nil, "", nil, false
	}
	limit := m.maxInflightPerAuth(ctx, auth)
	release, current := m.forceAcquireAuthExecutionSlot(auth)
	if release == nil {
		return nil, nil, "", nil, false
	}
	logAuthInflightOverflow(ctx, auth, provider, current, limit)
	return auth, executor, provider, release, true
}

func (m *Manager) pickPreferredExecutionSessionAuthReserved(ctx context.Context, sessionID string, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, allowOverflow bool) (*Auth, ProviderExecutor, string, func(), bool) {
	if m == nil {
		return nil, nil, "", nil, false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil, "", nil, false
	}
	authID := m.executionSessionPreferredAuthID(sessionID)
	if authID == "" {
		return nil, nil, "", nil, false
	}
	if _, used := tried[authID]; used {
		return nil, nil, "", nil, false
	}

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey != "" {
			providerSet[providerKey] = struct{}{}
		}
	}

	m.mu.RLock()
	currentAuth := m.auths[authID]
	if currentAuth == nil {
		m.mu.RUnlock()
		m.unbindExecutionSession(sessionID)
		return nil, nil, "", nil, false
	}
	authSnapshot := currentAuth.Clone()
	m.mu.RUnlock()

	providerKey := strings.TrimSpace(strings.ToLower(authSnapshot.Provider))
	if providerKey == "" {
		m.unbindExecutionSession(sessionID)
		return nil, nil, "", nil, false
	}
	if len(providerSet) > 0 {
		if _, ok := providerSet[providerKey]; !ok {
			return nil, nil, "", nil, false
		}
	}
	if authSnapshot.Disabled {
		return nil, nil, "", nil, false
	}
	if !metadataBool(opts.Metadata, cliproxyexecutor.SkipModelRegistryCheckMetadataKey) {
		modelKey := strings.TrimSpace(model)
		if modelKey != "" {
			parsed := thinking.ParseSuffix(modelKey)
			if parsed.ModelName != "" {
				modelKey = strings.TrimSpace(parsed.ModelName)
			}
		}
		if modelKey != "" {
			registryRef := registry.GetGlobalRegistry()
			if registryRef != nil && !registryRef.ClientSupportsModel(authSnapshot.ID, modelKey) {
				return nil, nil, "", nil, false
			}
		}
	}

	if _, err := m.preparePickedAuth(authSnapshot, providerKey, model); err != nil {
		return nil, nil, "", nil, false
	}

	executor := m.executorFor(providerKey)
	if executor == nil {
		return nil, nil, "", nil, false
	}
	release, ok, inflightCurrent, limit := m.tryAcquireAuthExecutionSlot(ctx, authSnapshot)
	if !ok && allowOverflow {
		release, inflightCurrent = m.forceAcquireAuthExecutionSlot(authSnapshot)
		if release != nil {
			logAuthInflightOverflow(ctx, authSnapshot, providerKey, inflightCurrent, limit)
			ok = true
		}
	}
	if !ok {
		return nil, nil, "", nil, false
	}
	return authSnapshot, executor, providerKey, release, true
}

func attachStreamResultRelease(ctx context.Context, streamResult *cliproxyexecutor.StreamResult, release func()) *cliproxyexecutor.StreamResult {
	if streamResult == nil || release == nil {
		return streamResult
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer release()
		for chunk := range streamResult.Chunks {
			if ctx == nil {
				out <- chunk
				continue
			}
			select {
			case <-ctx.Done():
				discardStreamChunks(streamResult.Chunks)
				return
			case out <- chunk:
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: streamResult.Headers, Chunks: out}
}

func isPoolExhaustedPickError(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		switch strings.ToLower(strings.TrimSpace(authErr.Code)) {
		case "auth_not_found", "auth_unavailable", "auth_busy":
			return true
		default:
		}
		msg := strings.ToLower(strings.TrimSpace(authErr.Message))
		return strings.Contains(msg, "no auth available") || strings.Contains(msg, "selector returned no auth")
	}
	return false
}

func (m *Manager) triedModelCooldownError(tried map[string]struct{}, providers []string, model string) *modelCooldownError {
	if m == nil || len(tried) == 0 || len(providers) == 0 {
		return nil
	}
	providerForError := ""
	if len(providers) == 1 {
		providerForError = strings.ToLower(strings.TrimSpace(providers[0]))
	}

	now := time.Now()
	total := 0
	cooldownCount := 0
	earliest := time.Time{}

	m.mu.RLock()
	for authID := range tried {
		auth := m.auths[authID]
		if auth == nil {
			continue
		}
		total++

		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		if !blocked || reason != blockReasonCooldown || next.IsZero() || !next.After(now) {
			continue
		}
		cooldownCount++
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
	}
	m.mu.RUnlock()

	if total == 0 || cooldownCount != total || earliest.IsZero() {
		return nil
	}
	resetIn := earliest.Sub(now)
	if resetIn < 0 {
		resetIn = 0
	}
	return newModelCooldownError(model, providerForError, resetIn)
}

func nextSameAuthRetryDelay() time.Duration {
	if sameAuthRetryMaxWait <= sameAuthRetryMinWait {
		return sameAuthRetryMinWait
	}
	window := sameAuthRetryMaxWait - sameAuthRetryMinWait
	return sameAuthRetryMinWait + time.Duration(rand.Int64N(int64(window)+1))
}

func nextStreamDisconnectRetryDelay() time.Duration {
	if streamDisconnectRetryMaxWait <= streamDisconnectRetryMinWait {
		return streamDisconnectRetryMinWait
	}
	window := streamDisconnectRetryMaxWait - streamDisconnectRetryMinWait
	return streamDisconnectRetryMinWait + time.Duration(rand.Int64N(int64(window)+1))
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	planType := strings.ToLower(strings.TrimSpace(codexPlanType(auth)))
	return planType == "free" || planType == "none"
}

func freeCodexAutoDeleteDisabled(cfg *internalconfig.Config) bool {
	if cfg == nil {
		return false
	}
	return cfg.Routing.DisableFreeCodexAutoDelete
}

func freeCodexFirstUsedAt(auth *Auth) (time.Time, bool) {
	if auth == nil || auth.Metadata == nil {
		return time.Time{}, false
	}
	raw, ok := auth.Metadata[metadataFreeFirstUsedAtKey]
	if !ok {
		return time.Time{}, false
	}
	ts, ok := parseTimeValue(raw)
	if !ok || ts.IsZero() {
		return time.Time{}, false
	}
	return ts, true
}

func ensureFreeCodexFirstUsedAt(auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if ts, ok := freeCodexFirstUsedAt(auth); ok {
		return ts, false
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	ts := now.UTC()
	auth.Metadata[metadataFreeFirstUsedAtKey] = ts.Format(time.RFC3339Nano)
	return ts, true
}

func freeCodexDeleteReasonFromMessage(message string) string {
	msg := strings.ToLower(strings.TrimSpace(message))
	if msg == "" {
		return ""
	}
	for _, needle := range []string{
		"unauthorized",
		"token_revoked",
		"token_invalidated",
		"account_deactivated",
		"must be a member of an organization to use the api",
		"deactivated_workspace",
		"encountered invalidated oauth token for user",
		"your authentication token has been invalidated. please try signing in again.",
		"your openai account has been deactivated",
		"account has been deactivated",
		"authorization lost",
		"forbidden",
		"usage limit",
		"rate limit",
		"rate limited",
		"too many requests",
		"selected model is at capacity",
	} {
		if strings.Contains(msg, needle) {
			return strings.TrimSpace(message)
		}
	}
	return ""
}

func freeCodexPlanTypeFromErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	for _, path := range []string{"error.plan_type", "plan_type"} {
		if value := strings.TrimSpace(gjson.Get(message, path).String()); value != "" {
			return strings.ToLower(value)
		}
	}
	return ""
}

func isFreeCodexDeleteCandidate(auth *Auth, resultErr *Error) bool {
	if auth != nil && isFreeCodexAuth(auth) {
		return true
	}
	if resultErr == nil {
		return false
	}
	planType := freeCodexPlanTypeFromErrorMessage(resultErr.Message)
	return planType == "free" || planType == "none"
}

func shouldAutoDisableFreeCodexAuth(auth *Auth, resultErr *Error, cfg *internalconfig.Config) bool {
	if !freeCodexAutoDeleteDisabled(cfg) {
		return false
	}
	if auth == nil || resultErr == nil || !isFreeCodexDeleteCandidate(auth, resultErr) {
		return false
	}
	if resultErr.StatusCode() != http.StatusTooManyRequests {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(resultErr.Message))
	if msg == "" {
		return true
	}
	return strings.Contains(msg, "usage limit") ||
		strings.Contains(msg, "usage_limit_reached") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "rate limited") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "selected model is at capacity")
}

func shouldAutoDisableCodexAuthOnQuotaExceeded(auth *Auth, resultErr *Error, cfg *internalconfig.Config) bool {
	if shouldAutoDisableFreeCodexAuth(auth, resultErr, cfg) {
		return true
	}
	if auth == nil || resultErr == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if resultErr.StatusCode() != http.StatusTooManyRequests {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(resultErr.Message))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "selected model is at capacity") {
		return false
	}
	return strings.Contains(msg, "usage limit") ||
		strings.Contains(msg, "usage_limit_reached")
}

func isGenericCodexRateLimitError(auth *Auth, resultErr *Error) bool {
	if auth == nil || resultErr == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if resultErr.StatusCode() != http.StatusTooManyRequests {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(resultErr.Message))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "selected model is at capacity") {
		return false
	}
	if strings.Contains(msg, "usage limit") || strings.Contains(msg, "usage_limit_reached") {
		return false
	}
	return strings.Contains(msg, "rate limit exceeded")
}

func isStreamDisconnectedBeforeCompletionError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	if strings.Contains(message, "stream disconnected before completion") {
		return true
	}
	if strings.Contains(message, "stream closed before response.completed") {
		return true
	}
	if strings.Contains(message, "selected model is at capacity") {
		return true
	}
	if strings.Contains(message, "error sending request for url") {
		return true
	}
	if strings.Contains(message, "goaway") || strings.Contains(message, "protocol_error") {
		return true
	}
	if strings.Contains(message, "server had an error processing your request") {
		return true
	}
	return false
}

func isStreamSetupTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		code := strings.ToLower(strings.TrimSpace(authErr.Code))
		if code == "stream_bootstrap_timeout" || code == "stream_connect_timeout" {
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	return strings.Contains(message, "stream_bootstrap_timeout") ||
		strings.Contains(message, "timed out waiting for first payload") ||
		strings.Contains(message, "stream_connect_timeout") ||
		strings.Contains(message, "upstream stream setup timed out")
}

func shouldRetryAcrossAuthPool(err error) bool {
	if isStreamSetupTimeoutError(err) {
		return false
	}
	return shouldRetryAcrossAuths(err)
}

func shouldDeleteFreeCodexAuth(auth *Auth, resultErr *Error, cfg *internalconfig.Config) (bool, string) {
	if freeCodexAutoDeleteDisabled(cfg) {
		return false, ""
	}
	if auth == nil || resultErr == nil || !hasPersistedAuthRecord(auth) || !isFreeCodexDeleteCandidate(auth, resultErr) {
		return false, ""
	}
	if reason := freeCodexDeleteReasonFromMessage(resultErr.Message); reason != "" {
		return true, reason
	}
	switch resultErr.StatusCode() {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		reason := strings.TrimSpace(resultErr.Message)
		if reason == "" {
			reason = "free codex auth removed after terminal upstream status"
		}
		return true, reason
	default:
		return false, ""
	}
}

func freeCodexAuthExpiredAt(auth *Auth) (time.Time, bool) {
	if !isFreeCodexAuth(auth) {
		return time.Time{}, false
	}
	firstUsedAt, ok := freeCodexFirstUsedAt(auth)
	if !ok {
		return time.Time{}, false
	}
	return firstUsedAt.Add(freeCodexAuthTTL), true
}

func freeCodexAuthExpired(auth *Auth, now time.Time) (time.Time, bool) {
	expireAt, ok := freeCodexAuthExpiredAt(auth)
	if !ok {
		return time.Time{}, false
	}
	return expireAt, !expireAt.After(now)
}

func (m *Manager) noteFreeCodexUse(ctx context.Context, authID string) (*Auth, bool) {
	if m == nil {
		return nil, false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, false
	}

	var (
		authSnapshot         *Auth
		persistAuth          *Auth
		scheduleFreeExpiryAt time.Time
	)

	m.mu.Lock()
	auth := m.auths[authID]
	if auth != nil && isFreeCodexAuth(auth) && !freeCodexAutoDeleteDisabled(m.CurrentConfig()) {
		now := time.Now()
		firstUsedAt, _ := ensureFreeCodexFirstUsedAt(auth, now)
		scheduleFreeExpiryAt = firstUsedAt.Add(freeCodexAuthTTL)
		authSnapshot = auth.Clone()
		persistAuth = authSnapshot.Clone()
	}
	m.mu.Unlock()

	if authSnapshot != nil {
		if m.scheduler != nil {
			m.scheduler.upsertAuth(authSnapshot)
		}
		if !scheduleFreeExpiryAt.IsZero() {
			m.scheduleFreeAuthExpiry(authSnapshot, scheduleFreeExpiryAt)
		}
		if persistAuth != nil {
			_ = m.persist(ctx, persistAuth)
		}
		m.hook.OnAuthUpdated(ctx, authSnapshot.Clone())
	}
	return authSnapshot, false
}

func (m *Manager) stopFreeAuthExpiryTimerLocked(authID string) {
	if m == nil || m.freeAuthExpiryIndex == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	item := m.freeAuthExpiryIndex[authID]
	if item == nil {
		return
	}
	heap.Remove(&m.freeAuthExpiryHeap, item.index)
	delete(m.freeAuthExpiryIndex, authID)
}

func (m *Manager) scheduleFreeAuthExpiry(auth *Auth, deleteAt time.Time) {
	if m == nil {
		return
	}
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	if authID == "" {
		return
	}

	m.mu.Lock()
	if current := m.auths[authID]; current == nil || !isFreeCodexAuth(current) {
		m.stopFreeAuthExpiryTimerLocked(authID)
		m.mu.Unlock()
		return
	}
	m.stopFreeAuthExpiryTimerLocked(authID)
	item := &freeAuthExpiryItem{
		authID:   authID,
		expireAt: deleteAt,
	}
	heap.Push(&m.freeAuthExpiryHeap, item)
	if m.freeAuthExpiryIndex == nil {
		m.freeAuthExpiryIndex = make(map[string]*freeAuthExpiryItem)
	}
	m.freeAuthExpiryIndex[authID] = item
	m.mu.Unlock()
	m.signalFreeAuthExpiryWorker()
}

func (m *Manager) syncFreeAuthExpiryTimer(auth *Auth) {
	if m == nil || auth == nil {
		return
	}
	if !isFreeCodexAuth(auth) {
		m.mu.Lock()
		m.stopFreeAuthExpiryTimerLocked(auth.ID)
		m.mu.Unlock()
		m.signalFreeAuthExpiryWorker()
		return
	}
	firstUsedAt, ok := freeCodexFirstUsedAt(auth)
	if !ok {
		m.mu.Lock()
		m.stopFreeAuthExpiryTimerLocked(auth.ID)
		m.mu.Unlock()
		m.signalFreeAuthExpiryWorker()
		return
	}
	m.scheduleFreeAuthExpiry(auth, firstUsedAt.Add(freeCodexAuthTTL))
}

func (m *Manager) expireFreeAuth(authID string, scheduledAt time.Time) {
	if m == nil {
		return
	}
	if freeCodexAutoDeleteDisabled(m.CurrentConfig()) {
		m.mu.Lock()
		m.stopFreeAuthExpiryTimerLocked(authID)
		m.mu.Unlock()
		return
	}
	var removed *Auth
	m.mu.Lock()
	m.stopFreeAuthExpiryTimerLocked(authID)
	current := m.auths[authID]
	if current != nil && isFreeCodexAuth(current) {
		if firstUsedAt, ok := freeCodexFirstUsedAt(current); ok {
			expireAt := firstUsedAt.Add(freeCodexAuthTTL)
			now := time.Now()
			if expireAt.After(now.Add(10*time.Millisecond)) && !scheduledAt.IsZero() && !expireAt.Equal(scheduledAt) {
				item := &freeAuthExpiryItem{
					authID:   authID,
					expireAt: expireAt,
				}
				heap.Push(&m.freeAuthExpiryHeap, item)
				if m.freeAuthExpiryIndex == nil {
					m.freeAuthExpiryIndex = make(map[string]*freeAuthExpiryItem)
				}
				m.freeAuthExpiryIndex[authID] = item
				m.mu.Unlock()
				m.signalFreeAuthExpiryWorker()
				return
			}
		}
		removed = current.Clone()
		delete(m.auths, authID)
	}
	m.mu.Unlock()
	if removed != nil {
		m.deleteRevokedAuth(context.Background(), removed, "free codex auth expired 1 hour after first use", "free-expiry")
	}
}

func (m *Manager) signalFreeAuthExpiryWorker() {
	if m == nil || m.freeAuthExpiryWake == nil {
		return
	}
	select {
	case m.freeAuthExpiryWake <- struct{}{}:
	default:
	}
}

func (m *Manager) nextFreeAuthExpiryDue() (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.freeAuthExpiryHeap) == 0 {
		return time.Time{}, false
	}
	item := m.freeAuthExpiryHeap[0]
	if item == nil {
		return time.Time{}, false
	}
	return item.expireAt, true
}

func (m *Manager) collectExpiredFreeAuths(now time.Time, limit int) []freeAuthExpiryItem {
	if m == nil || limit <= 0 {
		return nil
	}
	expired := make([]freeAuthExpiryItem, 0, limit)
	m.mu.Lock()
	for len(expired) < limit && len(m.freeAuthExpiryHeap) > 0 {
		item := m.freeAuthExpiryHeap[0]
		if item == nil {
			heap.Pop(&m.freeAuthExpiryHeap)
			continue
		}
		if item.expireAt.After(now) {
			break
		}
		heap.Pop(&m.freeAuthExpiryHeap)
		delete(m.freeAuthExpiryIndex, item.authID)
		expired = append(expired, freeAuthExpiryItem{
			authID:   item.authID,
			expireAt: item.expireAt,
		})
	}
	m.mu.Unlock()
	return expired
}

func (m *Manager) runFreeAuthExpiryWorker() {
	if m == nil {
		return
	}
	for {
		now := time.Now()
		expired := m.collectExpiredFreeAuths(now, freeAuthExpiryBatchSize)
		if len(expired) > 0 {
			for _, item := range expired {
				m.expireFreeAuth(item.authID, item.expireAt)
			}
			if len(expired) == freeAuthExpiryBatchSize {
				timer := time.NewTimer(freeAuthExpiryDrainDelay)
				select {
				case <-timer.C:
				case <-m.freeAuthExpiryWake:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
				}
				continue
			}
		}

		nextDue, ok := m.nextFreeAuthExpiryDue()
		if !ok {
			<-m.freeAuthExpiryWake
			continue
		}

		wait := time.Until(nextDue)
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-m.freeAuthExpiryWake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
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

func (m *Manager) closestRetryWaitForError(err error, providers []string, model string, attempt int) (time.Duration, bool) {
	if isStreamDisconnectedBeforeCompletionError(err) {
		if attempt >= streamDisconnectRetryLimit {
			return 0, false
		}
		wait := streamDisconnectRetryDelayFunc()
		if wait < 0 {
			wait = 0
		}
		return wait, true
	}
	if isCompactExecuteTimeoutError(err) {
		wait := sameAuthRetryDelayFunc()
		if wait < 0 {
			wait = 0
		}
		return wait, true
	}
	return m.closestCooldownWait(providers, model, attempt)
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
	// Pool exhausted errors are deterministic; waiting/retrying will not help and will
	// only pierce the pool with repeated pick attempts.
	if isPoolExhaustedPickError(err) {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isStreamDisconnectedBeforeCompletionError(err) {
		return 0, false
	}
	if isCompactExecuteTimeoutError(err) {
		return 0, false
	}
	if !shouldRetryAcrossAuths(err) {
		return 0, false
	}
	if !m.retryAllowed(attempt, providers) {
		return 0, false
	}
	wait, found := m.closestRetryWaitForError(err, providers, model, attempt)
	if found {
		if wait > maxWait {
			return 0, false
		}
		return wait, true
	}
	// No cooldown state found; for 429 errors, fall back to the Retry-After header.
	if status != http.StatusTooManyRequests {
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

// MarkResult records an execution result and notifies hooks.
func (m *Manager) recordResult(ctx context.Context, result Result) {
	if !result.Success && result.Error != nil && result.Error.StatusCode() == http.StatusTooManyRequests {
		m.publishImmediateQuotaCooldown(result.AuthID, result.Provider, result.RetryAfter, time.Now())
	}
	m.MarkResult(ctx, result)
}

func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	removedAuth := (*Auth)(nil)
	deleteWarning := ""
	var authSnapshot *Auth
	var persistAuth *Auth

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		applySlowRequestPenaltyResult(auth, result, now, m.CurrentConfig())
		if result.Success {
			recordAuthLastUsedAt(auth, now)
			if strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
				setCodexUsageNextRefresh(auth, now.Add(codexUsageRefreshDelay))
			}
		}
		if removedAuth == nil {
			if deleteAuth, reason := shouldDeleteRevokedAuth(auth, result.Error); deleteAuth {
				removedAuth = auth.Clone()
				deleteWarning = reason
				m.stopFreeAuthExpiryTimerLocked(result.AuthID)
				m.unbindExecutionSessionsForAuthLocked(result.AuthID)
				delete(m.auths, result.AuthID)
			} else if deleteAuth, reason := shouldDeleteFreeCodexAuth(auth, result.Error, m.CurrentConfig()); deleteAuth {
				removedAuth = auth.Clone()
				deleteWarning = reason
				m.stopFreeAuthExpiryTimerLocked(result.AuthID)
				m.unbindExecutionSessionsForAuthLocked(result.AuthID)
				delete(m.auths, result.AuthID)
			}
		}

		if removedAuth != nil {
			// Auth already removed from runtime state.
		} else if result.Success {
			if result.Model != "" {
				state := ensureModelState(auth, result.Model)
				resetModelState(state, now)
				updateAggregatedAvailability(auth, now)
				syncCooldownMetadata(auth, now)
				if !hasModelError(auth, now) {
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
				auth.UpdatedAt = now
				shouldResumeModel = true
				clearModelQuota = true
			} else {
				clearAuthStateOnSuccess(auth, now)
				syncCooldownMetadata(auth, now)
			}
		} else {
			if result.Model != "" {
				if !isRequestScopedNotFoundResultError(result.Error) {
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

					statusCode := statusCodeFromResult(result.Error)
					if isModelSupportResultError(result.Error) {
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "model_not_supported"
						shouldSuspendModel = true
					} else {
					switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
								state.NextRetryAfter = next
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								next := now.Add(30 * time.Minute)
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
							if result.RetryAfter != nil {
								next = now.Add(*result.RetryAfter)
							} else if isGenericCodexRateLimitError(auth, result.Error) {
								next = now.Add(codexRateLimitCooldown)
							} else {
								cooldown, nextLevel := nextQuotaCooldown(backoffLevel, quotaCooldownDisabledForAuth(auth))
								if cooldown > 0 {
									next = now.Add(cooldown)
								}
								backoffLevel = nextLevel
							}
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								state.NextRetryAfter = next
							}
							if isGenericCodexRateLimitError(auth, result.Error) {
								state.Quota = QuotaState{}
								if !disableCooling {
									extendTransientQuotaCooldown(auth, next)
									auth.NextRetryAfter = next
								}
								bumpQuotaPriorityPenalty(auth)
							} else {
								state.Quota = QuotaState{
									Exceeded:      true,
									Reason:        "quota",
									NextRecoverAt: next,
									BackoffLevel:  backoffLevel,
								}
								if !disableCooling {
									suspendReason = "quota"
									shouldSuspendModel = true
									setModelQuota = true
								}
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
					} // end else (non-model-support errors)
					}

				auth.Status = StatusError
				auth.UpdatedAt = now
				updateAggregatedAvailability(auth, now)
				syncCooldownMetadata(auth, now)
				if shouldAutoDisableCodexAuthOnQuotaExceeded(auth, result.Error, m.CurrentConfig()) {
					applyAutoDisabledQuotaExceededState(auth, result.Error, result.RetryAfter, now)
				}
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
				syncCooldownMetadata(auth, now)
				if shouldAutoDisableCodexAuthOnQuotaExceeded(auth, result.Error, m.CurrentConfig()) {
					applyAutoDisabledQuotaExceededState(auth, result.Error, result.RetryAfter, now)
				}
			}
		}

		if removedAuth == nil {
			authSnapshot = auth.Clone()
			persistAuth = authSnapshot.Clone()
		}
	}
	m.mu.Unlock()
	if m.scheduler != nil && authSnapshot != nil {
		m.scheduler.upsertAuth(authSnapshot)
	}

	if removedAuth != nil {
		m.deleteRevokedAuth(ctx, removedAuth, deleteWarning, "request")
		m.hook.OnResult(ctx, result)
		return
	}

	if clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if setModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}
	if persistAuth != nil {
		_ = m.persist(ctx, persistAuth)
	}

	m.hook.OnResult(ctx, result)
}

func applySlowRequestPenaltyResult(auth *Auth, result Result, now time.Time, cfg *internalconfig.Config) {
	if auth == nil {
		return
	}
	auth.LastObservedLatency = result.Latency
	if cfg == nil || !cfg.Routing.SlowPenaltyEnabled() {
		if result.Success {
			relaxSlowRequestPriorityPenalty(auth, slowRequestPenaltyRecoverStep(cfg))
			auth.SlowRequestWindowCount = 0
			auth.SlowRequestWindowStartedAt = time.Time{}
			if !auth.SlowRequestCooldownUntil.IsZero() && !auth.SlowRequestCooldownUntil.After(now) {
				auth.SlowRequestCooldownUntil = time.Time{}
			}
		}
		return
	}

	threshold := slowRequestThreshold(cfg)
	isSlow := result.Latency > 0 && result.Latency >= threshold
	if !isSlow {
		if result.Success {
			relaxSlowRequestPriorityPenalty(auth, slowRequestPenaltyRecoverStep(cfg))
			auth.SlowRequestWindowCount = 0
			auth.SlowRequestWindowStartedAt = time.Time{}
			if auth.SlowRequestCooldownUntil.After(now) {
				auth.SlowRequestCooldownUntil = now
			}
		}
		if !auth.SlowRequestCooldownUntil.IsZero() && !auth.SlowRequestCooldownUntil.After(now) {
			auth.SlowRequestCooldownUntil = time.Time{}
		}
		return
	}

	window := slowRequestWindow(cfg)
	if auth.SlowRequestWindowStartedAt.IsZero() || now.Sub(auth.SlowRequestWindowStartedAt) > window {
		auth.SlowRequestWindowStartedAt = now
		auth.SlowRequestWindowCount = 0
	}
	auth.SlowRequestWindowCount++
	if auth.SlowRequestWindowCount < slowRequestTriggerCount(cfg) {
		return
	}

	bumpSlowRequestPriorityPenalty(auth, slowRequestPenaltyStep(cfg), slowRequestPenaltyMax(cfg))
	auth.SlowRequestWindowCount = 0
	auth.SlowRequestWindowStartedAt = now
	if cooldown := slowRequestCooldown(cfg); cooldown > 0 {
		until := now.Add(cooldown)
		if until.After(auth.SlowRequestCooldownUntil) {
			auth.SlowRequestCooldownUntil = until
		}
	}
}

func slowRequestThreshold(cfg *internalconfig.Config) time.Duration {
	if cfg != nil && cfg.Routing.SlowRequestThresholdMs > 0 {
		return time.Duration(cfg.Routing.SlowRequestThresholdMs) * time.Millisecond
	}
	return time.Duration(slowRequestThresholdDefaultMs) * time.Millisecond
}

func slowRequestWindow(cfg *internalconfig.Config) time.Duration {
	if cfg != nil && cfg.Routing.SlowRequestWindowSeconds > 0 {
		return time.Duration(cfg.Routing.SlowRequestWindowSeconds) * time.Second
	}
	return slowRequestWindowDefault
}

func slowRequestTriggerCount(cfg *internalconfig.Config) int {
	if cfg != nil && cfg.Routing.SlowRequestTriggerCount > 0 {
		return cfg.Routing.SlowRequestTriggerCount
	}
	return slowRequestTriggerDefault
}

func slowRequestPenaltyStep(cfg *internalconfig.Config) int {
	if cfg != nil && cfg.Routing.SlowRequestPenaltyStep > 0 {
		return cfg.Routing.SlowRequestPenaltyStep
	}
	return slowRequestPenaltyStepDefault
}

func slowRequestPenaltyMax(cfg *internalconfig.Config) int {
	if cfg != nil && cfg.Routing.SlowRequestPenaltyMax > 0 {
		return cfg.Routing.SlowRequestPenaltyMax
	}
	return slowRequestPenaltyMaxDefault
}

func slowRequestPenaltyRecoverStep(cfg *internalconfig.Config) int {
	if cfg != nil && cfg.Routing.SlowRequestPenaltyRecover > 0 {
		return cfg.Routing.SlowRequestPenaltyRecover
	}
	return slowRequestPenaltyRecover
}

func slowRequestCooldown(cfg *internalconfig.Config) time.Duration {
	if cfg != nil {
		if cfg.Routing.SlowRequestCooldownSeconds <= 0 {
			return 0
		}
		return time.Duration(cfg.Routing.SlowRequestCooldownSeconds) * time.Second
	}
	return slowRequestCooldownDefault
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
	if len(auth.ModelStates) == 0 {
		clearAggregatedAvailability(auth)
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	hasState := false
	for _, state := range auth.ModelStates {
		if state == nil {
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
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !hasState {
		clearAggregatedAvailability(auth)
		return
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func syncCooldownMetadata(auth *Auth, now time.Time) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	if auth.Quota.Exceeded && !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
		auth.Metadata[metadataCooldownUntilKey] = auth.Quota.NextRecoverAt.Unix()
		auth.Metadata[metadataCooldownReasonKey] = cooldownReasonQuota
		return
	}
	delete(auth.Metadata, metadataCooldownUntilKey)
	delete(auth.Metadata, metadataCooldownReasonKey)
}

func clearAggregatedAvailability(auth *Auth) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.NextRetryAfter = time.Time{}
	auth.Quota = QuotaState{}
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

func resetAllModelStates(auth *Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	for _, state := range auth.ModelStates {
		resetModelState(state, now)
	}
}

func applyQuotaProbeSuccessState(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	autoDisabled := quotaProbeAutoDisabled(auth)
	manualDisabled := auth.Disabled && !autoDisabled
	previousStatusMessage := auth.StatusMessage
	clearAuthStateOnSuccess(auth, now)
	resetAllModelStates(auth, now)
	auth.TransientCooldownUntil = time.Time{}
	relaxQuotaPriorityPenalty(auth)
	if autoDisabled {
		auth.Disabled = false
		auth.Status = StatusActive
		auth.StatusMessage = ""
		clearQuotaProbeAutoDisabled(auth)
	} else if manualDisabled {
		auth.Status = StatusDisabled
		auth.Disabled = true
		auth.StatusMessage = previousStatusMessage
	}
	syncCooldownMetadata(auth, now)
}

func applyQuotaProbeQuotaExceededState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	manualDisabled := auth.Disabled && !quotaProbeAutoDisabled(auth)
	applyAuthFailureState(auth, resultErr, retryAfter, now)
	syncCooldownMetadata(auth, now)
	if manualDisabled {
		auth.Status = StatusDisabled
		return
	}
	auth.Disabled = true
	auth.Status = StatusDisabled
	setQuotaProbeAutoDisabled(auth)
}

func applyAutoDisabledQuotaExceededState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	manualDisabled := auth.Disabled && !quotaProbeAutoDisabled(auth)
	applyAuthFailureState(auth, resultErr, retryAfter, now)
	syncCooldownMetadata(auth, now)
	if manualDisabled {
		auth.Status = StatusDisabled
		return
	}
	auth.Disabled = true
	auth.Status = StatusDisabled
	setQuotaProbeAutoDisabled(auth)
}

func shouldDeleteAuthOnQuotaProbe(auth *Auth, resultErr *Error, cfg *internalconfig.Config) (bool, string) {
	if deleteAuth, reason := shouldDeleteRevokedAuth(auth, resultErr); deleteAuth {
		return true, reason
	}
	if deleteAuth, reason := shouldDeleteFreeCodexAuth(auth, resultErr, cfg); deleteAuth {
		return true, reason
	}
	if auth == nil || resultErr == nil {
		return false, ""
	}
	if resultErr.StatusCode() != http.StatusUnauthorized {
		return false, ""
	}
	if !hasPersistedAuthRecord(auth) {
		return false, ""
	}
	reason := strings.TrimSpace(resultErr.Message)
	if reason == "" {
		reason = "quota refresh returned unauthorized"
	}
	return true, reason
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

func modelNotFoundFromError(err *Error, fallbackModel string) (string, bool) {
	if err == nil || err.HTTPStatus != http.StatusNotFound {
		return "", false
	}
	message := strings.TrimSpace(err.Message)
	if message == "" {
		return "", false
	}
	match := modelNotFoundPattern.FindStringSubmatch(message)
	if len(match) >= 2 {
		if model := canonicalModelKey(match[1]); model != "" {
			return model, true
		}
	}
	fallback := canonicalModelKey(fallbackModel)
	if fallback == "" {
		return "", false
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "model not found") {
		return fallback, true
	}
	return "", false
}

func mergeExcludedModels(auth *Auth, model string) bool {
	if auth == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	if model == "" {
		return false
	}
	seen := make(map[string]struct{})
	merged := make([]string, 0, 4)
	appendList := func(raw string) {
		for _, part := range strings.Split(raw, ",") {
			item := strings.ToLower(strings.TrimSpace(part))
			if item == "" {
				continue
			}
			if _, ok := seen[item]; ok {
				continue
			}
			seen[item] = struct{}{}
			merged = append(merged, item)
		}
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	appendList(auth.Attributes["excluded_models"])
	if metaRaw, ok := auth.Metadata["excluded_models"]; ok {
		switch typed := metaRaw.(type) {
		case string:
			appendList(typed)
		case []string:
			appendList(strings.Join(typed, ","))
		case []any:
			parts := make([]string, 0, len(typed))
			for _, item := range typed {
				if str, ok := item.(string); ok {
					parts = append(parts, str)
				}
			}
			appendList(strings.Join(parts, ","))
		}
	}
	if _, exists := seen[model]; exists {
		return false
	}
	seen[model] = struct{}{}
	merged = append(merged, model)
	sort.Strings(merged)
	auth.Attributes["excluded_models"] = strings.Join(merged, ",")
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["excluded_models"] = append([]string(nil), merged...)
	return true
}

func trimLogMessage(message string, limit int) string {
	message = strings.TrimSpace(message)
	if limit > 0 && len(message) > limit {
		return message[:limit]
	}
	return message
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
	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	copied := *retryAfter
	return &copied
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
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
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isModelSupportResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
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

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it treats 400 responses with
// "invalid_request_error", request-scoped 404 item misses caused by `store=false`,
// and all 422 responses as request-shape failures, where switching auths or
// pooled upstream models will not help. Model-support errors are excluded so
// routing can fall through to another auth or upstream.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if isModelSupportError(err) {
		return false
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest:
		message := strings.ToLower(strings.TrimSpace(err.Error()))
		if message == "" {
			return false
		}
		return strings.Contains(message, "invalid_request_error") ||
			strings.Contains(message, "unsupported parameter") ||
			strings.Contains(message, "unknown parameter")
	case http.StatusNotFound:
		// Upstream sometimes returns a 404 for invalid request payloads, especially
		// when clients feed previous /responses output back into input (rs_... items)
		// while store=false. Retrying across auths only pierces the pool.
		if isRequestScopedNotFoundMessage(err.Error()) {
			return true
		}
		message := strings.ToLower(strings.TrimSpace(err.Error()))
		if message == "" {
			return false
		}
		if strings.Contains(message, "items are not persisted when `store` is set to false") {
			return true
		}
		if strings.Contains(message, "item with id") && strings.Contains(message, "not found") && strings.Contains(message, "rs_") {
			return true
		}
		return false
	case http.StatusUnprocessableEntity:
		// All 422 responses represent request-shape failures, so retries with other
		// auths or upstream model pool entries will not help.
		return true
	default:
		return false
	}
}

// shouldRetryAcrossAuths returns true only for auth/model specific restriction
// failures where switching credentials has a reasonable chance to succeed.
// Transport/bootstrap timeouts are deliberately excluded because they are usually
// infrastructure or proxy-pool failures; rotating the auth pool only amplifies latency.
func shouldRetryAcrossAuths(err error) bool {
	if err == nil || isRequestInvalidError(err) {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		// modelCooldownError is a pool-wide signal ("all creds cooling down"), so
		// switching credentials or waiting inside the same request won't help.
		return false
	}
	switch statusCodeFromError(err) {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return !isStreamSetupTimeoutError(err)
	default:
		return isStreamDisconnectedBeforeCompletionError(err)
	}
}

func shouldCountRetryCredentialBudget(err error) bool {
	if err == nil {
		return true
	}
	switch statusCodeFromError(err) {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return true
	}
	for _, needle := range []string{
		"unauthorized",
		"forbidden",
		"authorization lost",
		"token_revoked",
		"token_invalidated",
		"account_deactivated",
		"must be a member of an organization to use the api",
		"deactivated_workspace",
		"your authentication token has been invalidated. please try signing in again.",
		"your openai account has been deactivated",
		"account has been deactivated",
		"usage limit",
		"rate limit",
		"rate limited",
		"too many requests",
		"selected model is at capacity",
	} {
		if strings.Contains(message, needle) {
			return false
		}
	}
	return true
}

func shouldDeleteRevokedAuth(auth *Auth, resultErr *Error) (bool, string) {
	if auth == nil || resultErr == nil {
		return false, ""
	}
	if !hasPersistedAuthRecord(auth) {
		return false, ""
	}
	msg := strings.ToLower(strings.TrimSpace(resultErr.Message))
	if msg == "" {
		return false, ""
	}
	if strings.Contains(msg, "deactivated_workspace") {
		return true, strings.TrimSpace(resultErr.Message)
	}
	if resultErr.StatusCode() != http.StatusUnauthorized {
		return false, ""
	}
	for _, needle := range []string{
		"token_revoked",
		"token_invalidated",
		"account_deactivated",
		"must be a member of an organization to use the api",
		"encountered invalidated oauth token for user",
		"your authentication token has been invalidated. please try signing in again.",
		"your openai account has been deactivated",
		"account has been deactivated",
	} {
		if strings.Contains(msg, needle) {
			return true, strings.TrimSpace(resultErr.Message)
		}
	}
	return false, ""
}

func hasRefreshTokenMetadata(auth *Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
		return true
	}
	if v, ok := auth.Metadata["refreshToken"].(string); ok && strings.TrimSpace(v) != "" {
		return true
	}
	raw, ok := auth.Metadata["token"]
	if !ok || raw == nil {
		return false
	}
	switch typed := raw.(type) {
	case map[string]any:
		if v, ok := typed["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			return true
		}
		if v, ok := typed["refreshToken"].(string); ok && strings.TrimSpace(v) != "" {
			return true
		}
	case map[string]string:
		if v := strings.TrimSpace(typed["refresh_token"]); v != "" {
			return true
		}
		if v := strings.TrimSpace(typed["refreshToken"]); v != "" {
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

func (m *Manager) deleteRevokedAuth(_ context.Context, auth *Auth, warning string, source string) {
	if auth == nil {
		return
	}
	m.clearQuotaRefreshPending(auth.ID)
	m.unbindExecutionSessionsForAuth(auth.ID)
	m.mu.Lock()
	m.stopFreeAuthExpiryTimerLocked(auth.ID)
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.removeAuth(auth.ID)
	}
	m.removeAPIKeyModelAlias(auth.ID)
	registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	m.enqueueRevokedDelete(auth.Clone(), warning, source)
}

func (m *Manager) enqueueRevokedDelete(auth *Auth, warning string, source string) {
	if m == nil || auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	task := revokedDeleteTask{auth: auth.Clone(), warning: warning, source: source}
	m.mu.Lock()
	if m.revokedDeletePending == nil {
		m.revokedDeletePending = make(map[string]struct{})
	}
	if _, exists := m.revokedDeletePending[authID]; exists {
		m.mu.Unlock()
		return
	}
	m.revokedDeletePending[authID] = struct{}{}
	m.mu.Unlock()
	m.revokedDeleteQueue <- task
}

func (m *Manager) runRevokedDeleteWorker() {
	if m == nil {
		return
	}
	for task := range m.revokedDeleteQueue {
		m.processRevokedDeleteTask(task)
	}
}

func revokedDeleteRetryDelay(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 0
	case 1:
		return time.Second
	case 2:
		return 3 * time.Second
	default:
		return 10 * time.Second
	}
}

func (m *Manager) processRevokedDeleteTask(task revokedDeleteTask) {
	if m == nil || task.auth == nil {
		return
	}
	authID := strings.TrimSpace(task.auth.ID)
	if authID == "" {
		return
	}
	defer func() {
		m.mu.Lock()
		delete(m.revokedDeletePending, authID)
		m.mu.Unlock()
	}()

	source := strings.TrimSpace(task.source)
	if source == "" {
		source = "request"
	}

	var deleteErr error
	for attempt := 0; attempt < 3; attempt++ {
		if wait := revokedDeleteRetryDelay(attempt); wait > 0 {
			time.Sleep(wait)
		}
		m.mu.RLock()
		store := m.store
		m.mu.RUnlock()
		if store == nil {
			deleteErr = nil
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), revokedDeleteTimeout)
		deleteErr = store.Delete(ctx, authID)
		cancel()
		if deleteErr == nil {
			break
		}
	}
	if deleteErr != nil {
		log.Warnf("removed revoked auth %s%s from runtime after invalid token error during %s, but failed to delete from store: %v", authID, authPathSuffix(task.auth), source, deleteErr)
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "request"
	}
	log.Warnf("deleted revoked auth %s%s after invalid token error during %s: %s", authID, authPathSuffix(task.auth), source, task.warning)
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
		if strings.TrimSpace(auth.StatusMessage) == "" {
			auth.StatusMessage = "quota exhausted"
		}
		var next time.Time
		if retryAfter != nil {
			next = now.Add(*retryAfter)
		} else if isGenericCodexRateLimitError(auth, resultErr) {
			next = now.Add(codexRateLimitCooldown)
		} else {
			cooldown, _ := nextQuotaCooldown(auth.Quota.BackoffLevel, quotaCooldownDisabledForAuth(auth))
			if cooldown > 0 {
				next = now.Add(cooldown)
			}
		}
		auth.NextRetryAfter = next
		extendTransientQuotaCooldown(auth, next)
		bumpQuotaPriorityPenalty(auth)
		if isGenericCodexRateLimitError(auth, resultErr) {
			auth.Quota.Exceeded = false
			auth.Quota.Reason = ""
			auth.Quota.NextRecoverAt = time.Time{}
			auth.Quota.BackoffLevel = 0
		} else {
			auth.Quota.Exceeded = true
			auth.Quota.Reason = "quota"
			auth.Quota.NextRecoverAt = next
			scheduleQuotaProbeAfter(auth, next)
		}
	case 408, 500, 502, 503, 504:
		if strings.TrimSpace(auth.StatusMessage) == "" {
			auth.StatusMessage = "transient upstream error"
		}
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

func bumpQuotaPriorityPenalty(auth *Auth) {
	if auth == nil {
		return
	}
	auth.QuotaPriorityPenalty += quotaPriorityPenaltyStep
	if auth.QuotaPriorityPenalty > quotaPriorityPenaltyMax {
		auth.QuotaPriorityPenalty = quotaPriorityPenaltyMax
	}
}

func bumpSlowRequestPriorityPenalty(auth *Auth, step int, max int) {
	if auth == nil {
		return
	}
	if step <= 0 {
		step = slowRequestPenaltyStepDefault
	}
	if max <= 0 {
		max = slowRequestPenaltyMaxDefault
	}
	auth.SlowRequestPriorityPenalty += step
	if auth.SlowRequestPriorityPenalty > max {
		auth.SlowRequestPriorityPenalty = max
	}
}

func relaxQuotaPriorityPenalty(auth *Auth) {
	if auth == nil || auth.QuotaPriorityPenalty <= 0 {
		return
	}
	auth.QuotaPriorityPenalty -= quotaPriorityPenaltyRecover
	if auth.QuotaPriorityPenalty < 0 {
		auth.QuotaPriorityPenalty = 0
	}
}

func relaxSlowRequestPriorityPenalty(auth *Auth, step int) {
	if auth == nil || auth.SlowRequestPriorityPenalty <= 0 {
		return
	}
	if step <= 0 {
		step = slowRequestPenaltyRecover
	}
	auth.SlowRequestPriorityPenalty -= step
	if auth.SlowRequestPriorityPenalty < 0 {
		auth.SlowRequestPriorityPenalty = 0
	}
}

func scheduleQuotaProbeAfter(auth *Auth, next time.Time) {
	if auth == nil || next.IsZero() {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataQuotaProbeAfterKey] = next.Unix()
}

func extendTransientQuotaCooldown(auth *Auth, until time.Time) {
	if auth == nil || until.IsZero() {
		return
	}
	if auth.TransientCooldownUntil.IsZero() || auth.TransientCooldownUntil.Before(until) {
		auth.TransientCooldownUntil = until
	}
}

func (m *Manager) publishImmediateQuotaCooldown(authID string, provider string, retryAfter *time.Duration, now time.Time) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return
	}
	var snapshot *Auth
	m.mu.Lock()
	if auth := m.auths[authID]; auth != nil {
		until := time.Time{}
		if retryAfter != nil && *retryAfter > 0 {
			until = now.Add(*retryAfter)
		} else {
			cooldown, _ := nextQuotaCooldown(auth.Quota.BackoffLevel, quotaCooldownDisabledForAuth(auth))
			if cooldown > 0 {
				until = now.Add(cooldown)
			}
		}
		if auth.Quota.NextRecoverAt.After(until) {
			until = auth.Quota.NextRecoverAt
		}
		extendTransientQuotaCooldown(auth, until)
		bumpQuotaPriorityPenalty(auth)
		if !until.IsZero() {
			scheduleQuotaProbeAfter(auth, until)
		}
		snapshot = auth.Clone()
	}
	m.mu.Unlock()
	if snapshot != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

func (m *Manager) preparePickedAuth(auth *Auth, provider, model string) (*Auth, error) {
	if m == nil || auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	now := time.Now()
	m.mu.RLock()
	current := m.auths[auth.ID]
	var snapshot *Auth
	if current != nil {
		snapshot = current.Clone()
	}
	m.mu.RUnlock()
	if snapshot == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if !freeCodexAutoDeleteDisabled(m.CurrentConfig()) {
		if _, expired := freeCodexAuthExpired(snapshot, now); expired {
			var removed *Auth
			m.mu.Lock()
			current = m.auths[auth.ID]
			if current != nil {
				if _, currentExpired := freeCodexAuthExpired(current, now); currentExpired {
					removed = current.Clone()
					m.stopFreeAuthExpiryTimerLocked(auth.ID)
					m.unbindExecutionSessionsForAuthLocked(auth.ID)
					delete(m.auths, auth.ID)
				}
			}
			m.mu.Unlock()
			if removed != nil {
				if m.scheduler != nil {
					m.scheduler.removeAuth(removed.ID)
				}
				m.deleteRevokedAuth(context.Background(), removed, "free codex auth expired 1 hour after first use", "free-expiry")
			}
			return nil, &Error{Code: "auth_unavailable", Message: "auth unavailable"}
		}
	}
	checkModel := m.selectionModelForAuth(snapshot, model)
	blocked, reason, next := isAuthBlockedForModel(snapshot, checkModel, now)
	if !blocked {
		return snapshot, nil
	}
	switch reason {
	case blockReasonCooldown:
		resetIn := next.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		providerForError := strings.TrimSpace(provider)
		if providerForError == "" {
			providerForError = strings.TrimSpace(snapshot.Provider)
		}
		return nil, newModelCooldownError(model, providerForError, resetIn)
	case blockReasonDisabled:
		return nil, &Error{Code: "auth_unavailable", Message: "auth disabled"}
	default:
		return nil, &Error{Code: "auth_unavailable", Message: "auth unavailable"}
	}
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
	m.unbindExecutionSession(sessionID)

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
	skipModelRegistryCheck := metadataBool(opts.Metadata, cliproxyexecutor.SkipModelRegistryCheckMetadataKey)

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
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if !skipModelRegistryCheck && modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
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
	selected, errPick := m.selector.Pick(ctx, provider, selectionArgForSelector(m.selector, model), opts, available)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, errPick
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
	if strings.TrimSpace(model) != "" {
		m.mu.RLock()
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Provider != provider || candidate.Disabled {
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
	skipModelRegistryCheck := metadataBool(opts.Metadata, cliproxyexecutor.SkipModelRegistryCheckMetadataKey)

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
		if !skipModelRegistryCheck && modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
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
	selected, errPick := m.selector.Pick(ctx, "mixed", selectionArgForSelector(m.selector, model), opts, available)
	if errPick != nil {
		m.mu.RUnlock()
		return nil, nil, "", errPick
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
	if strings.TrimSpace(model) != "" {
		providerSet := make(map[string]struct{}, len(eligibleProviders))
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
	persistAuth := sanitizeAuthForPersist(auth)
	if persistAuth == nil || persistAuth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, persistAuth)
	return err
}

func sanitizeAuthForPersist(auth *Auth) *Auth {
	if auth == nil {
		return nil
	}
	sanitized := auth.Clone()
	if sanitized.Metadata == nil {
		return sanitized
	}
	delete(sanitized.Metadata, metadataCooldownUntilKey)
	delete(sanitized.Metadata, metadataCooldownReasonKey)
	delete(sanitized.Metadata, metadataQuotaProbeLastKey)
	delete(sanitized.Metadata, metadataQuotaProbeAfterKey)
	return sanitized
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

// StartAutoQuotaRefresh launches a background loop that probes account quota state
// in fixed-size batches. It intentionally includes disabled auths so accounts that
// were auto-disabled on quota exhaustion can recover automatically.
func (m *Manager) StartAutoQuotaRefresh(parent context.Context) {
	if m == nil {
		return
	}
	if m.quotaRefreshCancel != nil {
		m.quotaRefreshCancel()
		m.quotaRefreshCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.quotaRefreshCancel = cancel
	go func() {
		ticker := time.NewTicker(quotaProbeBatchInterval)
		defer ticker.Stop()
		m.checkQuotaRefreshes(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkQuotaRefreshes(ctx)
			}
		}
	}()
}

// StopAutoQuotaRefresh cancels the background quota refresh loop, if running.
func (m *Manager) StopAutoQuotaRefresh() {
	if m == nil {
		return
	}
	if m.quotaRefreshCancel != nil {
		m.quotaRefreshCancel()
		m.quotaRefreshCancel = nil
	}
}

func (m *Manager) checkQuotaRefreshes(ctx context.Context) {
	now := time.Now()
	batch := m.pickQuotaRefreshBatch(now, quotaProbeBatchSize)
	for _, authID := range batch {
		go m.refreshQuotaAuthWithLimit(ctx, authID)
	}
}

func (m *Manager) refreshQuotaAuthWithLimit(ctx context.Context, id string) {
	if m.quotaRefreshSemaphore == nil {
		m.refreshQuotaAuth(ctx, id)
		return
	}
	select {
	case m.quotaRefreshSemaphore <- struct{}{}:
		defer func() { <-m.quotaRefreshSemaphore }()
	case <-ctx.Done():
		return
	}
	m.refreshQuotaAuth(ctx, id)
}

func (m *Manager) pickQuotaRefreshBatch(now time.Time, limit int) []string {
	if m == nil || limit <= 0 {
		return nil
	}
	snapshot := m.snapshotAuths()
	cfg := m.CurrentConfig()
	type quotaCandidate struct {
		auth      *Auth
		due       time.Time
		last      time.Time
		idleProbe bool
		idleSince time.Duration
	}
	candidates := make([]quotaCandidate, 0, len(snapshot))
	for _, auth := range snapshot {
		if auth == nil {
			continue
		}
		if m.executorFor(auth.Provider) == nil {
			continue
		}
		if auth.Disabled && !quotaProbeAutoDisabled(auth) {
			continue
		}
		due, last := combinedQuotaRefreshSchedule(auth)
		idleProbe := shouldProbeIdleAuth(cfg, auth, now)
		if !idleProbe && !due.IsZero() && due.After(now) {
			continue
		}
		candidates = append(candidates, quotaCandidate{
			auth:      auth,
			due:       due,
			last:      last,
			idleProbe: idleProbe,
			idleSince: authIdleSince(auth, now),
		})
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.idleProbe != right.idleProbe {
			return left.idleProbe
		}
		if left.idleProbe && right.idleProbe && left.idleSince != right.idleSince {
			return left.idleSince > right.idleSince
		}
		leftAutoDisabled := quotaProbeAutoDisabled(left.auth)
		rightAutoDisabled := quotaProbeAutoDisabled(right.auth)
		if leftAutoDisabled != rightAutoDisabled {
			return leftAutoDisabled
		}
		if left.auth.Disabled != right.auth.Disabled {
			return left.auth.Disabled
		}
		if left.last.IsZero() != right.last.IsZero() {
			return left.last.IsZero()
		}
		if !left.due.Equal(right.due) {
			if left.due.IsZero() {
				return true
			}
			if right.due.IsZero() {
				return false
			}
			return left.due.Before(right.due)
		}
		if !left.last.Equal(right.last) {
			if left.last.IsZero() {
				return true
			}
			if right.last.IsZero() {
				return false
			}
			return left.last.Before(right.last)
		}
		return left.auth.ID < right.auth.ID
	})
	selected := make([]string, 0, limit)
	for _, candidate := range candidates {
		if len(selected) >= limit {
			break
		}
		if m.markQuotaRefreshPending(candidate.auth.ID, now, candidate.idleProbe) {
			selected = append(selected, candidate.auth.ID)
		}
	}
	return selected
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
	if a == nil || a.Disabled {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
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

func quotaProbeSchedule(auth *Auth) (time.Time, time.Time) {
	if auth == nil {
		return time.Time{}, time.Time{}
	}
	var next time.Time
	var last time.Time
	if auth.Metadata != nil {
		next, _ = lookupMetadataTime(auth.Metadata, metadataQuotaProbeAfterKey)
		last, _ = lookupMetadataTime(auth.Metadata, metadataQuotaProbeLastKey)
	}
	if next.IsZero() {
		if auth.Quota.Exceeded && !auth.Quota.NextRecoverAt.IsZero() {
			next = auth.Quota.NextRecoverAt
		} else if !auth.NextRetryAfter.IsZero() {
			next = auth.NextRetryAfter
		}
	}
	return next, last
}

func combinedQuotaRefreshSchedule(auth *Auth) (time.Time, time.Time) {
	probeDue, probeLast := quotaProbeSchedule(auth)
	usageDue, usageLast := codexUsageRefreshSchedule(auth)
	due := earliestNonZeroTime(usageDue, probeDue)
	switch {
	case !usageDue.IsZero() && (probeDue.IsZero() || !probeDue.Before(usageDue)):
		return due, usageLast
	case !probeDue.IsZero():
		return due, probeLast
	case !usageLast.IsZero():
		return due, usageLast
	default:
		return due, probeLast
	}
}

func authIdleProbeAfter(cfg *internalconfig.Config) time.Duration {
	if cfg == nil || cfg.Routing.IdleProbeAfterHours <= 0 {
		return 0
	}
	return time.Duration(cfg.Routing.IdleProbeAfterHours) * time.Hour
}

func authLastUsedAt(auth *Auth) (time.Time, bool) {
	if auth == nil || auth.Metadata == nil {
		return time.Time{}, false
	}
	return lookupMetadataTime(auth.Metadata, metadataLastUsedAtKey)
}

func authIdleSince(auth *Auth, now time.Time) time.Duration {
	if auth == nil {
		return 0
	}
	if ts, ok := authLastUsedAt(auth); ok && !ts.IsZero() {
		return now.Sub(ts)
	}
	if !auth.CreatedAt.IsZero() {
		return now.Sub(auth.CreatedAt)
	}
	return 0
}

func shouldProbeIdleAuth(cfg *internalconfig.Config, auth *Auth, now time.Time) bool {
	threshold := authIdleProbeAfter(cfg)
	if threshold <= 0 {
		return false
	}
	if auth == nil {
		return false
	}
	if quotaProbeAutoDisabled(auth) {
		return false
	}
	if auth.Disabled {
		return false
	}
	return authIdleSince(auth, now) >= threshold
}

func quotaProbeAutoDisabled(auth *Auth) bool {
	switch strings.ToLower(strings.TrimSpace(quotaAutoDisabledReason(auth))) {
	case autoDisabledReasonQuotaExhausted, autoDisabledReasonQuotaLowBalance:
		return true
	default:
		return false
	}
}

func quotaAutoDisabledReason(auth *Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	raw, ok := auth.Metadata[metadataAutoDisabledReasonKey]
	if !ok {
		return ""
	}
	reason, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(reason)
}

func setQuotaProbeCooldown(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataQuotaProbeLastKey] = now.Unix()
	auth.Metadata[metadataQuotaProbeAfterKey] = now.Add(quotaProbeCooldown).Unix()
}

func clearQuotaProbeAutoDisabled(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, metadataAutoDisabledReasonKey)
}

func setQuotaProbeAutoDisabled(auth *Auth) {
	setQuotaAutoDisabledReason(auth, autoDisabledReasonQuotaExhausted)
}

func setQuotaAutoDisabledReason(auth *Auth, reason string) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataAutoDisabledReasonKey] = strings.TrimSpace(reason)
}

func clearGenericQuotaProbeSchedule(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	delete(auth.Metadata, metadataQuotaProbeLastKey)
	delete(auth.Metadata, metadataQuotaProbeAfterKey)
}

func setCodexUsageFetchMetadata(auth *Auth, now time.Time, payload []byte, remaining float64, hasRemaining bool) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[metadataCodexUsageLastKey] = now.Unix()
	if len(payload) > 0 {
		auth.Metadata[metadataCodexUsagePayloadKey] = string(bytes.TrimSpace(payload))
	} else {
		delete(auth.Metadata, metadataCodexUsagePayloadKey)
	}
	if hasRemaining {
		auth.Metadata[metadataCodexUsageRemainingKey] = remaining
	} else {
		delete(auth.Metadata, metadataCodexUsageRemainingKey)
	}
}

func setCodexUsageNextRefresh(auth *Auth, next time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if next.IsZero() {
		delete(auth.Metadata, metadataCodexUsageAfterKey)
		return
	}
	if currentNext, ok := lookupMetadataTime(auth.Metadata, metadataCodexUsageAfterKey); ok && !currentNext.IsZero() {
		lastFetch, _ := lookupMetadataTime(auth.Metadata, metadataCodexUsageLastKey)
		if (lastFetch.IsZero() || lastFetch.Before(currentNext)) && currentNext.Before(next) {
			next = currentNext
		}
	}
	auth.Metadata[metadataCodexUsageAfterKey] = next.Unix()
}

func authUsedWithinWindow(auth *Auth, now time.Time, window time.Duration) bool {
	if window <= 0 {
		return false
	}
	lastUsed, ok := authLastUsedAt(auth)
	if !ok || lastUsed.IsZero() {
		return false
	}
	return now.Sub(lastUsed) < window
}

func (m *Manager) markQuotaRefreshPending(id string, now time.Time, allowBeforeDue bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[id]
	if !ok || auth == nil {
		return false
	}
	if pendingUntil, ok := m.quotaRefreshPending[id]; ok && pendingUntil.After(now) {
		return false
	}
	due, _ := combinedQuotaRefreshSchedule(auth)
	if !allowBeforeDue {
		if !due.IsZero() && due.After(now) {
			return false
		}
	}
	if quotaProbeAutoDisabled(auth) {
		if !due.IsZero() && due.After(now) {
			return false
		}
	}
	if allowBeforeDue && auth.Disabled && !quotaProbeAutoDisabled(auth) {
		return false
	}
	if m.quotaRefreshPending == nil {
		m.quotaRefreshPending = make(map[string]time.Time)
	}
	m.quotaRefreshPending[id] = now.Add(quotaProbeTimeout)
	return true
}

func (m *Manager) clearQuotaRefreshPending(id string) {
	if m == nil || id == "" {
		return
	}
	m.mu.Lock()
	delete(m.quotaRefreshPending, id)
	m.mu.Unlock()
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[id]
	if !ok || auth == nil || auth.Disabled {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	return true
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
		resultErr := &Error{Message: err.Error()}
		retryAfter := retryAfterFromError(err)
		if se, ok := errors.AsType[cliproxyexecutor.StatusError](err); ok && se != nil {
			resultErr.HTTPStatus = se.StatusCode()
		}
		if deleteAuth, reason := shouldDeleteRevokedAuth(auth, resultErr); deleteAuth {
			m.mu.Lock()
			current := m.auths[id]
			if current != nil {
				m.unbindExecutionSessionsForAuthLocked(id)
				delete(m.auths, id)
			}
			m.mu.Unlock()
			if current != nil {
				m.deleteRevokedAuth(ctx, current.Clone(), reason, "refresh")
			}
			return
		}
		var snapshot *Auth
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			applyAuthFailureState(current, resultErr, retryAfter, now)
			current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			m.auths[id] = current
			snapshot = current.Clone()
		}
		m.mu.Unlock()
		if snapshot != nil && m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
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
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	_, _ = m.Update(ctx, updated)
}

func (m *Manager) refreshQuotaAuth(ctx context.Context, id string) {
	m.refreshQuotaAuthInternal(ctx, id, false)
}

func (m *Manager) ForceRefreshQuotaAuth(ctx context.Context, id string) {
	m.refreshQuotaAuthInternal(ctx, id, true)
}

func (m *Manager) refreshQuotaAuthInternal(ctx context.Context, id string, force bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer m.clearQuotaRefreshPending(id)
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
	if force && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		m.refreshCodexUsageAuth(ctx, id, exec, auth.Clone())
		return
	}
	if due, _ := codexUsageRefreshSchedule(auth); !due.IsZero() && !due.After(time.Now()) {
		m.refreshCodexUsageAuth(ctx, id, exec, auth.Clone())
		return
	}

	probeModel := pickQuotaProbeModel(auth)
	err := m.executeQuotaProbe(ctx, exec, auth.Clone())
	if err != nil && errors.Is(err, context.Canceled) {
		if ctx.Err() != nil {
			return
		}
	}

	now := time.Now()
	if err != nil {
		if errors.Is(err, errQuotaProbeSkippedNoSupportedModel) {
			entry := log.WithFields(log.Fields{
				"auth_id":  id,
				"provider": auth.Provider,
			})
			if probeModel != "" {
				entry = entry.WithField("probe_model", probeModel)
			}
			entry.Debug("quota refresh skipped: no supported model for auth")
			var snapshot *Auth
			var persistAuth *Auth
			m.mu.Lock()
			if current := m.auths[id]; current != nil {
				setQuotaProbeCooldown(current, now)
				current.UpdatedAt = now
				m.auths[id] = current
				snapshot = current.Clone()
				persistAuth = snapshot.Clone()
			}
			m.mu.Unlock()
			if snapshot != nil && m.scheduler != nil {
				m.scheduler.upsertAuth(snapshot)
			}
			if persistAuth != nil {
				_ = m.persist(ctx, persistAuth)
			}
			return
		}

		resultErr := &Error{Message: err.Error()}
		retryAfter := retryAfterFromError(err)
		if se, ok := errors.AsType[cliproxyexecutor.StatusError](err); ok && se != nil {
			resultErr.HTTPStatus = se.StatusCode()
		}
		if deleteAuth, reason := shouldDeleteAuthOnQuotaProbe(auth, resultErr, m.CurrentConfig()); deleteAuth {
			m.mu.Lock()
			current := m.auths[id]
			if current != nil {
				m.unbindExecutionSessionsForAuthLocked(id)
				delete(m.auths, id)
			}
			m.mu.Unlock()
			if current != nil {
				m.deleteRevokedAuth(ctx, current.Clone(), reason, "quota-refresh")
			}
			return
		}

		entry := log.WithFields(log.Fields{
			"auth_id":     id,
			"provider":    auth.Provider,
			"status_code": resultErr.HTTPStatus,
			"probe_model": probeModel,
		})
		if planType := strings.TrimSpace(codexPlanType(auth)); planType != "" {
			entry = entry.WithField("plan_type", planType)
		}
		if retryAfter != nil {
			entry = entry.WithField("retry_after", retryAfter.String())
		}
		entry.Warnf("quota refresh failed for auth %s%s: %s", id, authPathSuffix(auth), trimLogMessage(resultErr.Message, 512))

		var snapshot *Auth
		var persistAuth *Auth
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			setQuotaProbeCooldown(current, now)
			if resultErr.HTTPStatus == http.StatusTooManyRequests {
				applyQuotaProbeQuotaExceededState(current, resultErr, retryAfter, now)
			} else {
				applyAuthFailureState(current, resultErr, retryAfter, now)
				syncCooldownMetadata(current, now)
				if current.Disabled {
					current.Status = StatusDisabled
				}
			}
			current.UpdatedAt = now
			m.auths[id] = current
			snapshot = current.Clone()
			persistAuth = snapshot.Clone()
		}
		m.mu.Unlock()
		if snapshot != nil && m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
		if persistAuth != nil {
			_ = m.persist(ctx, persistAuth)
		}
		return
	}

	var snapshot *Auth
	var persistAuth *Auth
	m.mu.Lock()
	if current := m.auths[id]; current != nil {
		setQuotaProbeCooldown(current, now)
		applyQuotaProbeSuccessState(current, now)
		current.UpdatedAt = now
		m.auths[id] = current
		snapshot = current.Clone()
		persistAuth = snapshot.Clone()
	}
	m.mu.Unlock()
	if snapshot != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if persistAuth != nil {
		_ = m.persist(ctx, persistAuth)
	}
}

func (m *Manager) refreshCodexUsageAuth(ctx context.Context, id string, exec ProviderExecutor, auth *Auth) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil || exec == nil {
		return
	}

	attemptCtx := ctx
	cancel := func() {}
	if quotaProbeTimeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, quotaProbeTimeout)
	}
	defer cancel()

	requestAuth := auth.Clone()
	if requestAuth == nil {
		return
	}
	if requestAuth.Metadata == nil {
		requestAuth.Metadata = make(map[string]any)
	}
	hydrateCodexAccountID(requestAuth)

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if accountID := strings.TrimSpace(codexAccountID(requestAuth)); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	httpResp, err := exec.HttpRequest(attemptCtx, requestAuth, req)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return
		}
		resultErr := &Error{Message: err.Error()}
		retryAfter := retryAfterFromError(err)
		if se, ok := errors.AsType[cliproxyexecutor.StatusError](err); ok && se != nil {
			resultErr.HTTPStatus = se.StatusCode()
		}
		m.completeCodexUsageRefreshFailure(ctx, id, auth, resultErr, retryAfter)
		return
	}
	defer func() {
		if httpResp != nil && httpResp.Body != nil {
			_ = httpResp.Body.Close()
		}
	}()

	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) && ctx.Err() != nil {
			return
		}
		m.completeCodexUsageRefreshFailure(ctx, id, auth, &Error{Message: readErr.Error()}, nil)
		return
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		resultErr := &Error{Message: strings.TrimSpace(string(body)), HTTPStatus: httpResp.StatusCode}
		retryAfter := retryAfterFromHTTPResponse(httpResp.Header, body, time.Now())
		m.completeCodexUsageRefreshFailure(ctx, id, auth, resultErr, retryAfter)
		return
	}

	payload, parseErr := parseCodexUsagePayload(body)
	if parseErr != nil {
		log.WithError(parseErr).WithField("auth_id", id).Warn("codex usage refresh returned invalid payload")
		return
	}

	now := time.Now()
	remainingPct, hasRemaining := codexUsageRemainingPercentFromPayload(payload)
	nextResetAt, hasNextResetAt := codexUsageNextResetFromPayload(payload, now)
	planType := strings.TrimSpace(codexUsagePlanTypeFromPayload(payload))
	accountID := strings.TrimSpace(codexAccountID(requestAuth))

	var snapshot *Auth
	var persistAuth *Auth
	m.mu.Lock()
	if current := m.auths[id]; current != nil {
		clearGenericQuotaProbeSchedule(current)
		setCodexUsageFetchMetadata(current, now, body, remainingPct, hasRemaining)
		if accountID != "" {
			if current.Metadata == nil {
				current.Metadata = make(map[string]any)
			}
			current.Metadata["account_id"] = accountID
		}
		applyQuotaProbeSuccessState(current, now)
		if planType != "" {
			if len(current.Attributes) == 0 || strings.TrimSpace(current.Attributes[metadataPlanTypeKey]) == "" {
				if current.Metadata == nil {
					current.Metadata = make(map[string]any)
				}
				current.Metadata[metadataPlanTypeKey] = planType
			}
		}
		quotaBlocked, quotaRecoverAt, hasQuotaRecoverAt := codexUsageQuotaBlockedStateFromPayload(payload, now)
		lowBalance := hasRemaining && remainingPct < codexLowRemainingThresholdPct
		disableCooling := quotaCooldownDisabledForAuth(current)
		if quotaBlocked || lowBalance {
			nextRecoverAt := now.Add(quotaProbeCooldown)
			switch {
			case hasQuotaRecoverAt:
				nextRecoverAt = quotaRecoverAt
			case hasNextResetAt:
				nextRecoverAt = nextResetAt
			}
			if quotaBlocked || !disableCooling {
				current.Disabled = true
				current.Status = StatusDisabled
				if quotaBlocked {
					current.StatusMessage = "quota window exhausted; waiting for reset"
				} else {
					current.StatusMessage = "quota remaining below 10%; waiting for next refresh"
				}
				current.Quota = QuotaState{
					Exceeded:      true,
					Reason:        autoDisabledReasonQuotaLowBalance,
					NextRecoverAt: nextRecoverAt,
				}
				setQuotaAutoDisabledReason(current, autoDisabledReasonQuotaLowBalance)
			} else {
				current.Disabled = false
				current.Status = StatusActive
				current.StatusMessage = ""
				current.Quota = QuotaState{}
				clearQuotaProbeAutoDisabled(current)
			}
			setCodexUsageNextRefresh(current, nextRecoverAt)
		} else {
			setCodexUsageNextRefresh(current, time.Time{})
		}
		syncCooldownMetadata(current, now)
		current.UpdatedAt = now
		snapshot = current.Clone()
		persistAuth = snapshot.Clone()
	}
	m.mu.Unlock()
	if snapshot != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if persistAuth != nil {
		_ = m.persist(ctx, persistAuth)
	}
}

func (m *Manager) completeCodexUsageRefreshFailure(ctx context.Context, id string, auth *Auth, resultErr *Error, retryAfter *time.Duration) {
	if resultErr == nil {
		return
	}
	if deleteAuth, reason := shouldDeleteAuthOnQuotaProbe(auth, resultErr, m.CurrentConfig()); deleteAuth {
		m.mu.Lock()
		current := m.auths[id]
		if current != nil {
			m.unbindExecutionSessionsForAuthLocked(id)
			delete(m.auths, id)
		}
		m.mu.Unlock()
		if current != nil {
			m.deleteRevokedAuth(ctx, current.Clone(), reason, "codex-usage-refresh")
		}
		return
	}

	now := time.Now()
	var snapshot *Auth
	var persistAuth *Auth
	m.mu.Lock()
	if current := m.auths[id]; current != nil {
		clearGenericQuotaProbeSchedule(current)
		setCodexUsageFetchMetadata(current, now, nil, 0, false)
		if resultErr.HTTPStatus == http.StatusTooManyRequests {
			applyQuotaProbeQuotaExceededState(current, resultErr, retryAfter, now)
			if !current.Quota.NextRecoverAt.IsZero() {
				setCodexUsageNextRefresh(current, current.Quota.NextRecoverAt)
			} else if retryAfter != nil {
				setCodexUsageNextRefresh(current, now.Add(*retryAfter))
			}
		} else {
			applyAuthFailureState(current, resultErr, retryAfter, now)
			syncCooldownMetadata(current, now)
			if retryAfter != nil {
				setCodexUsageNextRefresh(current, now.Add(*retryAfter))
			}
		}
		current.UpdatedAt = now
		snapshot = current.Clone()
		persistAuth = snapshot.Clone()
	}
	m.mu.Unlock()
	if snapshot != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if persistAuth != nil {
		_ = m.persist(ctx, persistAuth)
	}
}

func (m *Manager) executeQuotaProbe(ctx context.Context, exec ProviderExecutor, auth *Auth) error {
	if exec == nil {
		return nil
	}
	req, opts, err := m.buildQuotaProbeRequest(auth)
	if err != nil {
		return err
	}
	attempts := quotaProbeRetryLimit + 1
	for attempt := 0; attempt < attempts; attempt++ {
		attemptCtx := ctx
		cancel := func() {}
		if quotaProbeTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, quotaProbeTimeout)
		}
		_, execErr := exec.Execute(attemptCtx, auth.Clone(), req, opts)
		cancel()
		if execErr == nil {
			return nil
		}
		statusCode := statusCodeFromError(execErr)
		if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden || statusCode == http.StatusTooManyRequests {
			return execErr
		}
		if attempt == attempts-1 {
			return execErr
		}
	}
	return nil
}

func (m *Manager) buildQuotaProbeRequest(auth *Auth) (cliproxyexecutor.Request, cliproxyexecutor.Options, error) {
	model := pickQuotaProbeModel(auth)
	if model == "" {
		return cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, errQuotaProbeSkippedNoSupportedModel
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"input": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": "quota refresh ping",
			}},
		}},
		"max_output_tokens": 1,
	})
	if err != nil {
		return cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, err
	}
	return cliproxyexecutor.Request{
			Model:   model,
			Payload: payload,
			Format:  sdktranslator.FormatOpenAIResponse,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
		}, nil
}

func codexAccountID(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["account_id"].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func pickQuotaProbeModel(auth *Auth) string {
	if auth == nil {
		return ""
	}
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	if len(models) == 0 {
		models = defaultQuotaProbeModels(strings.ToLower(strings.TrimSpace(auth.Provider)))
	}
	for _, model := range models {
		if model == nil {
			continue
		}
		if candidate := strings.TrimSpace(model.ID); candidate != "" {
			return candidate
		}
	}
	return ""
}

func defaultQuotaProbeModels(provider string) []*registry.ModelInfo {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return registry.GetCodexFreeModels()
	case "claude":
		return registry.GetClaudeModels()
	case "gemini":
		return registry.GetGeminiModels()
	case "gemini-cli":
		return registry.GetGeminiCLIModels()
	case "gemini-vertex":
		return registry.GetGeminiVertexModels()
	case "aistudio":
		return registry.GetAIStudioModels()
	case "qwen":
		return registry.GetQwenModels()
	case "iflow":
		return registry.GetIFlowModels()
	case "kimi":
		return registry.GetKimiModels()
	default:
		return nil
	}
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
	m.noteFreeCodexUse(ctx, auth.ID)
	return exec.HttpRequest(ctx, auth, req)
}
