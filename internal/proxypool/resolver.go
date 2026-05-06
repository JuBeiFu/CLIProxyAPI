package proxypool

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Resolution is the effective outbound proxy decision for one auth/request.
type Resolution struct {
	ProxyURL         string
	ProxyPool        string
	ProxyName        string
	Source           string
	FallbackToDirect bool
}

// Resolve applies proxy-url / proxy-pool precedence for one auth.
func Resolve(cfg *config.Config, auth *coreauth.Auth) Resolution {
	return ResolveWithContext(context.Background(), cfg, auth, DefaultHealthManager())
}

// ResolveWithHealth applies proxy-url / proxy-pool precedence while skipping
// pool entries that have been actively marked unhealthy by the health manager.
func ResolveWithHealth(cfg *config.Config, auth *coreauth.Auth, manager *HealthManager) Resolution {
	return ResolveWithContext(context.Background(), cfg, auth, manager)
}

func ResolveWithContext(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, manager *HealthManager) Resolution {
	if route, ok := RequestRouteFromContext(ctx); ok {
		poolName := strings.TrimSpace(route.Pool)
		if poolName == "" {
			if auth != nil {
				poolName = strings.TrimSpace(auth.ProxyPool)
			}
			if poolName == "" && cfg != nil {
				poolName = strings.TrimSpace(cfg.DefaultProxyPool)
			}
		}
		if route.Direct {
			return Resolution{
				ProxyPool:        poolName,
				ProxyName:        strings.TrimSpace(route.Entry),
				Source:           "request-route-direct",
				FallbackToDirect: true,
			}
		}
		if entry, okEntry := findPoolEntryByName(cfg, poolName, route.Entry); okEntry {
			fallbackToDirect := false
			if cfg != nil {
				if pool := cfg.ProxyPoolByName(poolName); pool != nil {
					fallbackToDirect = pool.FallbackToDirect
				}
			}
			return Resolution{
				ProxyURL:         entry.URL,
				ProxyPool:        poolName,
				ProxyName:        entry.Name,
				Source:           "request-route",
				FallbackToDirect: fallbackToDirect,
			}
		}
	}

	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return Resolution{
				ProxyURL: proxyURL,
				Source:   "auth.proxy-url",
			}
		}
	}

	poolName := ""
	if auth != nil {
		poolName = strings.TrimSpace(auth.ProxyPool)
	}
	if poolName == "" && cfg != nil {
		poolName = strings.TrimSpace(cfg.DefaultProxyPool)
	}
	if cfg != nil && poolName != "" {
		if pool := cfg.ProxyPoolByName(poolName); pool != nil {
			if lease := coreauth.IPv6BindLease(auth); lease.URL != "" && lease.IP != "" {
				leasePool := strings.TrimSpace(lease.Pool)
				if leasePool == "" || strings.EqualFold(leasePool, pool.Name) {
					if config.IPv6BindLeasePoolContains(pool, lease.IP) {
						return Resolution{
							ProxyURL:         lease.URL,
							ProxyPool:        pool.Name,
							ProxyName:        lease.EntryName,
							Source:           "ipv6-bind-lease",
							FallbackToDirect: pool.FallbackToDirect,
						}
					}
				}
			}
			// Prefer the pool entry the auth was pinned to by the
			// multi-path plan_type probe. Probe and real dispatch MUST
			// travel through the same node because OpenAI's plan_type
			// cache is per-edge: mixing nodes between probe and dispatch
			// means the cached "paid" decision from the probe could be
			// contradicted by the node the dispatch happens to land on.
			bound := coreauth.BoundProxyEntry(auth)
			if bound != "" && bound != coreauth.BoundProxyEntryDirect {
				for _, entry := range pool.Entries {
					if entry.Disabled || strings.TrimSpace(entry.URL) == "" {
						continue
					}
					if entry.Name != bound {
						continue
					}
					if manager != nil && !manager.IsUsable(pool.Name, entry.Name) {
						break
					}
					return Resolution{
						ProxyURL:         entry.URL,
						ProxyPool:        pool.Name,
						ProxyName:        entry.Name,
						Source:           "bound",
						FallbackToDirect: pool.FallbackToDirect,
					}
				}
			} else if bound == coreauth.BoundProxyEntryDirect {
				// Auth was explicitly pinned to direct egress because
				// every pool entry reported free but direct reported
				// paid. Honor that.
				return Resolution{
					ProxyPool:        pool.Name,
					Source:           "bound-direct",
					FallbackToDirect: true,
				}
			}
			// For codex auths the FNV hash fallback is UNSAFE: OpenAI's
			// plan_type cache is per-region, routed by (client_IP,
			// account_id). Probing via one path and dispatching through a
			// randomly hashed other path means the dispatch can land on a
			// region that reports this account as free — causing the auth
			// to hit free quota even though cached probed=paid. Short-circuit
			// to direct egress until forced_refresh restores a valid binding
			// (<=5min via shouldForceRefresh "unbound" / "unhealthy-bound"
			// rules). Non-codex providers keep the hash-pick fallback.
			if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
				return Resolution{
					ProxyPool:        pool.Name,
					Source:           "codex-awaiting-rebind",
					FallbackToDirect: true,
				}
			}
			if entry, ok := selectPoolEntryWithHealth(pool, auth, manager); ok {
				return Resolution{
					ProxyURL:         entry.URL,
					ProxyPool:        pool.Name,
					ProxyName:        entry.Name,
					Source:           "proxy-pool",
					FallbackToDirect: pool.FallbackToDirect,
				}
			}
			if pool.FallbackToDirect {
				return Resolution{
					ProxyPool:        pool.Name,
					Source:           "proxy-pool",
					FallbackToDirect: true,
				}
			}
		}
	}

	if cfg != nil {
		if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
			return Resolution{
				ProxyURL: proxyURL,
				Source:   "config.proxy-url",
			}
		}
	}

	return Resolution{Source: "direct"}
}

func findPoolEntryByName(cfg *config.Config, poolName, entryName string) (config.ProxyPoolEntry, bool) {
	if cfg == nil {
		return config.ProxyPoolEntry{}, false
	}
	pool := cfg.ProxyPoolByName(strings.TrimSpace(poolName))
	if pool == nil {
		return config.ProxyPoolEntry{}, false
	}
	needle := strings.TrimSpace(entryName)
	for _, entry := range pool.Entries {
		if strings.EqualFold(strings.TrimSpace(entry.Name), needle) {
			return entry, true
		}
	}
	return config.ProxyPoolEntry{}, false
}

func selectPoolEntry(pool *config.ProxyPool, auth *coreauth.Auth) (config.ProxyPoolEntry, bool) {
	return selectPoolEntryWithHealth(pool, auth, nil)
}

// SelectPoolEntryWithHealth returns the stable hash-selected pool entry after
// filtering out disabled or currently unusable entries.
func SelectPoolEntryWithHealth(pool *config.ProxyPool, auth *coreauth.Auth, manager *HealthManager) (config.ProxyPoolEntry, bool) {
	return selectPoolEntryWithHealth(pool, auth, manager)
}

func selectPoolEntryWithHealth(pool *config.ProxyPool, auth *coreauth.Auth, manager *HealthManager) (config.ProxyPoolEntry, bool) {
	if pool == nil || len(pool.Entries) == 0 {
		return config.ProxyPoolEntry{}, false
	}

	weighted := make([]config.ProxyPoolEntry, 0, len(pool.Entries))
	for _, entry := range pool.Entries {
		if entry.Disabled || strings.TrimSpace(entry.URL) == "" {
			continue
		}
		if manager != nil && !manager.IsUsable(pool.Name, entry.Name) {
			continue
		}
		weight := entry.Weight
		if weight <= 0 {
			weight = 1
		}
		for i := 0; i < weight; i++ {
			weighted = append(weighted, entry)
		}
	}
	if len(weighted) == 0 {
		return config.ProxyPoolEntry{}, false
	}

	seed := authSeed(auth)
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(pool.Name))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(seed))
	index := int(hash.Sum64() % uint64(len(weighted)))
	return weighted[index], true
}

func authSeed(auth *coreauth.Auth) string {
	if auth == nil {
		return "default"
	}
	if index := strings.TrimSpace(auth.EnsureIndex()); index != "" {
		return index
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return id
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	return strings.TrimSpace(auth.Provider) + ":" + strconv.FormatBool(auth.Disabled)
}
