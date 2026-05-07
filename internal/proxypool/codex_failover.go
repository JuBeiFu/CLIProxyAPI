package proxypool

import (
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	CodexFailoverModeDirectV4 = "direct-v4"
	CodexFailoverModeDirectV6 = "direct-v6"
	CodexFailoverModeProxy    = "proxy-pool"
)

type codexFailoverState struct {
	mode        string
	reason      string
	preferUntil time.Time
	lease       coreauth.IPv6BindLeaseInfo
}

type CodexFailoverManager struct {
	mu    sync.RWMutex
	auths map[string]codexFailoverState
}

var defaultCodexFailoverManager = &CodexFailoverManager{
	auths: make(map[string]codexFailoverState),
}

func DefaultCodexFailoverManager() *CodexFailoverManager {
	return defaultCodexFailoverManager
}

func (m *CodexFailoverManager) PreferredMode(cfg *config.Config, authID string, now time.Time) string {
	if m == nil {
		return CodexFailoverModeDirectV4
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return CodexFailoverModeDirectV4
	}
	if now.IsZero() {
		now = time.Now()
	}

	m.mu.RLock()
	state, ok := m.auths[authID]
	m.mu.RUnlock()
	if !ok {
		return CodexFailoverModeDirectV4
	}
	if !state.preferUntil.IsZero() && now.After(state.preferUntil) {
		m.Clear(authID)
		return CodexFailoverModeDirectV4
	}
	switch state.mode {
	case CodexFailoverModeDirectV6, CodexFailoverModeProxy:
		return state.mode
	default:
		return CodexFailoverModeDirectV4
	}
}

func (m *CodexFailoverManager) PreferDirectV6(cfg *config.Config, authID string, now time.Time) {
	m.setMode(cfg, authID, CodexFailoverModeDirectV6, "", now)
}

func (m *CodexFailoverManager) PreferProxyPool(cfg *config.Config, authID string, now time.Time) {
	m.setMode(cfg, authID, CodexFailoverModeProxy, "", now)
}

func (m *CodexFailoverManager) PreferDirectV6WithReason(cfg *config.Config, authID, reason string, now time.Time) {
	m.setMode(cfg, authID, CodexFailoverModeDirectV6, reason, now)
}

func (m *CodexFailoverManager) PreferProxyPoolWithReason(cfg *config.Config, authID, reason string, now time.Time) {
	m.setMode(cfg, authID, CodexFailoverModeProxy, reason, now)
}

func (m *CodexFailoverManager) MarkSuccess(authID, mode string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if strings.TrimSpace(mode) == CodexFailoverModeDirectV4 {
		m.Clear(authID)
	}
}

func (m *CodexFailoverManager) Clear(authID string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	m.mu.Lock()
	delete(m.auths, authID)
	m.mu.Unlock()
}

func (m *CodexFailoverManager) setMode(cfg *config.Config, authID, mode, reason string, now time.Time) {
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
	hold := codexFailoverHold(cfg)
	m.mu.Lock()
	m.auths[authID] = codexFailoverState{
		mode:        strings.TrimSpace(mode),
		reason:      strings.TrimSpace(reason),
		preferUntil: now.Add(hold),
	}
	m.mu.Unlock()
}

func (m *CodexFailoverManager) NoteReason(authID, reason string) {
	if m == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	m.mu.Lock()
	state, ok := m.auths[authID]
	if ok {
		state.reason = strings.TrimSpace(reason)
		m.auths[authID] = state
	}
	m.mu.Unlock()
}

type CodexFailoverSnapshot struct {
	Mode        string
	Reason      string
	PreferUntil time.Time
	Lease       coreauth.IPv6BindLeaseInfo
}

func (m *CodexFailoverManager) Snapshot(authID string, now time.Time) CodexFailoverSnapshot {
	if m == nil {
		return CodexFailoverSnapshot{}
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return CodexFailoverSnapshot{}
	}
	if now.IsZero() {
		now = time.Now()
	}

	m.mu.RLock()
	state, ok := m.auths[authID]
	m.mu.RUnlock()
	if !ok {
		return CodexFailoverSnapshot{}
	}
	if !state.preferUntil.IsZero() && now.After(state.preferUntil) {
		m.Clear(authID)
		return CodexFailoverSnapshot{}
	}
	return CodexFailoverSnapshot{
		Mode:        strings.TrimSpace(state.mode),
		Reason:      strings.TrimSpace(state.reason),
		PreferUntil: state.preferUntil,
		Lease:       state.lease,
	}
}

func (m *CodexFailoverManager) CurrentIPv6Lease(auth *coreauth.Auth) coreauth.IPv6BindLeaseInfo {
	if auth == nil {
		return coreauth.IPv6BindLeaseInfo{}
	}
	authID := strings.TrimSpace(auth.ID)
	if authID != "" {
		m.mu.RLock()
		state, ok := m.auths[authID]
		m.mu.RUnlock()
		if ok && strings.TrimSpace(state.lease.URL) != "" && strings.TrimSpace(state.lease.IP) != "" {
			return state.lease
		}
	}
	return coreauth.IPv6BindLease(auth)
}

func (m *CodexFailoverManager) RotateToNextIPv6Lease(cfg *config.Config, auth *coreauth.Auth, now time.Time) bool {
	if m == nil || cfg == nil || auth == nil {
		return false
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return false
	}
	poolName := strings.TrimSpace(auth.ProxyPool)
	if poolName == "" {
		poolName = strings.TrimSpace(cfg.DefaultProxyPool)
	}
	pool := cfg.ProxyPoolByName(poolName)
	if pool == nil || len(pool.IPv6BindLeaseRanges) == 0 {
		return false
	}
	current := m.CurrentIPv6Lease(auth)
	occupied := m.runtimeOccupiedIPv6Leases(authID)
	if strings.TrimSpace(current.IP) != "" {
		occupied[strings.TrimSpace(current.IP)] = struct{}{}
	}
	allocated, ok := config.AllocateIPv6BindLease(pool, auth.EnsureIndex()+"-failover", occupied)
	if !ok || strings.TrimSpace(allocated.URL) == "" || strings.TrimSpace(allocated.IP) == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(allocated.IP), strings.TrimSpace(current.IP)) {
		return false
	}
	m.mu.Lock()
	state := m.auths[authID]
	state.mode = CodexFailoverModeDirectV6
	state.preferUntil = now.Add(codexFailoverHold(cfg))
	state.reason = strings.TrimSpace(state.reason)
	state.lease = coreauth.IPv6BindLeaseInfo{
		Pool:      allocated.Pool,
		EntryName: allocated.EntryName,
		IP:        allocated.IP,
		URL:       allocated.URL,
	}
	m.auths[authID] = state
	m.mu.Unlock()
	return true
}

func (m *CodexFailoverManager) runtimeOccupiedIPv6Leases(excludeAuthID string) map[string]struct{} {
	out := make(map[string]struct{})
	if m == nil {
		return out
	}
	excludeAuthID = strings.TrimSpace(excludeAuthID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for authID, state := range m.auths {
		if strings.TrimSpace(authID) == excludeAuthID {
			continue
		}
		ip := strings.TrimSpace(state.lease.IP)
		if ip == "" {
			continue
		}
		out[ip] = struct{}{}
	}
	return out
}

func codexFailoverHold(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.CodexRouteManagement.RecoveringProbeInterval > 0 {
		return cfg.CodexRouteManagement.RecoveringProbeInterval
	}
	return 30 * time.Second
}
