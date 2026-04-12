package proxypool

import (
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
	return ResolveWithHealth(cfg, auth, DefaultHealthManager())
}

// ResolveWithHealth applies proxy-url / proxy-pool precedence while skipping
// pool entries that have been actively marked unhealthy by the health manager.
func ResolveWithHealth(cfg *config.Config, auth *coreauth.Auth, manager *HealthManager) Resolution {
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

func selectPoolEntry(pool *config.ProxyPool, auth *coreauth.Auth) (config.ProxyPoolEntry, bool) {
	return selectPoolEntryWithHealth(pool, auth, nil)
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
