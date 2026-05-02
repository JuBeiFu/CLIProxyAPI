package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type memoryLeaseStore struct {
	items   []*Auth
	saved   []*Auth
	deleted []string
}

func (s *memoryLeaseStore) List(context.Context) ([]*Auth, error) {
	out := make([]*Auth, 0, len(s.items))
	for _, auth := range s.items {
		out = append(out, auth.Clone())
	}
	return out, nil
}

func (s *memoryLeaseStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth != nil {
		s.saved = append(s.saved, auth.Clone())
		return auth.ID, nil
	}
	return "", nil
}

func (s *memoryLeaseStore) Delete(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func testIPv6LeaseConfig(cidr string) *internalconfig.Config {
	return &internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			DefaultProxyPool: "dedicated-v6",
			ProxyPools: internalconfig.NormalizeProxyPools([]internalconfig.ProxyPool{
				{
					Name: "dedicated-v6",
					IPv6BindLeaseRanges: []internalconfig.IPv6BindLeaseRange{
						{CIDR: cidr, NamePrefix: "acct-v6-"},
					},
				},
			}),
		},
	}
}

func TestManager_RegisterAssignsPersistentIPv6Lease(t *testing.T) {
	store := &memoryLeaseStore{}
	mgr := NewManager(store, nil, nil)
	mgr.SetConfig(testIPv6LeaseConfig("2602:294:0:eb::/126"))

	registered, err := mgr.Register(context.Background(), &Auth{
		ID:       "auth-a",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	lease := IPv6BindLease(registered)
	if lease.IP == "" || lease.URL == "" || lease.EntryName == "" {
		t.Fatalf("expected IPv6 lease metadata, got %+v", lease)
	}
	if lease.Pool != "dedicated-v6" {
		t.Fatalf("lease.Pool = %q, want dedicated-v6", lease.Pool)
	}
	if len(store.saved) != 1 {
		t.Fatalf("expected one persisted auth, got %d", len(store.saved))
	}
	if persisted := IPv6BindLease(store.saved[0]); persisted.IP != lease.IP || persisted.URL != lease.URL {
		t.Fatalf("persisted lease = %+v, want %+v", persisted, lease)
	}
}

func TestManager_LoadKeepsExistingIPv6LeasesAndAssignsMissingOnConfigSet(t *testing.T) {
	store := &memoryLeaseStore{
		items: []*Auth{
			{
				ID:       "auth-a",
				Provider: "codex",
				Metadata: map[string]any{
					"type":                        "codex",
					MetadataIPv6BindLeaseIPKey:    "2602:294:0:eb::1",
					MetadataIPv6BindLeaseURLKey:   "bind://[2602:294:0:eb::1]",
					MetadataIPv6BindLeasePoolKey:  "dedicated-v6",
					MetadataIPv6BindLeaseEntryKey: "acct-v6-existing",
				},
			},
			{
				ID:       "auth-b",
				Provider: "codex",
				Metadata: map[string]any{"type": "codex"},
			},
		},
	}
	mgr := NewManager(store, nil, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	mgr.SetConfig(testIPv6LeaseConfig("2602:294:0:eb::/126"))

	authA, ok := mgr.GetByID("auth-a")
	if !ok {
		t.Fatal("expected auth-a to be loaded")
	}
	if got := IPv6BindLease(authA).IP; got != "2602:294:0:eb::1" {
		t.Fatalf("auth-a lease IP = %q, want existing lease", got)
	}
	authB, ok := mgr.GetByID("auth-b")
	if !ok {
		t.Fatal("expected auth-b to be loaded")
	}
	if got := IPv6BindLease(authB).IP; got == "" || got == "2602:294:0:eb::1" {
		t.Fatalf("auth-b lease IP = %q, want new unique lease", got)
	}
	if len(store.saved) != 1 {
		t.Fatalf("expected only missing lease to be persisted, got %d saves", len(store.saved))
	}
	if store.saved[0].ID != "auth-b" {
		t.Fatalf("persisted auth = %q, want auth-b", store.saved[0].ID)
	}
}

func TestManager_ReusesReleasedIPv6LeaseAfterRevokedDelete(t *testing.T) {
	store := &memoryLeaseStore{}
	mgr := NewManager(store, nil, nil)
	mgr.SetConfig(testIPv6LeaseConfig("2602:294:0:eb::/126"))

	authA := &Auth{
		ID:       "auth-a.json",
		FileName: "auth-a.json",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	registeredA, err := mgr.Register(context.Background(), authA)
	if err != nil {
		t.Fatalf("Register auth-a returned error: %v", err)
	}
	leaseA := IPv6BindLease(registeredA)
	if leaseA.IP == "" {
		t.Fatal("expected auth-a lease")
	}

	mgr.deleteRevokedAuth(registeredA, "token_revoked", "test")

	registeredB, err := mgr.Register(context.Background(), &Auth{
		ID:       "auth-b",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	})
	if err != nil {
		t.Fatalf("Register auth-b returned error: %v", err)
	}
	leaseB := IPv6BindLease(registeredB)
	if leaseB.IP != leaseA.IP {
		t.Fatalf("auth-b lease IP = %q, want recycled %q", leaseB.IP, leaseA.IP)
	}
}

func TestManager_DoesNotAssignIPv6LeaseToRuntimeOnlyAuthWithoutMetadata(t *testing.T) {
	store := &memoryLeaseStore{}
	mgr := NewManager(store, nil, nil)
	mgr.SetConfig(testIPv6LeaseConfig("2602:294:0:eb::/126"))

	registered, err := mgr.Register(context.Background(), &Auth{
		ID:       "runtime-only",
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if lease := IPv6BindLease(registered); lease.IP != "" || lease.URL != "" {
		t.Fatalf("expected no lease for runtime-only auth, got %+v", lease)
	}
	if len(store.saved) != 0 {
		t.Fatalf("expected runtime-only auth not to be persisted, got %d saves", len(store.saved))
	}
}

func TestManager_UpdatePreservesExistingIPv6LeaseWhenIncomingAuthOmitsMetadata(t *testing.T) {
	store := &memoryLeaseStore{
		items: []*Auth{
			{
				ID:       "auth-a",
				Provider: "codex",
				Metadata: map[string]any{
					"type":                        "codex",
					MetadataIPv6BindLeasePoolKey:  "dedicated-v6",
					MetadataIPv6BindLeaseEntryKey: "acct-v6-auth-a",
					MetadataIPv6BindLeaseIPKey:    "2602:294:0:eb::1",
					MetadataIPv6BindLeaseURLKey:   "bind://[2602:294:0:eb::1]",
				},
			},
		},
	}
	mgr := NewManager(store, nil, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	mgr.SetConfig(testIPv6LeaseConfig("2602:294:0:eb::/126"))

	updated, err := mgr.Update(context.Background(), &Auth{
		ID:       "auth-a",
		Provider: "codex",
		// Incoming auth omits metadata entirely, simulating a partial patch.
	})
	if err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	lease := IPv6BindLease(updated)
	if lease.IP != "2602:294:0:eb::1" {
		t.Fatalf("lease.IP = %q, want preserved existing lease", lease.IP)
	}
	if len(store.saved) != 1 {
		t.Fatalf("expected one persisted update, got %d", len(store.saved))
	}
	if got := IPv6BindLease(store.saved[0]); got.IP != "2602:294:0:eb::1" {
		t.Fatalf("persisted lease.IP = %q, want preserved existing lease", got.IP)
	}
}

func TestManager_DisabledAuthLeaseDoesNotOccupyIPv6Address(t *testing.T) {
	store := &memoryLeaseStore{
		items: []*Auth{
			{
				ID:       "disabled-auth",
				Provider: "codex",
				Disabled: true,
				Status:   StatusDisabled,
				Metadata: map[string]any{
					"type":                        "codex",
					MetadataIPv6BindLeasePoolKey:  "dedicated-v6",
					MetadataIPv6BindLeaseEntryKey: "acct-v6-disabled",
					MetadataIPv6BindLeaseIPKey:    "2602:294:0:eb::",
					MetadataIPv6BindLeaseURLKey:   "bind://[2602:294:0:eb::]",
				},
			},
		},
	}
	mgr := NewManager(store, nil, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	mgr.SetConfig(testIPv6LeaseConfig("2602:294:0:eb::/126"))

	disabled, ok := mgr.GetByID("disabled-auth")
	if !ok {
		t.Fatal("expected disabled auth to remain loaded")
	}
	if lease := IPv6BindLease(disabled); lease.IP != "" || lease.URL != "" {
		t.Fatalf("expected disabled auth lease to be cleared, got %+v", lease)
	}

	registered, err := mgr.Register(context.Background(), &Auth{
		ID:       "active-auth",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if got := IPv6BindLease(registered).IP; got != "2602:294:0:eb::" {
		t.Fatalf("active auth lease IP = %q, want recycled disabled address", got)
	}
}
