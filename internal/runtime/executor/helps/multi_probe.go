// Package helps provides multi-path Codex plan_type probing.
package helps

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net/http"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxypool"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
)

// multiProbeTimeout bounds a single /wham/usage attempt. Tight enough that
// a dead proxy doesn't stall the whole probe; generous enough that a
// healthy slow path (warp/socks) still succeeds.
const multiProbeTimeout = 20 * time.Second

var fetchUsageInfoWithProxy = func(ctx context.Context, proxyURL, accessToken string) (codexauth.WhamUsageInfo, error) {
	client, err := buildProbeClient(proxyURL)
	if err != nil {
		return codexauth.WhamUsageInfo{}, err
	}
	svc := codexauth.NewCodexAuthWithClient(client)
	return svc.FetchWhamUsageInfo(ctx, accessToken)
}

// ProbeCodexPlanAcrossPool tries /wham/usage through every entry of the
// auth's configured proxy pool (shuffled), stopping as soon as one reports
// a paid plan (plus/pro/team/enterprise). If every pool entry reports free
// (or errors), it falls back to direct egress.
//
// Returns:
//   - plan:        the plan_type to record (e.g. "plus", "free"). Empty
//     string iff probeOK=false.
//   - boundEntry:  name of the pool entry (or BoundProxyEntryDirect) the
//     caller should pin the auth to. Empty string when no
//     paid path was found (caller should clear the binding).
//   - probeOK:     true iff at least one path returned a usable plan_type
//     (paid OR free). False means every candidate errored, so the caller
//     keeps the auth's plan_type and bound entry unchanged.
//
// The shuffle randomizes the start order so probes spread across pool
// entries rather than hammering whichever entry the FNV hash happens to
// land on first.
func ProbeCodexPlanAcrossPool(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	accessToken string,
) (plan string, boundEntry string, supportedModels []string, probeOK bool) {
	pool := resolveCodexProbePool(cfg, auth)
	candidates := shuffledHealthyEntries(pool)
	boundPreferredName := strings.TrimSpace(cliproxyauth.BoundProxyEntry(auth))

	type probeCandidate struct {
		entryName string // "" maps to BoundProxyEntryDirect at bind time
		proxyURL  string // "" means direct
	}
	ordered := make([]probeCandidate, 0, len(candidates)+1)
	if boundPreferredName != "" && boundPreferredName != cliproxyauth.BoundProxyEntryDirect {
		for _, e := range candidates {
			if e.Name != boundPreferredName {
				continue
			}
			ordered = append(ordered, probeCandidate{entryName: e.Name, proxyURL: e.URL})
			break
		}
	}
	for _, e := range candidates {
		if boundPreferredName != "" && e.Name == boundPreferredName {
			continue
		}
		ordered = append(ordered, probeCandidate{entryName: e.Name, proxyURL: e.URL})
	}
	// Direct egress is always appended as last-resort fallback.
	ordered = append(ordered, probeCandidate{entryName: cliproxyauth.BoundProxyEntryDirect, proxyURL: ""})

	var lastFreePlan string
	var anySucceeded bool

	for _, c := range ordered {
		if ctx.Err() != nil {
			break
		}
		probeCtx, cancel := context.WithTimeout(ctx, multiProbeTimeout)
		info, probeErr := fetchUsageInfoWithProxy(probeCtx, c.proxyURL, accessToken)
		got := info.PlanType
		cancel()
		if probeErr != nil {
			continue
		}
		got = strings.ToLower(strings.TrimSpace(got))
		if got == "" {
			// Endpoint answered without a plan_type. Treat it as a failed
			// probe so lastFreePlan keeps its previous trusted value.
			continue
		}
		anySucceeded = true
		if isPaidPlanName(got) {
			return got, c.entryName, info.SupportedModels, true
		}
		// Successful probe but reports free. Keep searching for a paid
		// path through other nodes: OpenAI edge caches sometimes disagree
		// on newly-upgraded accounts.
		lastFreePlan = got
	}

	if anySucceeded {
		// All paths reported free. Clear any previous binding: the auth
		// has no egress that will serve it as paid right now.
		return lastFreePlan, "", nil, true
	}

	// Every candidate errored. Caller keeps existing state.
	return "", "", nil, false
}

// resolveCodexProbePool returns the proxy pool to enumerate for the auth.
// Mirrors the pool-name precedence in proxypool.Resolve (auth.ProxyPool >
// cfg.DefaultProxyPool).
func resolveCodexProbePool(cfg *config.Config, auth *cliproxyauth.Auth) *config.ProxyPool {
	if cfg == nil {
		return nil
	}
	poolName := ""
	if auth != nil {
		poolName = strings.TrimSpace(auth.ProxyPool)
	}
	if poolName == "" {
		poolName = strings.TrimSpace(cfg.DefaultProxyPool)
	}
	if poolName == "" {
		return nil
	}
	return cfg.ProxyPoolByName(poolName)
}

// shuffledHealthyEntries returns enabled + healthy entries from the pool
// in a random order. Returns nil if pool is empty or missing.
func shuffledHealthyEntries(pool *config.ProxyPool) []config.ProxyPoolEntry {
	if pool == nil || len(pool.Entries) == 0 {
		return nil
	}
	manager := proxypool.DefaultHealthManager()
	out := make([]config.ProxyPoolEntry, 0, len(pool.Entries))
	for _, e := range pool.Entries {
		if e.Disabled || strings.TrimSpace(e.URL) == "" {
			continue
		}
		if manager != nil && !manager.IsUsable(pool.Name, e.Name) {
			continue
		}
		out = append(out, e)
	}
	secureShuffle(out)
	return out
}

// secureShuffle Fisher-Yates using crypto/rand so the probe order is not
// predictable across auths that share a pool.
func secureShuffle(entries []config.ProxyPoolEntry) {
	n := len(entries)
	for i := n - 1; i > 0; i-- {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return // leave order alone on RNG failure; caller still iterates all
		}
		j := int(binary.BigEndian.Uint64(buf[:]) % uint64(i+1))
		entries[i], entries[j] = entries[j], entries[i]
	}
}

// buildProbeClient returns an http.Client that dials through the given
// proxy URL (socks5 / socks5h / http / https), or direct when empty.
// Uses proxyutil.BuildHTTPTransport for parity with the rest of CLIProxyAPI.
func buildProbeClient(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return &http.Client{Timeout: multiProbeTimeout, Transport: proxyutil.NewDirectTransport()}, nil
	}
	tr, _, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		return &http.Client{Timeout: multiProbeTimeout, Transport: proxyutil.NewDirectTransport()}, nil
	}
	return &http.Client{Timeout: multiProbeTimeout, Transport: tr}, nil
}

// isPaidPlanName mirrors auth.isPaidPlan (unexported there) for probe
// purposes. Keep in sync.
func isPaidPlanName(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "plus", "pro", "team", "enterprise":
		return true
	}
	return false
}
