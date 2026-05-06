package proxypool

import (
	"strings"
	"sync"
	"time"

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

type RouteDescriptor struct {
	Pool   string
	Entry  string
	Direct bool
}

type RoutePassiveOutcome struct {
	CheckedAt  time.Time
	FirstByte  time.Duration
	Successful bool
	Error      string
}

type CodexRouteConfig struct {
	PromotionWinThreshold        int
	QuarantineFirstByteThreshold time.Duration
}

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
	if cfg.PromotionWinThreshold <= 0 {
		cfg.PromotionWinThreshold = internalconfig.DefaultCodexRoutePromotionWinThreshold
	}
	if cfg.QuarantineFirstByteThreshold <= 0 {
		cfg.QuarantineFirstByteThreshold = time.Duration(internalconfig.DefaultCodexRouteQuarantineThresholdSecs) * time.Second
	}
	return &CodexRouteRegistry{
		cfg:   cfg,
		auths: make(map[string]*codexAuthRoutes),
	}
}

func CodexRouteConfigFromRuntimeConfig(cfg *internalconfig.Config) CodexRouteConfig {
	if cfg == nil {
		return CodexRouteConfig{}
	}
	return CodexRouteConfig{
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
	routeKey := route.key()
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
	routeKey := route.key()
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

func (r *CodexRouteRegistry) RecordPassiveOutcome(authID string, route RouteDescriptor, outcome RoutePassiveOutcome) {
	if r == nil {
		return
	}
	authKey := normalizeRouteAuthID(authID)
	routeKey := route.key()
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
		entry.state = RouteStateQuarantined
		if authRoutes.primaryKey == routeKey {
			authRoutes.primaryKey = ""
		}
		if authRoutes.standbyKey == routeKey {
			authRoutes.standbyKey = ""
		}
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
	if entry := authRoutes.routes[route.key()]; entry != nil {
		return entry.state
	}
	return RouteStateUnknown
}

func (r *CodexRouteRegistry) ApplyProbeOutcome(authID string, route RouteDescriptor, outcome ProbeOutcome) {
	if r == nil {
		return
	}
	authKey := normalizeRouteAuthID(authID)
	routeKey := route.key()
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
		entry.state = RouteStateQuarantined
		if authRoutes.primaryKey == routeKey {
			authRoutes.primaryKey = ""
		}
		if authRoutes.standbyKey == routeKey {
			authRoutes.standbyKey = ""
		}
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

func (r RouteDescriptor) key() string {
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
