package auth

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/performance"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// schedulerStrategy identifies which built-in routing semantics the scheduler should apply.
type schedulerStrategy int

const (
	schedulerStrategyCustom schedulerStrategy = iota
	schedulerStrategyRoundRobin
	schedulerStrategyFillFirst
)

const (
	defaultRoundRobinCandidateWindow   = 16
	defaultRoundRobinFullScanThreshold = 32
)

// scheduledState describes how an auth currently participates in a model shard.
type scheduledState int

const (
	scheduledStateReady scheduledState = iota
	scheduledStateCooldown
	scheduledStateBlocked
	scheduledStateDisabled
)

// authScheduler keeps the incremental provider/model scheduling state used by Manager.
type authScheduler struct {
	mu                       sync.Mutex
	strategy                 schedulerStrategy
	providers                map[string]*providerScheduler
	providerIndexValue       atomic.Value
	authProviders            map[string]string
	mixedCursorsMu           sync.Mutex
	mixedCursors             map[string]*schedulerMixedCursor
	inflight                 schedulerInflightMap
	lastSelectionsMu         sync.Mutex
	lastSelections           map[string]string
	performanceRoutingConfig performance.Config
	performanceRoutingValue  atomic.Value
	performanceScorer        PerformanceScorer
	performanceScorerValue   atomic.Value
	strategyValue            atomic.Int32
}

type schedulerInflightCounter struct {
	value atomic.Int64
}

type schedulerProviderIndex struct {
	providers     map[string]*providerScheduler
	authProviders map[string]string
}

type schedulerMixedCursor struct {
	mu    sync.Mutex
	value int
}

const schedulerInflightShardCount = 64

type schedulerInflightMap struct {
	shards [schedulerInflightShardCount]schedulerInflightShard
}

type schedulerInflightShard struct {
	mu       sync.RWMutex
	counters map[string]*schedulerInflightCounter
}

// providerScheduler stores auth metadata and model shards for a single provider.
type providerScheduler struct {
	mu              sync.RWMutex
	providerKey     string
	auths           map[string]*scheduledAuthMeta
	modelShards     map[string]*modelScheduler
	modelIndexValue atomic.Value
}

// scheduledAuthMeta stores the immutable scheduling fields derived from an auth snapshot.
type scheduledAuthMeta struct {
	auth              *Auth
	providerKey       string
	priority          int
	virtualParent     string
	websocketEnabled  bool
	supportedModelSet map[string]struct{}
}

// modelScheduler tracks ready and blocked auths for one provider/model combination.
type modelScheduler struct {
	mu              sync.Mutex
	modelKey        string
	entries         map[string]*scheduledAuth
	priorityOrder   []int
	readyByPriority map[int]*readyBucket
	blocked         cooldownQueue
	dirty           bool
	forceRebuild    bool
	pendingUpserts  map[string]*scheduledAuth
	pendingRemovals map[string]pendingStructuralRemoval
}

type pendingStructuralRemoval struct {
	authID            string
	priority          int
	websocketEnabled  bool
	virtualParentName string
}

// scheduledAuth stores the runtime scheduling state for a single auth inside a model shard.
type scheduledAuth struct {
	meta        *scheduledAuthMeta
	auth        *Auth
	state       scheduledState
	nextRetryAt time.Time
}

// readyBucket keeps the ready views for one priority level.
type readyBucket struct {
	all readyView
	ws  readyView
}

// readyView holds the selection order for flat or grouped round-robin traversal.
type readyView struct {
	flat         []*scheduledAuth
	cursor       int
	parentOrder  []string
	parentCursor int
	children     map[string]*childBucket
}

// childBucket keeps the per-parent rotation state for grouped Gemini virtual auths.
type childBucket struct {
	items  []*scheduledAuth
	cursor int
}

// cooldownQueue is the blocked auth collection ordered by next retry time during rebuilds.
type cooldownQueue []*scheduledAuth

func (s scheduledState) ready() bool {
	return s == scheduledStateReady
}

type readyViewCursorState struct {
	cursor       int
	parentCursor int
	childCursors map[string]int
}

type readyBucketCursorState struct {
	all readyViewCursorState
	ws  readyViewCursorState
}

func snapshotReadyViewCursors(view readyView) readyViewCursorState {
	state := readyViewCursorState{
		cursor:       view.cursor,
		parentCursor: view.parentCursor,
	}
	if len(view.children) == 0 {
		return state
	}
	state.childCursors = make(map[string]int, len(view.children))
	for parent, child := range view.children {
		if child == nil {
			continue
		}
		state.childCursors[parent] = child.cursor
	}
	return state
}

func restoreReadyViewCursors(view *readyView, state readyViewCursorState) {
	if view == nil {
		return
	}
	if len(view.flat) > 0 {
		view.cursor = normalizeCursor(state.cursor, len(view.flat))
	}
	if len(view.parentOrder) == 0 || len(view.children) == 0 {
		return
	}
	view.parentCursor = normalizeCursor(state.parentCursor, len(view.parentOrder))
	if len(state.childCursors) == 0 {
		return
	}
	for parent, child := range view.children {
		if child == nil || len(child.items) == 0 {
			continue
		}
		cursor, ok := state.childCursors[parent]
		if !ok {
			continue
		}
		child.cursor = normalizeCursor(cursor, len(child.items))
	}
}

func normalizeCursor(cursor, size int) int {
	if size <= 0 || cursor <= 0 {
		return 0
	}
	cursor = cursor % size
	if cursor < 0 {
		cursor += size
	}
	return cursor
}

// newAuthScheduler constructs an empty scheduler configured for the supplied selector strategy.
func newAuthScheduler(selector Selector) *authScheduler {
	scheduler := &authScheduler{
		strategy:                 selectorStrategy(selector),
		providers:                make(map[string]*providerScheduler),
		authProviders:            make(map[string]string),
		mixedCursors:             make(map[string]*schedulerMixedCursor),
		lastSelections:           make(map[string]string),
		performanceRoutingConfig: performance.DefaultConfig(),
	}
	scheduler.strategyValue.Store(int32(scheduler.strategy))
	scheduler.performanceRoutingValue.Store(performance.DefaultConfig())
	scheduler.performanceScorerValue.Store(performanceScorerHolder{})
	scheduler.providerIndexValue.Store(schedulerProviderIndex{
		providers:     map[string]*providerScheduler{},
		authProviders: map[string]string{},
	})
	return scheduler
}

// selectorStrategy maps a selector implementation to the scheduler semantics it should emulate.
func selectorStrategy(selector Selector) schedulerStrategy {
	switch selector.(type) {
	case *FillFirstSelector:
		return schedulerStrategyFillFirst
	case nil, *RoundRobinSelector:
		return schedulerStrategyRoundRobin
	default:
		return schedulerStrategyCustom
	}
}

// setSelector updates the active built-in strategy and resets mixed-provider cursors.
func (s *authScheduler) setSelector(selector Selector) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strategy = selectorStrategy(selector)
	s.strategyValue.Store(int32(s.strategy))
	s.clearMixedCursors()
}

func (s *authScheduler) setPerformanceRoutingConfig(cfg performance.Config) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.performanceRoutingConfig = performance.NormalizeConfig(cfg)
	s.performanceRoutingValue.Store(s.performanceRoutingConfig)
}

func (s *authScheduler) setPerformanceScorer(scorer PerformanceScorer) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.performanceScorer = scorer
	s.performanceScorerValue.Store(performanceScorerHolder{scorer: scorer})
}

func (s *authScheduler) currentStrategy() schedulerStrategy {
	if s == nil {
		return schedulerStrategyRoundRobin
	}
	return schedulerStrategy(s.strategyValue.Load())
}

func (s *authScheduler) currentPerformanceRoutingConfig() performance.Config {
	if s == nil {
		return performance.DefaultConfig()
	}
	if value := s.performanceRoutingValue.Load(); value != nil {
		if cfg, ok := value.(performance.Config); ok {
			return cfg
		}
	}
	return performance.DefaultConfig()
}

func (s *authScheduler) currentPerformanceScorer() PerformanceScorer {
	if s == nil {
		return nil
	}
	if value := s.performanceScorerValue.Load(); value != nil {
		if holder, ok := value.(performanceScorerHolder); ok {
			return holder.scorer
		}
	}
	return nil
}

func (s *authScheduler) publishProviderIndexLocked() {
	if s == nil {
		return
	}
	next := make(map[string]*providerScheduler, len(s.providers))
	for providerKey, providerState := range s.providers {
		if providerKey != "" && providerState != nil {
			next[providerKey] = providerState
		}
	}
	nextAuthProviders := make(map[string]string, len(s.authProviders))
	for authID, providerKey := range s.authProviders {
		if authID != "" && providerKey != "" {
			nextAuthProviders[authID] = providerKey
		}
	}
	s.providerIndexValue.Store(schedulerProviderIndex{
		providers:     next,
		authProviders: nextAuthProviders,
	})
}

func (s *authScheduler) loadProvider(providerKey string) *providerScheduler {
	if s == nil || providerKey == "" {
		return nil
	}
	value := s.providerIndexValue.Load()
	index, ok := value.(schedulerProviderIndex)
	if !ok || len(index.providers) == 0 {
		return nil
	}
	return index.providers[providerKey]
}

func (s *authScheduler) loadAuthProvider(authID string) string {
	if s == nil || authID == "" {
		return ""
	}
	value := s.providerIndexValue.Load()
	index, ok := value.(schedulerProviderIndex)
	if !ok || len(index.authProviders) == 0 {
		return ""
	}
	return index.authProviders[authID]
}

func (s *authScheduler) mixedCursor(cursorKey string) *schedulerMixedCursor {
	if s == nil || cursorKey == "" {
		return nil
	}
	s.mixedCursorsMu.Lock()
	defer s.mixedCursorsMu.Unlock()
	if s.mixedCursors == nil {
		s.mixedCursors = make(map[string]*schedulerMixedCursor)
	}
	cursor := s.mixedCursors[cursorKey]
	if cursor == nil {
		cursor = &schedulerMixedCursor{}
		s.mixedCursors[cursorKey] = cursor
	}
	return cursor
}

func (s *authScheduler) clearMixedCursors() {
	if s == nil {
		return
	}
	s.mixedCursorsMu.Lock()
	s.mixedCursors = make(map[string]*schedulerMixedCursor)
	s.mixedCursorsMu.Unlock()
}

func lockProviderStates(providerStates []*providerScheduler) []*providerScheduler {
	if len(providerStates) == 0 {
		return nil
	}
	seen := make(map[*providerScheduler]struct{}, len(providerStates))
	locked := make([]*providerScheduler, 0, len(providerStates))
	for _, providerState := range providerStates {
		if providerState == nil {
			continue
		}
		if _, ok := seen[providerState]; ok {
			continue
		}
		seen[providerState] = struct{}{}
		locked = append(locked, providerState)
	}
	sort.Slice(locked, func(i, j int) bool {
		return locked[i].providerKey < locked[j].providerKey
	})
	for _, providerState := range locked {
		providerState.mu.Lock()
	}
	return locked
}

func unlockProviderStates(providerStates []*providerScheduler) {
	for i := len(providerStates) - 1; i >= 0; i-- {
		providerStates[i].mu.Unlock()
	}
}

type lockedModelShard struct {
	providerKey string
	shard       *modelScheduler
}

func lockModelShards(shards []lockedModelShard) []lockedModelShard {
	if len(shards) == 0 {
		return nil
	}
	seen := make(map[*modelScheduler]struct{}, len(shards))
	locked := make([]lockedModelShard, 0, len(shards))
	for _, candidate := range shards {
		if candidate.shard == nil {
			continue
		}
		if _, ok := seen[candidate.shard]; ok {
			continue
		}
		seen[candidate.shard] = struct{}{}
		locked = append(locked, candidate)
	}
	sort.Slice(locked, func(i, j int) bool {
		if locked[i].providerKey == locked[j].providerKey {
			return locked[i].shard.modelKey < locked[j].shard.modelKey
		}
		return locked[i].providerKey < locked[j].providerKey
	})
	for _, candidate := range locked {
		candidate.shard.mu.Lock()
	}
	return locked
}

func unlockModelShards(shards []lockedModelShard) {
	for i := len(shards) - 1; i >= 0; i-- {
		shards[i].shard.mu.Unlock()
	}
}

func schedulerInflightKey(providerKey, modelKey, authID string) string {
	providerKey = strings.ToLower(strings.TrimSpace(providerKey))
	modelKey = canonicalModelKey(modelKey)
	authID = strings.TrimSpace(authID)
	if providerKey == "" || authID == "" {
		return ""
	}
	return providerKey + "|" + modelKey + "|" + authID
}

func schedulerSelectionKeySingle(providerKey, modelKey string) string {
	return "single:" + providerKey + ":" + modelKey
}

func schedulerInflightShardIndex(key string) uint32 {
	const prime32 = 16777619
	hash := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime32
	}
	return hash % schedulerInflightShardCount
}

func (m *schedulerInflightMap) acquire(key string) {
	if key == "" {
		return
	}
	shard := &m.shards[schedulerInflightShardIndex(key)]
	shard.mu.RLock()
	counter := shard.counters[key]
	if counter != nil {
		counter.value.Add(1)
		shard.mu.RUnlock()
		return
	}
	shard.mu.RUnlock()
	shard.mu.Lock()
	if shard.counters == nil {
		shard.counters = make(map[string]*schedulerInflightCounter)
	}
	counter = shard.counters[key]
	if counter == nil {
		counter = &schedulerInflightCounter{}
		shard.counters[key] = counter
	}
	counter.value.Add(1)
	shard.mu.Unlock()
}

func (m *schedulerInflightMap) release(key string) {
	if key == "" {
		return
	}
	shard := &m.shards[schedulerInflightShardIndex(key)]
	shard.mu.RLock()
	counter := shard.counters[key]
	shard.mu.RUnlock()
	if counter == nil {
		return
	}
	if counter.value.Add(-1) > 0 {
		return
	}
	shard.mu.Lock()
	if current := shard.counters[key]; current == counter && counter.value.Load() <= 0 {
		delete(shard.counters, key)
	}
	shard.mu.Unlock()
}

func (m *schedulerInflightMap) count(key string) int {
	if key == "" {
		return 0
	}
	shard := &m.shards[schedulerInflightShardIndex(key)]
	shard.mu.RLock()
	counter := shard.counters[key]
	shard.mu.RUnlock()
	if counter == nil {
		return 0
	}
	current := counter.value.Load()
	if current <= 0 {
		return 0
	}
	return int(current)
}

func (s *authScheduler) acquireInflight(provider, model, authID string) {
	if s == nil {
		return
	}
	key := schedulerInflightKey(provider, model, authID)
	if key == "" {
		return
	}
	s.inflight.acquire(key)
}

func (s *authScheduler) releaseInflight(provider, model, authID string) {
	if s == nil {
		return
	}
	key := schedulerInflightKey(provider, model, authID)
	if key == "" {
		return
	}
	s.inflight.release(key)
}

func (s *authScheduler) inflightCountLocked(providerKey, modelKey, authID string) int {
	if s == nil {
		return 0
	}
	key := schedulerInflightKey(providerKey, modelKey, authID)
	if key == "" {
		return 0
	}
	return s.inflight.count(key)
}

func (s *authScheduler) logFillFirstSelectionSwitchLocked(ctx context.Context, routeKey string, providerKey string, modelKey string, shard *modelScheduler, picked *Auth, selectionAttempt int) {
	if s == nil || picked == nil || routeKey == "" {
		return
	}
	s.lastSelectionsMu.Lock()
	defer s.lastSelectionsMu.Unlock()
	previousAuthID := strings.TrimSpace(s.lastSelections[routeKey])
	s.lastSelections[routeKey] = picked.ID
	if previousAuthID == "" || previousAuthID == picked.ID {
		return
	}
	if !log.IsLevelEnabled(log.InfoLevel) {
		return
	}
	entry := logEntryWithRequestID(ctx)
	if entry == nil {
		return
	}
	entry.WithFields(log.Fields{
		"provider":           providerKey,
		"model":              modelKey,
		"previous_auth_id":   previousAuthID,
		"selected_auth_id":   picked.ID,
		"selection_attempt":  selectionAttempt,
		"request_retry":      selectionAttempt > 1,
		"selection_snapshot": shard.selectionSnapshotLocked(modelKey, 5),
	}).Info("fill-first auth switched")
}

// rebuild recreates the complete scheduler state from an auth snapshot.
func (s *authScheduler) rebuild(auths []*Auth) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.providers = make(map[string]*providerScheduler)
	s.authProviders = make(map[string]string)
	s.clearMixedCursors()
	now := time.Now()
	for _, auth := range auths {
		s.upsertAuthLocked(auth, now)
	}
	s.publishProviderIndexLocked()
}

// upsertAuth incrementally synchronizes one auth into the scheduler.
func (s *authScheduler) upsertAuth(auth *Auth) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertAuthLocked(auth, time.Now()) {
		s.publishProviderIndexLocked()
	}
}

// removeAuth deletes one auth from every scheduler shard that references it.
func (s *authScheduler) removeAuth(authID string) {
	if s == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removeAuthLocked(authID) {
		s.publishProviderIndexLocked()
	}
}

// pickSingle returns the next auth for a single provider/model request using scheduler state.
func (s *authScheduler) pickSingle(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, error) {
	if s == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	modelKey := canonicalModelKey(model)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	allowedAuthIDs := selectionScopeAuthIDsFromMetadata(opts.Metadata)
	preferWebsocket := cliproxyexecutor.DownstreamWebsocket(ctx) && providerKey == "codex" && pinnedAuthID == ""
	strategy := s.currentStrategy()
	if preferBestAuthFromMetadata(opts.Metadata) {
		strategy = schedulerStrategyFillFirst
	}

	providerState := s.loadProvider(providerKey)
	if providerState == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	now := time.Now()
	shard := providerState.loadModel(modelKey)
	if shard == nil {
		providerState.mu.Lock()
		shard = providerState.ensureModelLocked(modelKey, now)
		providerState.mu.Unlock()
	}
	if shard == nil {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.ensureIndexesFreshLocked()
	shard.promoteExpiredLocked(now)
	predicate := func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		if pinnedAuthID != "" && entry.auth.ID != pinnedAuthID {
			return false
		}
		if !authAllowedBySelectionScope(allowedAuthIDs, entry.auth.ID) {
			return false
		}
		if len(tried) > 0 {
			if _, ok := tried[entry.auth.ID]; ok {
				return false
			}
		}
		return true
	}
	load := func(entry *scheduledAuth) int {
		if entry == nil || entry.auth == nil {
			return 0
		}
		return s.inflightCountLocked(providerKey, modelKey, entry.auth.ID)
	}
	performanceCfg := s.currentPerformanceRoutingConfig()
	performanceScorer := s.currentPerformanceScorer()
	selectionAttempt := len(tried) + 1
	if strategy == schedulerStrategyFillFirst {
		eligible := shard.selectionPredicate(predicate, now)
		priorityReady, okPriority := shard.highestReadyPriorityLocked(preferWebsocket, eligible)
		if !okPriority {
			return nil, shard.unavailableErrorLocked(provider, model, predicate)
		}
		if picked := shard.pickReadyAtPriorityLocked(providerKey, modelKey, preferWebsocket, priorityReady, strategy, eligible, load, performanceCfg, performanceScorer); picked != nil {
			s.logFillFirstSelectionSwitchLocked(ctx, schedulerSelectionKeySingle(providerKey, modelKey), providerKey, modelKey, shard, picked, selectionAttempt)
			return picked, nil
		}
		return nil, shard.unavailableErrorLocked(provider, model, predicate)
	}
	if picked := shard.pickReadyLocked(providerKey, modelKey, preferWebsocket, strategy, predicate, load, performanceCfg, performanceScorer); picked != nil {
		return picked, nil
	}
	return nil, shard.unavailableErrorLocked(provider, model, predicate)
}

// pickMixed returns the next auth and provider for a mixed-provider request.
func (s *authScheduler) pickMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, string, error) {
	if s == nil {
		return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	normalized := normalizeProviderKeys(providers)
	if len(normalized) == 0 {
		return nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	if len(normalized) == 1 {
		// When a single provider is eligible, reuse pickSingle so provider-specific preferences
		// (for example Codex websocket transport) are applied consistently.
		providerKey := normalized[0]
		picked, errPick := s.pickSingle(ctx, providerKey, model, opts, tried)
		if errPick != nil {
			return nil, "", errPick
		}
		if picked == nil {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		return picked, providerKey, nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	allowedAuthIDs := selectionScopeAuthIDsFromMetadata(opts.Metadata)
	strategy := s.currentStrategy()
	if preferBestAuthFromMetadata(opts.Metadata) {
		strategy = schedulerStrategyFillFirst
	}
	modelKey := canonicalModelKey(model)

	performanceCfg := s.currentPerformanceRoutingConfig()
	performanceScorer := s.currentPerformanceScorer()
	if pinnedAuthID != "" {
		if !authAllowedBySelectionScope(allowedAuthIDs, pinnedAuthID) {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		providerKey := s.loadAuthProvider(pinnedAuthID)
		if providerKey == "" || !containsProvider(normalized, providerKey) {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		providerState := s.loadProvider(providerKey)
		if providerState == nil {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		now := time.Now()
		shard := providerState.loadModel(modelKey)
		if shard == nil {
			providerState.mu.Lock()
			shard = providerState.ensureModelLocked(modelKey, now)
			providerState.mu.Unlock()
		}
		if shard == nil {
			return nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		shard.mu.Lock()
		defer shard.mu.Unlock()
		shard.ensureIndexesFreshLocked()
		shard.promoteExpiredLocked(now)
		predicate := func(entry *scheduledAuth) bool {
			if entry == nil || entry.auth == nil || entry.auth.ID != pinnedAuthID {
				return false
			}
			if !authAllowedBySelectionScope(allowedAuthIDs, entry.auth.ID) {
				return false
			}
			if len(tried) == 0 {
				return true
			}
			_, ok := tried[pinnedAuthID]
			return !ok
		}
		load := func(entry *scheduledAuth) int {
			if entry == nil || entry.auth == nil {
				return 0
			}
			return s.inflightCountLocked(providerKey, modelKey, entry.auth.ID)
		}
		if picked := shard.pickReadyLocked(providerKey, modelKey, false, strategy, predicate, load, performanceCfg, performanceScorer); picked != nil {
			return picked, providerKey, nil
		}
		return nil, "", shard.unavailableErrorLocked("mixed", model, predicate)
	}

	predicate := func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		if !authAllowedBySelectionScope(allowedAuthIDs, entry.auth.ID) {
			return false
		}
		if len(tried) == 0 {
			return true
		}
		_, used := tried[entry.auth.ID]
		return !used
	}
	candidateShards := make([]*modelScheduler, len(normalized))
	candidatePredicates := make([]func(*scheduledAuth) bool, len(normalized))
	bestPriority := 0
	hasCandidate := false
	now := time.Now()
	providerStates := make([]*providerScheduler, len(normalized))
	for providerIndex, providerKey := range normalized {
		providerStates[providerIndex] = s.loadProvider(providerKey)
	}
	lockedShards := make([]lockedModelShard, 0, len(normalized))
	for providerIndex := range normalized {
		providerState := providerStates[providerIndex]
		if providerState == nil {
			continue
		}
		shard := providerState.loadModel(modelKey)
		if shard == nil {
			providerState.mu.Lock()
			shard = providerState.ensureModelLocked(modelKey, now)
			providerState.mu.Unlock()
		}
		candidateShards[providerIndex] = shard
		if shard != nil {
			lockedShards = append(lockedShards, lockedModelShard{providerKey: normalized[providerIndex], shard: shard})
		}
	}
	lockedShards = lockModelShards(lockedShards)
	defer unlockModelShards(lockedShards)
	for providerIndex := range normalized {
		shard := candidateShards[providerIndex]
		if shard == nil {
			continue
		}
		shard.ensureIndexesFreshLocked()
		shard.promoteExpiredLocked(now)
		eligible := shard.selectionPredicate(predicate, now)
		candidatePredicates[providerIndex] = eligible
		priorityReady, okPriority := shard.highestReadyPriorityLocked(false, eligible)
		if !okPriority {
			continue
		}
		if !hasCandidate || priorityReady > bestPriority {
			bestPriority = priorityReady
			hasCandidate = true
		}
	}
	if !hasCandidate {
		return nil, "", s.mixedUnavailableErrorLocked(candidateShards, model, tried)
	}

	if strategy == schedulerStrategyFillFirst {
		for providerIndex, providerKey := range normalized {
			shard := candidateShards[providerIndex]
			if shard == nil {
				continue
			}
			eligible := candidatePredicates[providerIndex]
			if eligible == nil {
				eligible = shard.selectionPredicate(predicate, now)
			}
			load := func(entry *scheduledAuth) int {
				if entry == nil || entry.auth == nil {
					return 0
				}
				return s.inflightCountLocked(providerKey, modelKey, entry.auth.ID)
			}
			picked := shard.pickReadyAtPriorityLocked(providerKey, modelKey, false, bestPriority, strategy, eligible, load, performanceCfg, performanceScorer)
			if picked != nil {
				return picked, providerKey, nil
			}
		}
		return nil, "", s.mixedUnavailableErrorLocked(candidateShards, model, tried)
	}

	cursorKey := strings.Join(normalized, ",") + ":" + modelKey
	weights := make([]int, len(normalized))
	segmentStarts := make([]int, len(normalized))
	segmentEnds := make([]int, len(normalized))
	totalWeight := 0
	for providerIndex, shard := range candidateShards {
		segmentStarts[providerIndex] = totalWeight
		if shard != nil {
			weights[providerIndex] = shard.readyCountAtPriorityLocked(false, bestPriority)
		}
		totalWeight += weights[providerIndex]
		segmentEnds[providerIndex] = totalWeight
	}
	if totalWeight == 0 {
		return nil, "", s.mixedUnavailableErrorLocked(candidateShards, model, tried)
	}

	cursor := s.mixedCursor(cursorKey)
	if cursor == nil {
		return nil, "", &Error{Code: "auth_unavailable", Message: "no auth available"}
	}
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	startSlot := cursor.value % totalWeight
	startProviderIndex := -1
	for providerIndex := range normalized {
		if weights[providerIndex] == 0 {
			continue
		}
		if startSlot < segmentEnds[providerIndex] {
			startProviderIndex = providerIndex
			break
		}
	}
	if startProviderIndex < 0 {
		return nil, "", s.mixedUnavailableErrorLocked(candidateShards, model, tried)
	}

	slot := startSlot
	for offset := 0; offset < len(normalized); offset++ {
		providerIndex := (startProviderIndex + offset) % len(normalized)
		if weights[providerIndex] == 0 {
			continue
		}
		if providerIndex != startProviderIndex {
			slot = segmentStarts[providerIndex]
		}
		providerKey := normalized[providerIndex]
		shard := candidateShards[providerIndex]
		if shard == nil {
			continue
		}
		eligible := candidatePredicates[providerIndex]
		if eligible == nil {
			eligible = shard.selectionPredicate(predicate, now)
		}
		load := func(entry *scheduledAuth) int {
			if entry == nil || entry.auth == nil {
				return 0
			}
			return s.inflightCountLocked(providerKey, modelKey, entry.auth.ID)
		}
		picked := shard.pickReadyAtPriorityLocked(providerKey, modelKey, false, bestPriority, schedulerStrategyRoundRobin, eligible, load, performanceCfg, performanceScorer)
		if picked == nil {
			continue
		}
		cursor.value = slot + 1
		return picked, providerKey, nil
	}
	return nil, "", s.mixedUnavailableErrorLocked(candidateShards, model, tried)
}

// mixedUnavailableErrorLocked synthesizes the mixed-provider cooldown or unavailable error.
func (s *authScheduler) mixedUnavailableErrorLocked(candidateShards []*modelScheduler, model string, tried map[string]struct{}) error {
	now := time.Now()
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	for _, shard := range candidateShards {
		if shard == nil {
			continue
		}
		localTotal, localCooldownCount, localEarliest := shard.availabilitySummaryLocked(triedPredicate(tried))
		total += localTotal
		cooldownCount += localCooldownCount
		if !localEarliest.IsZero() && (earliest.IsZero() || localEarliest.Before(earliest)) {
			earliest = localEarliest
		}
	}
	if total == 0 {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if cooldownCount == total && !earliest.IsZero() {
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(model, "", resetIn)
	}
	return &Error{Code: "auth_unavailable", Message: "no auth available"}
}

// triedPredicate builds a filter that excludes auths already attempted for the current request.
func triedPredicate(tried map[string]struct{}) func(*scheduledAuth) bool {
	if len(tried) == 0 {
		return func(entry *scheduledAuth) bool { return entry != nil && entry.auth != nil }
	}
	return func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		_, ok := tried[entry.auth.ID]
		return !ok
	}
}

// normalizeProviderKeys lowercases, trims, and de-duplicates provider keys while preserving order.
func normalizeProviderKeys(providers []string) []string {
	seen := make(map[string]struct{}, len(providers))
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		providerKey := strings.ToLower(strings.TrimSpace(provider))
		if providerKey == "" {
			continue
		}
		if _, ok := seen[providerKey]; ok {
			continue
		}
		seen[providerKey] = struct{}{}
		out = append(out, providerKey)
	}
	return out
}

// containsProvider reports whether provider is present in the normalized provider list.
func containsProvider(providers []string, provider string) bool {
	for _, candidate := range providers {
		if candidate == provider {
			return true
		}
	}
	return false
}

func preferBestAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.PreferBestAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	prefer, ok := raw.(bool)
	return ok && prefer
}

// upsertAuthLocked updates one auth in-place while the scheduler mutex is held.
func (s *authScheduler) upsertAuthLocked(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	authID := strings.TrimSpace(auth.ID)
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if authID == "" || providerKey == "" {
		return s.removeAuthLocked(authID)
	}
	changedIndex := false
	if previousProvider := s.authProviders[authID]; previousProvider != "" && previousProvider != providerKey {
		if previousState := s.providers[previousProvider]; previousState != nil {
			previousState.mu.Lock()
			previousState.removeAuthLocked(authID)
			previousState.mu.Unlock()
		}
		changedIndex = true
	} else if previousProvider == "" {
		changedIndex = true
	}
	meta := buildScheduledAuthMeta(auth)
	s.authProviders[authID] = providerKey
	providerState, createdProvider := s.ensureProviderLocked(providerKey)
	if createdProvider {
		changedIndex = true
	}
	providerState.mu.Lock()
	defer providerState.mu.Unlock()
	providerState.upsertAuthLocked(meta, now)
	return changedIndex
}

// removeAuthLocked removes one auth from the scheduler while the scheduler mutex is held.
func (s *authScheduler) removeAuthLocked(authID string) bool {
	if authID == "" {
		return false
	}
	if providerKey := s.authProviders[authID]; providerKey != "" {
		if providerState := s.providers[providerKey]; providerState != nil {
			providerState.mu.Lock()
			providerState.removeAuthLocked(authID)
			providerState.mu.Unlock()
		}
		delete(s.authProviders, authID)
		return true
	}
	return false
}

// ensureProviderLocked returns the provider scheduler for providerKey, creating it when needed.
func (s *authScheduler) ensureProviderLocked(providerKey string) (*providerScheduler, bool) {
	if s.providers == nil {
		s.providers = make(map[string]*providerScheduler)
	}
	providerState := s.providers[providerKey]
	if providerState == nil {
		providerState = &providerScheduler{
			providerKey: providerKey,
			auths:       make(map[string]*scheduledAuthMeta),
			modelShards: make(map[string]*modelScheduler),
		}
		providerState.modelIndexValue.Store(map[string]*modelScheduler{})
		s.providers[providerKey] = providerState
		return providerState, true
	}
	return providerState, false
}

// buildScheduledAuthMeta extracts the scheduling metadata needed for shard bookkeeping.
func buildScheduledAuthMeta(auth *Auth) *scheduledAuthMeta {
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	virtualParent := ""
	if auth.Attributes != nil {
		virtualParent = strings.TrimSpace(auth.Attributes["gemini_virtual_parent"])
	}
	return &scheduledAuthMeta{
		auth:              auth,
		providerKey:       providerKey,
		priority:          authPriority(auth),
		virtualParent:     virtualParent,
		websocketEnabled:  authWebsocketsEnabled(auth),
		supportedModelSet: supportedModelSetForAuth(auth.ID),
	}
}

func (p *providerScheduler) publishModelIndexLocked() {
	if p == nil {
		return
	}
	next := make(map[string]*modelScheduler, len(p.modelShards))
	for modelKey, shard := range p.modelShards {
		if shard != nil {
			next[modelKey] = shard
		}
	}
	p.modelIndexValue.Store(next)
}

func (p *providerScheduler) loadModel(modelKey string) *modelScheduler {
	if p == nil {
		return nil
	}
	modelKey = canonicalModelKey(modelKey)
	value := p.modelIndexValue.Load()
	index, ok := value.(map[string]*modelScheduler)
	if !ok || len(index) == 0 {
		return nil
	}
	return index[modelKey]
}

// supportedModelSetForAuth snapshots the registry models currently registered for an auth.
func supportedModelSetForAuth(authID string) map[string]struct{} {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	models := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(models) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelKey := canonicalModelKey(model.ID)
		if modelKey == "" {
			continue
		}
		set[modelKey] = struct{}{}
	}
	return set
}

// upsertAuthLocked updates every existing model shard that can reference the auth metadata.
func (p *providerScheduler) upsertAuthLocked(meta *scheduledAuthMeta, now time.Time) {
	if p == nil || meta == nil || meta.auth == nil {
		return
	}
	p.auths[meta.auth.ID] = meta
	for modelKey, shard := range p.modelShards {
		if shard == nil {
			continue
		}
		shard.mu.Lock()
		if !meta.supportsModel(modelKey) {
			shard.removeEntryLocked(meta.auth.ID)
			shard.mu.Unlock()
			continue
		}
		shard.upsertEntryLocked(meta, now)
		shard.mu.Unlock()
	}
}

// removeAuthLocked removes an auth from all model shards owned by the provider scheduler.
func (p *providerScheduler) removeAuthLocked(authID string) {
	if p == nil || authID == "" {
		return
	}
	delete(p.auths, authID)
	for _, shard := range p.modelShards {
		if shard != nil {
			shard.mu.Lock()
			shard.removeEntryLocked(authID)
			shard.mu.Unlock()
		}
	}
}

// ensureModelLocked returns the shard for modelKey, building it lazily from provider auths.
func (p *providerScheduler) ensureModelLocked(modelKey string, now time.Time) *modelScheduler {
	if p == nil {
		return nil
	}
	modelKey = canonicalModelKey(modelKey)
	if shard, ok := p.modelShards[modelKey]; ok && shard != nil {
		return shard
	}
	p.refreshSupportedModelsLocked()
	shard := &modelScheduler{
		modelKey:        modelKey,
		entries:         make(map[string]*scheduledAuth),
		readyByPriority: make(map[int]*readyBucket),
	}
	for _, meta := range p.auths {
		if meta == nil || !meta.supportsModel(modelKey) {
			continue
		}
		shard.upsertEntryLocked(meta, now)
	}
	shard.ensureIndexesFreshLocked()
	p.modelShards[modelKey] = shard
	p.publishModelIndexLocked()
	return shard
}

func (p *providerScheduler) refreshSupportedModelsLocked() {
	if p == nil || len(p.auths) == 0 {
		return
	}
	for authID, meta := range p.auths {
		if meta == nil || strings.TrimSpace(authID) == "" {
			continue
		}
		meta.supportedModelSet = supportedModelSetForAuth(authID)
	}
}

// supportsModel reports whether the auth metadata currently supports modelKey.
func (m *scheduledAuthMeta) supportsModel(modelKey string) bool {
	modelKey = canonicalModelKey(modelKey)
	if modelKey == "" {
		return true
	}
	if len(m.supportedModelSet) == 0 {
		return false
	}
	_, ok := m.supportedModelSet[modelKey]
	return ok
}

// upsertEntryLocked updates or inserts one auth entry and rebuilds indexes when ordering changes.
func (m *modelScheduler) upsertEntryLocked(meta *scheduledAuthMeta, now time.Time) {
	if m == nil || meta == nil || meta.auth == nil {
		return
	}
	entry, ok := m.entries[meta.auth.ID]
	if !ok || entry == nil {
		entry = &scheduledAuth{}
		m.entries[meta.auth.ID] = entry
	}
	previousState := entry.state
	previousNextRetryAt := entry.nextRetryAt
	previousAuth := entry.auth
	previousPriority := 0
	previousParent := ""
	previousWebsocketEnabled := false
	if entry.meta != nil {
		previousPriority = entry.meta.priority
		previousParent = entry.meta.virtualParent
		previousWebsocketEnabled = entry.meta.websocketEnabled
	}

	entry.meta = meta
	entry.auth = meta.auth
	entry.nextRetryAt = time.Time{}
	blocked, reason, next := isAuthBlockedForModel(meta.auth, m.modelKey, now)
	switch {
	case !blocked:
		entry.state = scheduledStateReady
	case reason == blockReasonCooldown:
		entry.state = scheduledStateCooldown
		entry.nextRetryAt = next
	case reason == blockReasonDisabled:
		entry.state = scheduledStateDisabled
	default:
		entry.state = scheduledStateBlocked
		entry.nextRetryAt = next
	}

	if ok && previousState == entry.state && previousNextRetryAt.Equal(entry.nextRetryAt) && previousPriority == meta.priority && previousParent == meta.virtualParent && previousWebsocketEnabled == meta.websocketEnabled {
		return
	}
	if ok && previousPriority == meta.priority && previousParent == meta.virtualParent && previousWebsocketEnabled == meta.websocketEnabled {
		if m.applyNonStructuralTransitionLocked(entry, previousAuth, previousState, previousNextRetryAt) {
			return
		}
	}
	if ok {
		m.forceRebuild = true
	} else {
		if m.pendingUpserts == nil {
			m.pendingUpserts = make(map[string]*scheduledAuth)
		}
		m.pendingUpserts[meta.auth.ID] = entry
	}
	m.dirty = true
}

// removeEntryLocked deletes one auth entry and rebuilds the shard indexes if needed.
func (m *modelScheduler) removeEntryLocked(authID string) {
	if m == nil || authID == "" {
		return
	}
	entry, ok := m.entries[authID]
	if !ok {
		return
	}
	if m.pendingRemovals == nil {
		m.pendingRemovals = make(map[string]pendingStructuralRemoval)
	}
	removal := pendingStructuralRemoval{authID: authID}
	if entry != nil && entry.meta != nil {
		removal.priority = entry.meta.priority
		removal.websocketEnabled = entry.meta.websocketEnabled
		removal.virtualParentName = entry.meta.virtualParent
	}
	m.pendingRemovals[authID] = removal
	delete(m.pendingUpserts, authID)
	delete(m.entries, authID)
	m.dirty = true
}

// promoteExpiredLocked reevaluates blocked auths whose retry time has elapsed.
func (m *modelScheduler) promoteExpiredLocked(now time.Time) {
	if m == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	for len(m.blocked) > 0 {
		entry := m.blocked[0]
		if entry == nil || entry.auth == nil {
			m.blocked = m.blocked[1:]
			continue
		}
		if entry.state != scheduledStateCooldown {
			m.blocked = m.blocked[1:]
			continue
		}
		if !entry.nextRetryAt.IsZero() && entry.nextRetryAt.After(now) {
			break
		}
		m.blocked = m.blocked[1:]
		blocked, reason, next := isAuthBlockedForModel(entry.auth, m.modelKey, now)
		if blocked {
			entry.nextRetryAt = next
			switch reason {
			case blockReasonCooldown:
				entry.state = scheduledStateCooldown
				m.upsertCooldownEntryLocked(entry)
			case blockReasonDisabled:
				entry.state = scheduledStateDisabled
			default:
				entry.state = scheduledStateBlocked
			}
			continue
		}
		entry.state = scheduledStateReady
		entry.nextRetryAt = time.Time{}
		if !m.canApplyReadyViewMutationLocked(entry.meta) {
			m.forceRebuild = true
			m.dirty = true
			return
		}
		m.insertEntryIntoReadyIndexesLocked(entry)
	}
}

// pickReadyLocked selects the next ready auth from the highest available priority bucket.
func (m *modelScheduler) pickReadyLocked(providerKey, modelKey string, preferWebsocket bool, strategy schedulerStrategy, predicate func(*scheduledAuth) bool, load func(*scheduledAuth) int, performanceCfg performance.Config, performanceScorer PerformanceScorer) *Auth {
	if m == nil {
		return nil
	}
	m.ensureIndexesFreshLocked()
	now := time.Now()
	eligible := m.selectionPredicate(predicate, now)
	priorityReady, okPriority := m.highestReadyPriorityLocked(preferWebsocket, eligible)
	if !okPriority {
		return nil
	}
	return m.pickReadyAtPriorityLocked(providerKey, modelKey, preferWebsocket, priorityReady, strategy, eligible, load, performanceCfg, performanceScorer)
}

// highestReadyPriorityLocked returns the highest priority bucket that still has a matching ready auth.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) highestReadyPriorityLocked(preferWebsocket bool, predicate func(*scheduledAuth) bool) (int, bool) {
	if m == nil {
		return 0, false
	}
	m.ensureIndexesFreshLocked()
	if preferWebsocket {
		// When downstream is websocket and Codex supports websocket transport, prefer websocket-enabled
		// credentials even if they are in a lower priority tier than HTTP-only credentials.
		for _, priority := range m.priorityOrder {
			bucket := m.readyByPriority[priority]
			if bucket == nil {
				continue
			}
			if bucket.ws.pickFirst(predicate) != nil {
				return priority, true
			}
		}
	}
	for _, priority := range m.priorityOrder {
		bucket := m.readyByPriority[priority]
		if bucket == nil {
			continue
		}
		if bucket.all.pickFirst(predicate) != nil {
			return priority, true
		}
	}
	return 0, false
}

func (m *modelScheduler) selectionPredicate(base func(*scheduledAuth) bool, now time.Time) func(*scheduledAuth) bool {
	return func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		if blocked, _, _ := isAuthBlockedForModel(entry.auth, m.modelKey, now); blocked {
			return false
		}
		if base != nil && !base(entry) {
			return false
		}
		return true
	}
}

// pickReadyAtPriorityLocked selects the next ready auth from a specific priority bucket.
// The caller must ensure expired entries are already promoted when needed.
func (m *modelScheduler) pickReadyAtPriorityLocked(providerKey, modelKey string, preferWebsocket bool, priority int, strategy schedulerStrategy, predicate func(*scheduledAuth) bool, load func(*scheduledAuth) int, performanceCfg performance.Config, performanceScorer PerformanceScorer) *Auth {
	if m == nil {
		return nil
	}
	m.ensureIndexesFreshLocked()
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return nil
	}
	view := &bucket.all
	if preferWebsocket && bucket.ws.pickFirst(predicate) != nil {
		view = &bucket.ws
	}
	var picked *scheduledAuth
	if strategy == schedulerStrategyFillFirst {
		picked = view.pickFirst(predicate)
	} else {
		picked = view.pickRoundRobin(predicate, load)
	}
	if picked == nil || picked.auth == nil {
		return nil
	}
	performanceCfg = performance.NormalizeConfig(performanceCfg)
	if !performanceCfg.Enabled && !performanceCfg.ShadowLog {
		return picked.auth
	}
	performancePicked, scores := view.performanceRankedLocked(providerKey, modelKey, predicate, load, performanceCfg, performanceScorer)
	view.logPerformanceShadowSelection(providerKey, modelKey, picked, performancePicked, scores, performanceCfg)
	if performanceCfg.Enabled && performancePicked != nil && performancePicked.auth != nil && readyScoreCount(scores) > 0 {
		return performancePicked.auth
	}
	return picked.auth
}

func (v *readyView) performanceRankedLocked(providerKey, modelKey string, predicate func(*scheduledAuth) bool, load func(*scheduledAuth) int, cfg performance.Config, scorer PerformanceScorer) (*scheduledAuth, []performance.Score) {
	cfg = performance.NormalizeConfig(cfg)
	if v == nil || scorer == nil || (!cfg.Enabled && !cfg.ShadowLog) {
		return nil, nil
	}
	entriesByAuthID := make(map[string]*scheduledAuth, len(v.flat))
	candidates := make([]performance.ScoreCandidate, 0, len(v.flat))
	for _, entry := range v.flat {
		if entry == nil || entry.auth == nil {
			continue
		}
		if predicate != nil && !predicate(entry) {
			continue
		}
		authID := entry.auth.ID
		entriesByAuthID[authID] = entry
		candidate := scorer.SnapshotCandidate(providerKey, authID, modelKey, inflightLoadValue(load, entry))
		if strings.TrimSpace(candidate.Provider) == "" {
			candidate.Provider = providerKey
		}
		if strings.TrimSpace(candidate.AuthID) == "" {
			candidate.AuthID = authID
		}
		if strings.TrimSpace(candidate.Model) == "" {
			candidate.Model = modelKey
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	scores := scorer.ScoreCandidates(candidates, cfg)
	if len(scores) == 0 {
		return nil, nil
	}
	best := entriesByAuthID[scores[0].AuthID]
	if best == nil || best.auth == nil {
		return nil, scores
	}
	return best, scores
}

func (v *readyView) logPerformanceShadowSelection(providerKey, modelKey string, picked *scheduledAuth, shadowPicked *scheduledAuth, scores []performance.Score, cfg performance.Config) {
	if picked == nil || picked.auth == nil {
		return
	}
	cfg = performance.NormalizeConfig(cfg)
	if !cfg.ShadowLog {
		return
	}
	if shadowPicked == nil || shadowPicked.auth == nil || shadowPicked.auth.ID == picked.auth.ID {
		return
	}
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	log.WithFields(log.Fields{
		"provider":           providerKey,
		"model":              modelKey,
		"selected_auth_id":   picked.auth.ID,
		"shadow_auth_id":     shadowPicked.auth.ID,
		"selected_score":     scoreForAuthID(scores, picked.auth.ID),
		"shadow_score":       scoreForAuthID(scores, shadowPicked.auth.ID),
		"candidate_count":    len(scores),
		"sample_ready_count": readyScoreCount(scores),
	}).Debug("auth performance shadow selection differs")
}

func scoreForAuthID(scores []performance.Score, authID string) float64 {
	for _, score := range scores {
		if score.AuthID == authID {
			return score.Score
		}
	}
	return 0
}

func readyScoreCount(scores []performance.Score) int {
	count := 0
	for _, score := range scores {
		if score.SampleReady {
			count++
		}
	}
	return count
}

func (m *modelScheduler) readyCountAtPriorityLocked(preferWebsocket bool, priority int) int {
	if m == nil {
		return 0
	}
	m.ensureIndexesFreshLocked()
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return 0
	}
	if preferWebsocket && len(bucket.ws.flat) > 0 {
		return len(bucket.ws.flat)
	}
	return len(bucket.all.flat)
}

func (m *modelScheduler) selectionSnapshotLocked(modelKey string, limit int) string {
	if m == nil || len(m.entries) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = 5
	}
	ordered := make([]*scheduledAuth, 0, len(m.entries))
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil {
			continue
		}
		ordered = append(ordered, entry)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return authPreferredBefore(ordered[i].auth, ordered[j].auth)
	})
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	now := time.Now()
	parts := make([]string, 0, len(ordered))
	for _, entry := range ordered {
		snapshot := scheduledAuthSnapshot(entry, modelKey, now)
		if snapshot == "" {
			continue
		}
		parts = append(parts, snapshot)
	}
	return strings.Join(parts, " | ")
}

func scheduledAuthSnapshot(entry *scheduledAuth, modelKey string, now time.Time) string {
	if entry == nil || entry.auth == nil {
		return ""
	}
	auth := entry.auth
	modelState := selectedModelState(auth, modelKey)
	planType := ""
	if auth.Attributes != nil {
		planType = strings.TrimSpace(auth.Attributes["plan_type"])
	}
	quotaBackoff := auth.Quota.BackoffLevel
	if modelState != nil {
		quotaBackoff = modelState.Quota.BackoffLevel
	}
	blocked, _, nextRetryAfter := isAuthBlockedForModel(auth, modelKey, now)
	createdAt := ""
	if !auth.CreatedAt.IsZero() {
		createdAt = auth.CreatedAt.UTC().Format(time.RFC3339)
	}
	nextRetry := ""
	if !nextRetryAfter.IsZero() {
		nextRetry = nextRetryAfter.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"auth_id=%s priority=%d plan_type=%s quota_backoff=%d created_at=%s unavailable=%t next_retry_after=%s",
		auth.ID,
		authPriority(auth),
		planType,
		quotaBackoff,
		createdAt,
		blocked,
		nextRetry,
	)
}

func selectedModelState(auth *Auth, modelKey string) *ModelState {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	if state := auth.ModelStates[modelKey]; state != nil {
		return state
	}
	baseModelKey := canonicalModelKey(modelKey)
	if baseModelKey == "" || baseModelKey == modelKey {
		return nil
	}
	return auth.ModelStates[baseModelKey]
}

// unavailableErrorLocked returns the correct unavailable or cooldown error for the shard.
func (m *modelScheduler) unavailableErrorLocked(provider, model string, predicate func(*scheduledAuth) bool) error {
	now := time.Now()
	total, cooldownCount, earliest := m.availabilitySummaryLocked(predicate)
	if total == 0 {
		return &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if cooldownCount == total && !earliest.IsZero() {
		providerForError := provider
		if providerForError == "mixed" {
			providerForError = ""
		}
		resetIn := earliest.Sub(now)
		if resetIn < 0 {
			resetIn = 0
		}
		return newModelCooldownError(model, providerForError, resetIn)
	}
	return &Error{Code: "auth_unavailable", Message: "no auth available"}
}

// availabilitySummaryLocked summarizes total candidates, cooldown count, and earliest retry time.
func (m *modelScheduler) availabilitySummaryLocked(predicate func(*scheduledAuth) bool) (int, int, time.Time) {
	if m == nil {
		return 0, 0, time.Time{}
	}
	total := 0
	cooldownCount := 0
	earliest := time.Time{}
	now := time.Now()
	for _, entry := range m.entries {
		if predicate != nil && !predicate(entry) {
			continue
		}
		total++
		if entry == nil || entry.auth == nil {
			continue
		}
		blocked, reason, nextRetry := isAuthBlockedForModel(entry.auth, m.modelKey, now)
		if !blocked || reason != blockReasonCooldown {
			continue
		}
		cooldownCount++
		if !nextRetry.IsZero() && (earliest.IsZero() || nextRetry.Before(earliest)) {
			earliest = nextRetry
		}
	}
	return total, cooldownCount, earliest
}

// rebuildIndexesLocked reconstructs ready and blocked views from the current entry map.
func (m *modelScheduler) rebuildIndexesLocked() {
	cursorStates := make(map[int]readyBucketCursorState, len(m.readyByPriority))
	for priority, bucket := range m.readyByPriority {
		if bucket == nil {
			continue
		}
		cursorStates[priority] = readyBucketCursorState{
			all: snapshotReadyViewCursors(bucket.all),
			ws:  snapshotReadyViewCursors(bucket.ws),
		}
	}

	m.readyByPriority = make(map[int]*readyBucket)
	m.priorityOrder = m.priorityOrder[:0]
	priorityBuckets := make(map[int][]*scheduledAuth)
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.meta == nil {
			continue
		}
		if !entry.state.ready() {
			continue
		}
		priority := entry.meta.priority
		priorityBuckets[priority] = append(priorityBuckets[priority], entry)
	}
	for priority, entries := range priorityBuckets {
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i] == nil || entries[j] == nil {
				return entries[i] != nil
			}
			return authPreferredBefore(entries[i].auth, entries[j].auth)
		})
		bucket := buildReadyBucket(entries)
		if cursorState, ok := cursorStates[priority]; ok && bucket != nil {
			restoreReadyViewCursors(&bucket.all, cursorState.all)
			restoreReadyViewCursors(&bucket.ws, cursorState.ws)
		}
		m.readyByPriority[priority] = bucket
		m.priorityOrder = append(m.priorityOrder, priority)
	}
	sort.Slice(m.priorityOrder, func(i, j int) bool {
		return m.priorityOrder[i] > m.priorityOrder[j]
	})
	m.rebuildCooldownQueueLocked()
	m.dirty = false
	m.forceRebuild = false
	clear(m.pendingUpserts)
	clear(m.pendingRemovals)
}

func (m *modelScheduler) ensureIndexesFreshLocked() {
	if m == nil || !m.dirty {
		return
	}
	if m.canApplyPendingIncrementalLocked() {
		m.applyPendingIncrementalLocked()
		return
	}
	m.rebuildIndexesLocked()
}

func (m *modelScheduler) canApplyPendingIncrementalLocked() bool {
	if m == nil || m.forceRebuild {
		return false
	}
	for _, removal := range m.pendingRemovals {
		if removal.virtualParentName != "" {
			return false
		}
		bucket := m.readyByPriority[removal.priority]
		if bucket == nil {
			continue
		}
		if len(bucket.all.children) > 0 || len(bucket.ws.children) > 0 {
			return false
		}
	}
	for _, entry := range m.pendingUpserts {
		if entry == nil || entry.auth == nil || entry.meta == nil {
			return false
		}
		if entry.meta.virtualParent != "" {
			return false
		}
		bucket := m.readyByPriority[entry.meta.priority]
		if bucket == nil {
			continue
		}
		if len(bucket.all.children) > 0 || len(bucket.ws.children) > 0 {
			return false
		}
	}
	return len(m.pendingRemovals) > 0 || len(m.pendingUpserts) > 0
}

func (m *modelScheduler) applyPendingIncrementalLocked() {
	if m == nil {
		return
	}
	for _, removal := range m.pendingRemovals {
		m.applyPendingRemovalLocked(removal)
	}
	for _, entry := range m.pendingUpserts {
		m.applyPendingUpsertLocked(entry)
	}
	sort.Slice(m.priorityOrder, func(i, j int) bool {
		return m.priorityOrder[i] > m.priorityOrder[j]
	})
	m.rebuildCooldownQueueLocked()
	m.dirty = false
	m.forceRebuild = false
	clear(m.pendingUpserts)
	clear(m.pendingRemovals)
}

func (m *modelScheduler) applyPendingRemovalLocked(removal pendingStructuralRemoval) {
	if m == nil || removal.authID == "" {
		return
	}
	bucket := m.readyByPriority[removal.priority]
	if bucket == nil {
		return
	}
	bucket.all = removeAuthFromReadyView(bucket.all, removal.authID)
	if removal.websocketEnabled {
		bucket.ws = removeAuthFromReadyView(bucket.ws, removal.authID)
	}
	if len(bucket.all.flat) == 0 {
		delete(m.readyByPriority, removal.priority)
		m.priorityOrder = removePriorityValue(m.priorityOrder, removal.priority)
	}
}

func (m *modelScheduler) applyPendingUpsertLocked(entry *scheduledAuth) {
	if m == nil || entry == nil || entry.auth == nil || entry.meta == nil {
		return
	}
	if !entry.state.ready() {
		return
	}
	priority := entry.meta.priority
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		bucket = &readyBucket{}
		m.readyByPriority[priority] = bucket
		m.priorityOrder = append(m.priorityOrder, priority)
	}
	m.insertEntryIntoReadyIndexesLocked(entry)
}

func insertAuthIntoReadyView(view readyView, entry *scheduledAuth) readyView {
	if entry == nil || entry.auth == nil {
		return view
	}
	insertAt := len(view.flat)
	for index, existing := range view.flat {
		if existing == nil || existing.auth == nil {
			insertAt = index
			break
		}
		if authPreferredBefore(entry.auth, existing.auth) {
			insertAt = index
			break
		}
	}
	view.flat = append(view.flat, nil)
	copy(view.flat[insertAt+1:], view.flat[insertAt:])
	view.flat[insertAt] = entry
	if insertAt < view.cursor {
		view.cursor++
	}
	view.cursor = normalizeCursor(view.cursor, len(view.flat))
	return view
}

func removeAuthFromReadyView(view readyView, authID string) readyView {
	if authID == "" || len(view.flat) == 0 {
		return view
	}
	removeAt := -1
	for index, entry := range view.flat {
		if entry == nil || entry.auth == nil {
			continue
		}
		if entry.auth.ID == authID {
			removeAt = index
			break
		}
	}
	if removeAt < 0 {
		return view
	}
	copy(view.flat[removeAt:], view.flat[removeAt+1:])
	view.flat[len(view.flat)-1] = nil
	view.flat = view.flat[:len(view.flat)-1]
	if removeAt < view.cursor {
		view.cursor--
	}
	view.cursor = normalizeCursor(view.cursor, len(view.flat))
	return view
}

func removePriorityValue(values []int, target int) []int {
	for index, value := range values {
		if value != target {
			continue
		}
		return append(values[:index], values[index+1:]...)
	}
	return values
}

func insertPriorityValueDesc(values []int, target int) []int {
	for _, value := range values {
		if value == target {
			return values
		}
	}
	insertAt := len(values)
	for index, value := range values {
		if target > value {
			insertAt = index
			break
		}
	}
	values = append(values, 0)
	copy(values[insertAt+1:], values[insertAt:])
	values[insertAt] = target
	return values
}

func (m *modelScheduler) canApplyReadyViewMutationLocked(meta *scheduledAuthMeta) bool {
	if m == nil || meta == nil || meta.virtualParent != "" {
		return false
	}
	bucket := m.readyByPriority[meta.priority]
	if bucket == nil {
		return true
	}
	return len(bucket.all.children) == 0 && len(bucket.ws.children) == 0
}

func (m *modelScheduler) insertEntryIntoReadyIndexesLocked(entry *scheduledAuth) {
	if m == nil || entry == nil || entry.auth == nil || entry.meta == nil || !entry.state.ready() {
		return
	}
	priority := entry.meta.priority
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		bucket = &readyBucket{}
		m.readyByPriority[priority] = bucket
		m.priorityOrder = insertPriorityValueDesc(m.priorityOrder, priority)
	}
	bucket.all = insertAuthIntoReadyView(bucket.all, entry)
	if entry.meta.websocketEnabled {
		bucket.ws = insertAuthIntoReadyView(bucket.ws, entry)
	}
}

func (m *modelScheduler) removeEntryFromReadyIndexesLocked(authID string, priority int, websocketEnabled bool) {
	if m == nil || authID == "" {
		return
	}
	bucket := m.readyByPriority[priority]
	if bucket == nil {
		return
	}
	bucket.all = removeAuthFromReadyView(bucket.all, authID)
	if websocketEnabled {
		bucket.ws = removeAuthFromReadyView(bucket.ws, authID)
	}
	if len(bucket.all.flat) == 0 {
		delete(m.readyByPriority, priority)
		m.priorityOrder = removePriorityValue(m.priorityOrder, priority)
	}
}

func (m *modelScheduler) applyNonStructuralTransitionLocked(entry *scheduledAuth, previousAuth *Auth, previousState scheduledState, previousNextRetryAt time.Time) bool {
	if m == nil || entry == nil || entry.auth == nil || entry.meta == nil {
		return false
	}
	if !m.canApplyReadyViewMutationLocked(entry.meta) {
		return false
	}

	currentState := entry.state
	previousReady := previousState.ready()
	currentReady := currentState.ready()

	switch {
	case previousReady && currentReady:
		if !authOrderKeyEqual(previousAuth, entry.auth) {
			m.removeEntryFromReadyIndexesLocked(entry.auth.ID, entry.meta.priority, entry.meta.websocketEnabled)
			m.insertEntryIntoReadyIndexesLocked(entry)
		}
	case previousReady && !currentReady:
		m.removeEntryFromReadyIndexesLocked(entry.auth.ID, entry.meta.priority, entry.meta.websocketEnabled)
	case !previousReady && currentReady:
		m.insertEntryIntoReadyIndexesLocked(entry)
	}

	if previousState == scheduledStateCooldown || !previousNextRetryAt.IsZero() {
		m.removeCooldownEntryLocked(entry.auth.ID)
	}
	if currentState == scheduledStateCooldown {
		m.upsertCooldownEntryLocked(entry)
	}
	return true
}

func authOrderKeyEqual(previous *Auth, next *Auth) bool {
	if previous == nil || next == nil {
		return previous == next
	}
	return authPriority(previous) == authPriority(next) &&
		authPlanTierScore(previous) == authPlanTierScore(next) &&
		authCyberPolicyTriggerCount(previous) == authCyberPolicyTriggerCount(next) &&
		authQuotaHealthScore(previous) == authQuotaHealthScore(next) &&
		authCreatedAtUnix(previous) == authCreatedAtUnix(next) &&
		previous.ID == next.ID
}

func (m *modelScheduler) rebuildCooldownQueueLocked() {
	if m == nil {
		return
	}
	m.blocked = m.blocked[:0]
	for _, entry := range m.entries {
		if entry == nil || entry.auth == nil || entry.state != scheduledStateCooldown {
			continue
		}
		m.blocked = append(m.blocked, entry)
	}
	sort.SliceStable(m.blocked, func(i, j int) bool {
		left := m.blocked[i]
		right := m.blocked[j]
		if left == nil || right == nil || left.auth == nil || right.auth == nil {
			return left != nil && left.auth != nil
		}
		if !left.nextRetryAt.Equal(right.nextRetryAt) {
			if left.nextRetryAt.IsZero() {
				return true
			}
			if right.nextRetryAt.IsZero() {
				return false
			}
			return left.nextRetryAt.Before(right.nextRetryAt)
		}
		return left.auth.ID < right.auth.ID
	})
}

func (m *modelScheduler) removeCooldownEntryLocked(authID string) {
	if m == nil || authID == "" || len(m.blocked) == 0 {
		return
	}
	for index, entry := range m.blocked {
		if entry == nil || entry.auth == nil || entry.auth.ID != authID {
			continue
		}
		copy(m.blocked[index:], m.blocked[index+1:])
		m.blocked[len(m.blocked)-1] = nil
		m.blocked = m.blocked[:len(m.blocked)-1]
		return
	}
}

func (m *modelScheduler) upsertCooldownEntryLocked(entry *scheduledAuth) {
	if m == nil || entry == nil || entry.auth == nil || entry.state != scheduledStateCooldown {
		return
	}
	m.removeCooldownEntryLocked(entry.auth.ID)
	insertAt := len(m.blocked)
	for index, existing := range m.blocked {
		if existing == nil || existing.auth == nil {
			insertAt = index
			break
		}
		if entry.nextRetryAt.Before(existing.nextRetryAt) || (entry.nextRetryAt.Equal(existing.nextRetryAt) && entry.auth.ID < existing.auth.ID) {
			insertAt = index
			break
		}
	}
	m.blocked = append(m.blocked, nil)
	copy(m.blocked[insertAt+1:], m.blocked[insertAt:])
	m.blocked[insertAt] = entry
}

// buildReadyBucket prepares the general and websocket-only ready views for one priority bucket.
func buildReadyBucket(entries []*scheduledAuth) *readyBucket {
	bucket := &readyBucket{}
	bucket.all = buildReadyView(entries)
	wsEntries := make([]*scheduledAuth, 0, len(entries))
	for _, entry := range entries {
		if entry != nil && entry.meta != nil && entry.meta.websocketEnabled {
			wsEntries = append(wsEntries, entry)
		}
	}
	bucket.ws = buildReadyView(wsEntries)
	return bucket
}

// buildReadyView creates either a flat view or a grouped parent/child view for rotation.
func buildReadyView(entries []*scheduledAuth) readyView {
	view := readyView{flat: append([]*scheduledAuth(nil), entries...)}
	if len(entries) == 0 {
		return view
	}
	groups := make(map[string][]*scheduledAuth)
	for _, entry := range entries {
		if entry == nil || entry.meta == nil || entry.meta.virtualParent == "" {
			return view
		}
		groups[entry.meta.virtualParent] = append(groups[entry.meta.virtualParent], entry)
	}
	if len(groups) <= 1 {
		return view
	}
	view.children = make(map[string]*childBucket, len(groups))
	view.parentOrder = make([]string, 0, len(groups))
	for parent := range groups {
		view.parentOrder = append(view.parentOrder, parent)
	}
	for parent, items := range groups {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i] == nil || items[j] == nil {
				return items[i] != nil
			}
			return authPreferredBefore(items[i].auth, items[j].auth)
		})
		groups[parent] = items
	}
	sort.SliceStable(view.parentOrder, func(i, j int) bool {
		leftItems := groups[view.parentOrder[i]]
		rightItems := groups[view.parentOrder[j]]
		if len(leftItems) == 0 || len(rightItems) == 0 {
			return view.parentOrder[i] < view.parentOrder[j]
		}
		if authPreferredBefore(leftItems[0].auth, rightItems[0].auth) {
			return true
		}
		if authPreferredBefore(rightItems[0].auth, leftItems[0].auth) {
			return false
		}
		return view.parentOrder[i] < view.parentOrder[j]
	})
	for _, parent := range view.parentOrder {
		view.children[parent] = &childBucket{items: append([]*scheduledAuth(nil), groups[parent]...)}
	}
	return view
}

// pickFirst returns the first ready entry that satisfies predicate without advancing cursors.
func (v *readyView) pickFirst(predicate func(*scheduledAuth) bool) *scheduledAuth {
	for _, entry := range v.flat {
		if predicate == nil || predicate(entry) {
			return entry
		}
	}
	return nil
}

// pickRoundRobin returns the next ready entry using flat or grouped round-robin traversal.
func inflightLoadValue(load func(*scheduledAuth) int, entry *scheduledAuth) int {
	if load == nil {
		return 0
	}
	value := load(entry)
	if value < 0 {
		return 0
	}
	return value
}

func (v *readyView) pickRoundRobin(predicate func(*scheduledAuth) bool, load func(*scheduledAuth) int) *scheduledAuth {
	if len(v.parentOrder) > 1 && len(v.children) > 0 {
		return v.pickGroupedRoundRobin(predicate, load)
	}
	if len(v.flat) == 0 {
		return nil
	}
	start := 0
	if len(v.flat) > 0 {
		start = v.cursor % len(v.flat)
	}
	window := len(v.flat)
	if len(v.flat) > defaultRoundRobinFullScanThreshold {
		window = defaultRoundRobinCandidateWindow
	}
	if window <= 0 || window > len(v.flat) {
		window = len(v.flat)
	}
	for scanned := 0; scanned < len(v.flat); {
		bestIndex := -1
		bestLoad := 0
		segment := window
		if remaining := len(v.flat) - scanned; segment > remaining {
			segment = remaining
		}
		for offset := 0; offset < segment; offset++ {
			index := (start + scanned + offset) % len(v.flat)
			entry := v.flat[index]
			if predicate != nil && !predicate(entry) {
				continue
			}
			entryLoad := inflightLoadValue(load, entry)
			if bestIndex >= 0 && entryLoad >= bestLoad {
				continue
			}
			bestIndex = index
			bestLoad = entryLoad
			if bestLoad == 0 {
				break
			}
		}
		if bestIndex >= 0 {
			v.cursor = bestIndex + 1
			return v.flat[bestIndex]
		}
		scanned += segment
	}
	return nil
}

// pickGroupedRoundRobin rotates across parents first and then within the selected parent.
func (v *readyView) pickGroupedRoundRobin(predicate func(*scheduledAuth) bool, load func(*scheduledAuth) int) *scheduledAuth {
	start := 0
	if len(v.parentOrder) > 0 {
		start = v.parentCursor % len(v.parentOrder)
	}
	bestParentIndex := -1
	bestItemIndex := -1
	bestLoad := 0
	var bestEntry *scheduledAuth
	for offset := 0; offset < len(v.parentOrder); offset++ {
		parentIndex := (start + offset) % len(v.parentOrder)
		parent := v.parentOrder[parentIndex]
		child := v.children[parent]
		if child == nil || len(child.items) == 0 {
			continue
		}
		itemStart := child.cursor % len(child.items)
		for itemOffset := 0; itemOffset < len(child.items); itemOffset++ {
			itemIndex := (itemStart + itemOffset) % len(child.items)
			entry := child.items[itemIndex]
			if predicate != nil && !predicate(entry) {
				continue
			}
			entryLoad := inflightLoadValue(load, entry)
			if bestEntry != nil && entryLoad >= bestLoad {
				continue
			}
			bestEntry = entry
			bestLoad = entryLoad
			bestParentIndex = parentIndex
			bestItemIndex = itemIndex
			if bestLoad == 0 {
				break
			}
		}
		if bestEntry != nil && bestLoad == 0 {
			break
		}
	}
	if bestEntry == nil || bestParentIndex < 0 || bestItemIndex < 0 {
		return nil
	}
	parent := v.parentOrder[bestParentIndex]
	child := v.children[parent]
	if child != nil {
		child.cursor = bestItemIndex + 1
	}
	v.parentCursor = bestParentIndex + 1
	return bestEntry
}
