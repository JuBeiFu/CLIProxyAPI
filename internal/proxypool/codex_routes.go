package proxypool

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexroute"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type RouteState int

const (
	RouteStateUnknown RouteState = iota
	RouteStateStandby
	RouteStatePrimary
	RouteStateCooling
	RouteStateRecovering
	RouteStateQuarantined
)

type RouteDescriptor = codexroute.RouteDescriptor

type RoutePassiveOutcome struct {
	CheckedAt  time.Time
	FirstByte  time.Duration
	Successful bool
	Error      string
}

type CodexRouteConfig struct {
	CoolingFirstByteThreshold    time.Duration
	PromotionWinThreshold        int
	QuarantineFirstByteThreshold time.Duration
}

type AuthRoutePlan = codexroute.AuthRoutePlan

type codexRouteEntry struct {
	route       RouteDescriptor
	state       RouteState
	hedgeWins   int
	lastChecked time.Time
	lastError   string
}

type codexAuthRoutes struct {
	primaryKey string
	standbyKey string
	routes     map[string]*codexRouteEntry
}

type CodexRouteRegistry struct {
	mu    sync.RWMutex
	cfg   CodexRouteConfig
	auths map[string]*codexAuthRoutes
}

var defaultCodexRouteRegistry = NewCodexRouteRegistry(CodexRouteConfig{})

func NewCodexRouteRegistry(cfg CodexRouteConfig) *CodexRouteRegistry {
	cfg = normalizeCodexRouteConfig(cfg)
	return &CodexRouteRegistry{
		cfg:   cfg,
		auths: make(map[string]*codexAuthRoutes),
	}
}

func normalizeCodexRouteConfig(cfg CodexRouteConfig) CodexRouteConfig {
	if cfg.PromotionWinThreshold <= 0 {
		cfg.PromotionWinThreshold = internalconfig.DefaultCodexRoutePromotionWinThreshold
	}
	if cfg.CoolingFirstByteThreshold <= 0 {
		cfg.CoolingFirstByteThreshold = time.Duration(internalconfig.DefaultCodexRouteCoolingFirstByteThresholdSecs) * time.Second
	}
	if cfg.QuarantineFirstByteThreshold <= 0 {
		cfg.QuarantineFirstByteThreshold = time.Duration(internalconfig.DefaultCodexRouteQuarantineThresholdSecs) * time.Second
	}
	return cfg
}

func (r *CodexRouteRegistry) UpdateConfig(cfg CodexRouteConfig) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = normalizeCodexRouteConfig(cfg)
}

func CodexRouteConfigFromRuntimeConfig(cfg *internalconfig.Config) CodexRouteConfig {
	if cfg == nil {
		return CodexRouteConfig{}
	}
	return CodexRouteConfig{
		CoolingFirstByteThreshold:    cfg.CodexRouteManagement.CoolingFirstByteThreshold,
		PromotionWinThreshold:        cfg.CodexRouteManagement.PromotionWinThreshold,
		QuarantineFirstByteThreshold: cfg.CodexRouteManagement.QuarantineFirstByteThreshold,
	}
}

func DefaultCodexRouteRegistry() *CodexRouteRegistry {
	return defaultCodexRouteRegistry
}

func SetDefaultCodexRouteRegistry(reg *CodexRouteRegistry) {
	if reg == nil {
		return
	}
	defaultCodexRouteRegistry = reg
}

func (r *CodexRouteRegistry) UpsertCertifiedRoute(authID string, route RouteDescriptor, checkedAt time.Time) {
	if r == nil {
		return
	}
	authKey := normalizeRouteAuthID(authID)
	routeKey := routeDescriptorKey(route)
	if authKey == "" || routeKey == "" {
		return
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	authRoutes := r.authRoutesLocked(authKey)
	entry := authRoutes.routes[routeKey]
	if entry == nil {
		entry = &codexRouteEntry{route: route}
		authRoutes.routes[routeKey] = entry
	}
	entry.route = route
	entry.lastChecked = checkedAt
	entry.lastError = ""
	switch {
	case authRoutes.primaryKey == "":
		authRoutes.primaryKey = routeKey
		entry.state = RouteStatePrimary
	case authRoutes.primaryKey == routeKey:
		entry.state = RouteStatePrimary
	case authRoutes.standbyKey == "" || authRoutes.standbyKey == routeKey:
		authRoutes.standbyKey = routeKey
		entry.state = RouteStateStandby
	case entry.state == RouteStateQuarantined:
		// Keep quarantine until a later recovery path explicitly changes it.
	default:
		entry.state = RouteStateStandby
	}
}

func (r *CodexRouteRegistry) MarkHedgeWinner(authID string, route RouteDescriptor, checkedAt time.Time) {
	if r == nil {
		return
	}
	authKey := normalizeRouteAuthID(authID)
	routeKey := routeDescriptorKey(route)
	if authKey == "" || routeKey == "" {
		return
	}
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	authRoutes := r.authRoutesLocked(authKey)
	entry := authRoutes.routes[routeKey]
	if entry == nil {
		entry = &codexRouteEntry{route: route, state: RouteStateStandby}
		authRoutes.routes[routeKey] = entry
	}
	entry.route = route
	entry.lastChecked = checkedAt
	entry.hedgeWins++
	if authRoutes.primaryKey == "" {
		authRoutes.primaryKey = routeKey
		entry.state = RouteStatePrimary
		entry.hedgeWins = 0
		return
	}
	if authRoutes.primaryKey == routeKey {
		entry.state = RouteStatePrimary
		entry.hedgeWins = 0
		return
	}
	if entry.hedgeWins < r.cfg.PromotionWinThreshold {
		if authRoutes.standbyKey == "" {
			authRoutes.standbyKey = routeKey
		}
		entry.state = RouteStateStandby
		return
	}

	previousPrimaryKey := authRoutes.primaryKey
	authRoutes.primaryKey = routeKey
	authRoutes.standbyKey = previousPrimaryKey
	entry.state = RouteStatePrimary
	entry.hedgeWins = 0
	if prev := authRoutes.routes[previousPrimaryKey]; prev != nil {
		prev.state = RouteStateStandby
		prev.hedgeWins = 0
	}
}

func (r *CodexRouteRegistry) PrimaryAndStandby(authID string) (RouteDescriptor, RouteDescriptor, bool) {
	if r == nil {
		return RouteDescriptor{}, RouteDescriptor{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	authRoutes, ok := r.auths[normalizeRouteAuthID(authID)]
	if !ok || authRoutes == nil {
		return RouteDescriptor{}, RouteDescriptor{}, false
	}
	primary := authRoutes.routes[authRoutes.primaryKey]
	standby := authRoutes.routes[authRoutes.standbyKey]
	if primary == nil || standby == nil {
		return RouteDescriptor{}, RouteDescriptor{}, false
	}
	return primary.route, standby.route, true
}

func (r *CodexRouteRegistry) RoutePlan(authID string) (AuthRoutePlan, bool) {
	primary, standby, ok := r.PrimaryAndStandby(authID)
	if !ok {
		return AuthRoutePlan{}, false
	}
	return AuthRoutePlan{Primary: primary, Standby: standby}, true
}

func (r *CodexRouteRegistry) RecordPassiveOutcome(authID string, route RouteDescriptor, outcome RoutePassiveOutcome) {
	if r == nil {
		return
	}
	authKey := normalizeRouteAuthID(authID)
	routeKey := routeDescriptorKey(route)
	if authKey == "" || routeKey == "" {
		return
	}
	if outcome.CheckedAt.IsZero() {
		outcome.CheckedAt = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	authRoutes := r.authRoutesLocked(authKey)
	entry := authRoutes.routes[routeKey]
	if entry == nil {
		entry = &codexRouteEntry{route: route}
		authRoutes.routes[routeKey] = entry
	}
	entry.route = route
	entry.lastChecked = outcome.CheckedAt
	entry.lastError = strings.TrimSpace(outcome.Error)
	if outcome.FirstByte >= r.cfg.QuarantineFirstByteThreshold {
		r.demoteRouteLocked(authRoutes, routeKey, RouteStateQuarantined)
		return
	}
	if outcome.FirstByte >= r.cfg.CoolingFirstByteThreshold || (!outcome.Successful && entry.lastError != "") {
		r.demoteRouteLocked(authRoutes, routeKey, RouteStateCooling)
		return
	}
	if authRoutes.primaryKey == routeKey {
		entry.state = RouteStatePrimary
		return
	}
	if authRoutes.standbyKey == routeKey {
		entry.state = RouteStateStandby
		return
	}
	if entry.state == RouteStateUnknown {
		entry.state = RouteStateStandby
	}
}

func (r *CodexRouteRegistry) RouteState(authID string, route RouteDescriptor) RouteState {
	if r == nil {
		return RouteStateUnknown
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	authRoutes, ok := r.auths[normalizeRouteAuthID(authID)]
	if !ok || authRoutes == nil {
		return RouteStateUnknown
	}
	if entry := authRoutes.routes[routeDescriptorKey(route)]; entry != nil {
		return entry.state
	}
	return RouteStateUnknown
}

func (r *CodexRouteRegistry) ApplyProbeOutcome(authID string, route RouteDescriptor, outcome ProbeOutcome) {
	if r == nil {
		return
	}
	authKey := normalizeRouteAuthID(authID)
	routeKey := routeDescriptorKey(route)
	if authKey == "" || routeKey == "" {
		return
	}
	if outcome.CheckedAt.IsZero() {
		outcome.CheckedAt = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	authRoutes := r.authRoutesLocked(authKey)
	entry := authRoutes.routes[routeKey]
	if entry == nil {
		entry = &codexRouteEntry{route: route, state: RouteStateUnknown}
		authRoutes.routes[routeKey] = entry
	}
	entry.route = route
	entry.lastChecked = outcome.CheckedAt
	entry.lastError = strings.TrimSpace(outcome.TerminalReason)
	if !outcome.RouteConsistent {
		r.demoteRouteLocked(authRoutes, routeKey, RouteStateQuarantined)
		return
	}
	if authRoutes.primaryKey == routeKey {
		entry.state = RouteStatePrimary
		entry.lastError = ""
		return
	}
	authRoutes.standbyKey = routeKey
	entry.state = RouteStateStandby
	entry.lastError = ""
}

func (r *CodexRouteRegistry) authRoutesLocked(authID string) *codexAuthRoutes {
	authRoutes := r.auths[authID]
	if authRoutes == nil {
		authRoutes = &codexAuthRoutes{routes: make(map[string]*codexRouteEntry)}
		r.auths[authID] = authRoutes
	}
	return authRoutes
}

func (r *CodexRouteRegistry) demoteRouteLocked(authRoutes *codexAuthRoutes, routeKey string, state RouteState) {
	if authRoutes == nil {
		return
	}
	entry := authRoutes.routes[routeKey]
	if entry == nil {
		return
	}
	entry.state = state
	if authRoutes.primaryKey == routeKey {
		authRoutes.primaryKey = ""
	}
	if authRoutes.standbyKey == routeKey {
		authRoutes.standbyKey = ""
	}
	r.repairRouteAssignmentsLocked(authRoutes)
}

func (r *CodexRouteRegistry) repairRouteAssignmentsLocked(authRoutes *codexAuthRoutes) {
	if authRoutes == nil {
		return
	}
	if authRoutes.primaryKey == "" {
		if routeAssignableEntry(authRoutes.routes[authRoutes.standbyKey]) {
			authRoutes.primaryKey = authRoutes.standbyKey
			if entry := authRoutes.routes[authRoutes.primaryKey]; entry != nil {
				entry.state = RouteStatePrimary
			}
		} else if candidate := nextAssignableRouteKey(authRoutes, ""); candidate != "" {
			authRoutes.primaryKey = candidate
			if entry := authRoutes.routes[candidate]; entry != nil {
				entry.state = RouteStatePrimary
			}
		}
	}
	if authRoutes.standbyKey == authRoutes.primaryKey || !routeAssignableEntry(authRoutes.routes[authRoutes.standbyKey]) {
		authRoutes.standbyKey = ""
	}
	if authRoutes.standbyKey == "" {
		if candidate := nextAssignableRouteKey(authRoutes, authRoutes.primaryKey); candidate != "" {
			authRoutes.standbyKey = candidate
			if candidate != authRoutes.primaryKey {
				if entry := authRoutes.routes[candidate]; entry != nil {
					entry.state = RouteStateStandby
				}
			}
		}
	}
}

func nextAssignableRouteKey(authRoutes *codexAuthRoutes, exclude string) string {
	candidates := make([]string, 0, len(authRoutes.routes))
	for key, entry := range authRoutes.routes {
		if key == exclude || !routeAssignableEntry(entry) {
			continue
		}
		candidates = append(candidates, key)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left := authRoutes.routes[candidates[i]]
		right := authRoutes.routes[candidates[j]]
		leftRank := routeAssignmentRank(left)
		rightRank := routeAssignmentRank(right)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if !left.lastChecked.Equal(right.lastChecked) {
			return left.lastChecked.After(right.lastChecked)
		}
		return candidates[i] < candidates[j]
	})
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func routeAssignableEntry(entry *codexRouteEntry) bool {
	if entry == nil {
		return false
	}
	return entry.state != RouteStateCooling && entry.state != RouteStateQuarantined
}

func routeAssignmentRank(entry *codexRouteEntry) int {
	if entry == nil {
		return 99
	}
	switch entry.state {
	case RouteStatePrimary, RouteStateStandby:
		return 0
	case RouteStateRecovering:
		return 1
	case RouteStateUnknown:
		return 2
	default:
		return 3
	}
}

func routeDescriptorKey(r RouteDescriptor) string {
	if r.Direct {
		return normalizeRoutePart(r.Pool) + "|direct"
	}
	if normalizeRoutePart(r.Pool) == "" || normalizeRoutePart(r.Entry) == "" {
		return ""
	}
	return normalizeRoutePart(r.Pool) + "|" + normalizeRoutePart(r.Entry)
}

func normalizeRouteAuthID(authID string) string {
	return strings.TrimSpace(authID)
}

func normalizeRoutePart(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
