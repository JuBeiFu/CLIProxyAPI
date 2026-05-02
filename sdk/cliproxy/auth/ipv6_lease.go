package auth

import (
	"context"
	"sort"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func (m *Manager) reconcileIPv6BindLeases(ctx context.Context, cfg *internalconfig.Config) {
	if m == nil || cfg == nil {
		return
	}
	m.mu.Lock()
	changed := m.reconcileIPv6BindLeasesLocked(cfg)
	toPersist := make([]*Auth, 0, len(changed))
	for _, auth := range changed {
		toPersist = append(toPersist, auth.Clone())
	}
	m.mu.Unlock()

	for _, auth := range toPersist {
		if err := m.persist(ctx, auth); err != nil {
			logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist IPv6 bind lease: %v", err)
		}
	}
	if m.scheduler != nil && len(toPersist) > 0 {
		for _, auth := range toPersist {
			m.scheduler.upsertAuth(auth)
		}
	}
}

func (m *Manager) reconcileIPv6BindLeasesLocked(cfg *internalconfig.Config) []*Auth {
	if m == nil || cfg == nil || len(cfg.ProxyPools) == 0 || len(m.auths) == 0 {
		return nil
	}
	ids := make([]string, 0, len(m.auths))
	for id := range m.auths {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	changed := make([]*Auth, 0)
	for _, id := range ids {
		auth := m.auths[id]
		if auth == nil {
			continue
		}
		if m.ensureIPv6BindLeaseForAuthLocked(cfg, auth) {
			changed = append(changed, auth)
		}
	}
	return changed
}

func (m *Manager) ensureIPv6BindLeaseForAuthLocked(cfg *internalconfig.Config, auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Disabled {
		if IPv6BindLease(auth).IP != "" {
			ClearIPv6BindLease(auth)
			return true
		}
		return false
	}
	if !m.authEligibleForIPv6BindLeaseLocked(cfg, auth) {
		return false
	}

	lease := IPv6BindLease(auth)
	pool := m.ipv6BindLeasePoolForAuthLocked(cfg, auth)
	occupied := m.occupiedIPv6BindLeasesLocked(cfg, auth.ID)
	if lease.IP != "" && lease.URL != "" && pool != nil && internalconfig.IPv6BindLeasePoolContains(pool, lease.IP) {
		if _, inUse := occupied[lease.IP]; !inUse {
			return false
		}
	}
	if lease.IP != "" || lease.URL != "" {
		ClearIPv6BindLease(auth)
	}
	if pool == nil {
		return lease.IP != "" || lease.URL != ""
	}
	allocated, ok := internalconfig.AllocateIPv6BindLease(pool, auth.EnsureIndex(), occupied)
	if !ok {
		return lease.IP != "" || lease.URL != ""
	}
	SetIPv6BindLease(auth, IPv6BindLeaseInfo{
		Pool:      allocated.Pool,
		EntryName: allocated.EntryName,
		IP:        allocated.IP,
		URL:       allocated.URL,
	})
	return true
}

func preserveIPv6BindLeaseIfMissing(auth, existing *Auth) {
	if auth == nil || existing == nil {
		return
	}
	if lease := IPv6BindLease(auth); lease.IP != "" && lease.URL != "" {
		return
	}
	existingLease := IPv6BindLease(existing)
	if existingLease.IP == "" || existingLease.URL == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	SetIPv6BindLease(auth, existingLease)
}

func (m *Manager) occupiedIPv6BindLeasesLocked(cfg *internalconfig.Config, excludeAuthID string) map[string]struct{} {
	occupied := make(map[string]struct{}, len(m.auths))
	excludeAuthID = strings.TrimSpace(excludeAuthID)
	for id, auth := range m.auths {
		if auth == nil || strings.TrimSpace(id) == excludeAuthID || auth.Disabled {
			continue
		}
		lease := IPv6BindLease(auth)
		if lease.IP == "" {
			continue
		}
		pool := m.ipv6BindLeasePoolForAuthLocked(cfg, auth)
		if pool == nil || !internalconfig.IPv6BindLeasePoolContains(pool, lease.IP) {
			continue
		}
		occupied[lease.IP] = struct{}{}
	}
	return occupied
}

func (m *Manager) authEligibleForIPv6BindLeaseLocked(cfg *internalconfig.Config, auth *Auth) bool {
	if auth == nil || auth.Disabled {
		return false
	}
	if auth.Metadata == nil {
		return false
	}
	if strings.TrimSpace(auth.ProxyURL) != "" {
		return false
	}
	pool := m.ipv6BindLeasePoolForAuthLocked(cfg, auth)
	return pool != nil && len(pool.IPv6BindLeaseRanges) > 0
}

func (m *Manager) ipv6BindLeasePoolForAuthLocked(cfg *internalconfig.Config, auth *Auth) *internalconfig.ProxyPool {
	if cfg == nil || auth == nil {
		return nil
	}
	poolName := strings.TrimSpace(auth.ProxyPool)
	if poolName == "" {
		poolName = strings.TrimSpace(cfg.DefaultProxyPool)
	}
	if poolName == "" {
		return nil
	}
	return cfg.ProxyPoolByName(poolName)
}
