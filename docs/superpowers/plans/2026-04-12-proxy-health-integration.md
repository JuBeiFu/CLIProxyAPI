# Proxy Health Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make proxy pool health a real runtime signal and surface it in the management center usage page with the existing health-card style.

**Architecture:** Introduce a proxypool health manager that owns probe state, background checks, and selection-time filtering. Expose the health snapshot through management APIs, then wire the management center usage page to render a new Proxy Health card that reuses the existing health-panel visual language but shows pool and entry health instead of request success rate.

**Tech Stack:** Go, Gin, existing proxypool/config packages, React, TypeScript, management-center Vite build, Docker test container deployment.

---

### Task 1: Document current runtime gap and add failing backend tests

**Files:**
- Modify: `internal/proxypool/resolver_test.go`
- Create: `internal/proxypool/health_test.go`
- Test: `internal/proxypool/resolver_test.go`
- Test: `internal/proxypool/health_test.go`

- [ ] **Step 1: Write failing resolver tests for unhealthy proxy exclusion**

```go
func TestResolveSkipsUnhealthyProxyPoolEntries(t *testing.T) {
	manager := NewHealthManager()
	manager.StoreResult("shared-egress", "proxy-a", ProbeResult{Healthy: false})
	manager.StoreResult("shared-egress", "proxy-b", ProbeResult{Healthy: true})

	cfg := &config.Config{SDKConfig: config.SDKConfig{
		DefaultProxyPool: "shared-egress",
		ProxyPools: []config.ProxyPool{{
			Name: "shared-egress",
			Entries: []config.ProxyPoolEntry{
				{Name: "proxy-a", URL: "http://proxy-a.local:8080"},
				{Name: "proxy-b", URL: "http://proxy-b.local:8080"},
			},
		}},
	}}

	got := ResolveWithHealth(cfg, &coreauth.Auth{ID: "auth-1"}, manager)
	if got.ProxyName != "proxy-b" {
		t.Fatalf("expected healthy proxy-b, got %q", got.ProxyName)
	}
}
```

- [ ] **Step 2: Run backend resolver test to verify it fails**

Run: `go test ./internal/proxypool -run "TestResolveSkipsUnhealthyProxyPoolEntries|TestResolveAllUnhealthyUsesDirectFallback" -count=1`
Expected: FAIL because health-aware resolver path does not exist yet.

- [ ] **Step 3: Write failing tests for health snapshot and probe updates**

```go
func TestHealthManagerUpdatesSnapshotFromProbe(t *testing.T) {
	manager := NewHealthManager()
	manager.StoreResult("shared-egress", "proxy-a", ProbeResult{
		Healthy:    true,
		StatusCode: 204,
		Latency:    150 * time.Millisecond,
	})

	snapshot := manager.Snapshot()
	entry := snapshot["shared-egress"]["proxy-a"]
	if !entry.Healthy || entry.StatusCode != 204 {
		t.Fatalf("unexpected snapshot: %+v", entry)
	}
}
```

- [ ] **Step 4: Run backend health test to verify it fails**

Run: `go test ./internal/proxypool -run TestHealthManagerUpdatesSnapshotFromProbe -count=1`
Expected: FAIL because the health manager implementation does not exist yet.

- [ ] **Step 5: Commit**

```bash
git add internal/proxypool/resolver_test.go internal/proxypool/health_test.go
git commit -m "test: cover proxy pool health-aware routing"
```

### Task 2: Implement proxy health manager and runtime selection

**Files:**
- Create: `internal/proxypool/health.go`
- Modify: `internal/proxypool/resolver.go`
- Modify: `internal/proxypool/http.go`
- Modify: `internal/api/handlers/management/handler.go`
- Modify: `internal/api/handlers/management/proxy_pools.go`
- Modify: `internal/api/server.go`
- Test: `internal/proxypool/resolver_test.go`
- Test: `internal/proxypool/health_test.go`

- [ ] **Step 1: Add the minimal health state model**

```go
type ProbeResult struct {
	Healthy    bool
	StatusCode int
	Latency    time.Duration
	Error      string
	CheckedAt  time.Time
}

type HealthManager struct {
	mu      sync.RWMutex
	entries map[string]map[string]ProbeResult
}
```

- [ ] **Step 2: Implement snapshot, update, and pool filtering helpers**

```go
func (m *HealthManager) StoreResult(poolName, entryName string, result ProbeResult) { ... }
func (m *HealthManager) Snapshot() map[string]map[string]ProbeResult { ... }
func (m *HealthManager) IsHealthy(poolName, entryName string) (ProbeResult, bool) { ... }
```

- [ ] **Step 3: Make resolver consult the health manager**

```go
func ResolveWithHealth(cfg *config.Config, auth *coreauth.Auth, manager *HealthManager) Resolution {
	// same precedence as Resolve, but pool entry selection skips unhealthy entries
}
```

- [ ] **Step 4: Keep old callers working by routing `Resolve` through the new helper**

```go
var defaultHealthManager = NewHealthManager()

func Resolve(cfg *config.Config, auth *coreauth.Auth) Resolution {
	return ResolveWithHealth(cfg, auth, defaultHealthManager)
}
```

- [ ] **Step 5: Implement management-driven probe execution and background ticker**

```go
func (m *HealthManager) CheckPool(ctx context.Context, pool config.ProxyPool) PoolSnapshot { ... }
func (m *HealthManager) Start(ctx context.Context, cfgProvider func() *config.Config) { ... }
```

- [ ] **Step 6: Return cached status from management APIs and refresh on explicit `/proxy-pools/check`**

```go
func (h *Handler) GetProxyPools(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"default-proxy-pool": h.cfg.DefaultProxyPool,
		"proxy-pools": h.cfg.ProxyPools,
		"proxy-health": h.proxyHealth.Snapshot(),
	})
}
```

- [ ] **Step 7: Run focused backend tests**

Run: `go test ./internal/proxypool ./internal/api/... -run "ProxyPool|ProxyHealth" -count=1`
Expected: PASS, with unhealthy entries skipped and management snapshots populated.

- [ ] **Step 8: Commit**

```bash
git add internal/proxypool/health.go internal/proxypool/resolver.go internal/proxypool/http.go internal/api/handlers/management/handler.go internal/api/handlers/management/proxy_pools.go internal/api/server.go internal/proxypool/resolver_test.go internal/proxypool/health_test.go
git commit -m "feat: wire proxy pool health into runtime routing"
```

### Task 3: Add Proxy Health card to management center usage page

**Files:**
- Create: `src/services/api/proxyHealth.ts`
- Create: `src/components/usage/ProxyHealthCard.tsx`
- Modify: `src/components/usage/index.ts`
- Modify: `src/pages/UsagePage.tsx`
- Modify: `src/i18n/locales/en/*.json`
- Modify: `src/i18n/locales/zh/*.json`
- Test: `npm run build`

- [ ] **Step 1: Add a typed API client for proxy pool and health data**

```ts
export const proxyHealthApi = {
  getPools: () => apiClient.get<ProxyPoolsResponse>('/proxy-pools'),
  checkPools: (name?: string) => apiClient.post<ProxyHealthResponse>('/proxy-pools/check', name ? { name } : {}),
};
```

- [ ] **Step 2: Build a `ProxyHealthCard` that reuses the existing health-card structure**

```tsx
export function ProxyHealthCard() {
  const [snapshot, setSnapshot] = useState<ProxyHealthResponse | null>(null);
  // render default pool, per-entry status, latency, last error, refresh action
}
```

- [ ] **Step 3: Replace or complement the old service-health section in `UsagePage`**

```tsx
<ServiceHealthCard usage={usage} loading={loading} />
<ProxyHealthCard />
```

- [ ] **Step 4: Run front-end build to verify it passes**

Run: `npm run build`
Expected: PASS, with the new card included in the production bundle.

- [ ] **Step 5: Commit**

```bash
git add src/services/api/proxyHealth.ts src/components/usage/ProxyHealthCard.tsx src/components/usage/index.ts src/pages/UsagePage.tsx
git commit -m "feat: show proxy health in usage page"
```

### Task 4: Deploy to the test container and verify end-to-end behavior

**Files:**
- Modify: test container binary and `management.html` artifact only
- Test: remote test container

- [ ] **Step 1: Build backend and management center artifacts**

Run: `go test ./...`
Expected: PASS for the edited packages.

Run: `go build -o CLIProxyAPI.exe ./cmd/cli-proxy-api`
Expected: PASS, producing the updated backend binary.

Run: `npm run build`
Expected: PASS, producing the updated management center static bundle.

- [ ] **Step 2: Copy artifacts into `cliproxyapi-main-latest-sync-test`**

```bash
scp CLIProxyAPI root@66.92.50.50:/root/cliproxyapi-main-latest-sync-test/CLIProxyAPI
scp dist/management.html root@66.92.50.50:/root/cliproxyapi-main-latest-sync-test/static/management.html
```

- [ ] **Step 3: Restart only the test container and verify runtime behavior**

Run: `docker restart cliproxyapi-main-latest-sync-test`
Expected: container returns healthy.

Run: `curl -s -H "Authorization: Bearer codex-test-banrecords-20260412" http://127.0.0.1:8080/v0/management/proxy-pools/check`
Expected: JSON shows all proxies with health details.

- [ ] **Step 4: Force one bad proxy and verify it is skipped by runtime routing**

```bash
# edit test config entry URL to an invalid port, restart test container, send api-call twice
# expect selected proxy changes away from the unhealthy entry or direct fallback when all are bad
```

- [ ] **Step 5: Open the management center usage page and verify the Proxy Health card**

Expected:
- Usage page shows the new Proxy Health module
- default pool name is visible
- per-entry latency / error / status matches `/proxy-pools/check`
- manual refresh updates the card

- [ ] **Step 6: Commit deployment notes if needed**

```bash
git add docs/superpowers/plans/2026-04-12-proxy-health-integration.md
git commit -m "docs: add proxy health integration plan"
```

