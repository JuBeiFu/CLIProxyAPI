package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type schedulerBenchmarkExecutor struct {
	id string
}

func (e schedulerBenchmarkExecutor) Identifier() string { return e.id }

func (e schedulerBenchmarkExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerBenchmarkExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e schedulerBenchmarkExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e schedulerBenchmarkExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerBenchmarkExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func benchmarkManagerSetup(b *testing.B, total int, mixed bool, withPriority bool) (*Manager, []string, string) {
	b.Helper()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	providers := []string{"gemini"}
	manager.executors["gemini"] = schedulerBenchmarkExecutor{id: "gemini"}
	if mixed {
		providers = []string{"gemini", "claude"}
		manager.executors["claude"] = schedulerBenchmarkExecutor{id: "claude"}
	}

	reg := registry.GetGlobalRegistry()
	model := "bench-model"
	for index := 0; index < total; index++ {
		provider := providers[0]
		if mixed && index%2 == 1 {
			provider = providers[1]
		}
		auth := &Auth{ID: fmt.Sprintf("bench-%s-%04d", provider, index), Provider: provider}
		if withPriority {
			priority := "0"
			if index%2 == 0 {
				priority = "10"
			}
			auth.Attributes = map[string]string{"priority": priority}
		}
		_, errRegister := manager.Register(context.Background(), auth)
		if errRegister != nil {
			b.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
		reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
	}
	manager.syncScheduler()
	b.Cleanup(func() {
		for index := 0; index < total; index++ {
			provider := providers[0]
			if mixed && index%2 == 1 {
				provider = providers[1]
			}
			reg.UnregisterClient(fmt.Sprintf("bench-%s-%04d", provider, index))
		}
	})

	return manager, providers, model
}

func BenchmarkManagerPickNext500(b *testing.B) {
	manager, _, model := benchmarkManagerSetup(b, 500, false, false)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	if _, _, errWarm := manager.pickNext(ctx, "gemini", model, opts, tried); errWarm != nil {
		b.Fatalf("warmup pickNext error = %v", errWarm)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth, exec, errPick := manager.pickNext(ctx, "gemini", model, opts, tried)
		if errPick != nil || auth == nil || exec == nil {
			b.Fatalf("pickNext failed: auth=%v exec=%v err=%v", auth, exec, errPick)
		}
	}
}

func BenchmarkManagerPickNext1000(b *testing.B) {
	manager, _, model := benchmarkManagerSetup(b, 1000, false, false)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	if _, _, errWarm := manager.pickNext(ctx, "gemini", model, opts, tried); errWarm != nil {
		b.Fatalf("warmup pickNext error = %v", errWarm)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth, exec, errPick := manager.pickNext(ctx, "gemini", model, opts, tried)
		if errPick != nil || auth == nil || exec == nil {
			b.Fatalf("pickNext failed: auth=%v exec=%v err=%v", auth, exec, errPick)
		}
	}
}

func BenchmarkManagerPickNextPriority500(b *testing.B) {
	manager, _, model := benchmarkManagerSetup(b, 500, false, true)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	if _, _, errWarm := manager.pickNext(ctx, "gemini", model, opts, tried); errWarm != nil {
		b.Fatalf("warmup pickNext error = %v", errWarm)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth, exec, errPick := manager.pickNext(ctx, "gemini", model, opts, tried)
		if errPick != nil || auth == nil || exec == nil {
			b.Fatalf("pickNext failed: auth=%v exec=%v err=%v", auth, exec, errPick)
		}
	}
}

func BenchmarkManagerPickNextPriority1000(b *testing.B) {
	manager, _, model := benchmarkManagerSetup(b, 1000, false, true)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	if _, _, errWarm := manager.pickNext(ctx, "gemini", model, opts, tried); errWarm != nil {
		b.Fatalf("warmup pickNext error = %v", errWarm)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth, exec, errPick := manager.pickNext(ctx, "gemini", model, opts, tried)
		if errPick != nil || auth == nil || exec == nil {
			b.Fatalf("pickNext failed: auth=%v exec=%v err=%v", auth, exec, errPick)
		}
	}
}

func BenchmarkManagerPickNextMixed500(b *testing.B) {
	manager, providers, model := benchmarkManagerSetup(b, 500, true, false)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	if _, _, _, errWarm := manager.pickNextMixed(ctx, providers, model, opts, tried); errWarm != nil {
		b.Fatalf("warmup pickNextMixed error = %v", errWarm)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth, exec, provider, errPick := manager.pickNextMixed(ctx, providers, model, opts, tried)
		if errPick != nil || auth == nil || exec == nil || provider == "" {
			b.Fatalf("pickNextMixed failed: auth=%v exec=%v provider=%q err=%v", auth, exec, provider, errPick)
		}
	}
}

func BenchmarkManagerPickNextMixedPriority500(b *testing.B) {
	manager, providers, model := benchmarkManagerSetup(b, 500, true, true)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	if _, _, _, errWarm := manager.pickNextMixed(ctx, providers, model, opts, tried); errWarm != nil {
		b.Fatalf("warmup pickNextMixed error = %v", errWarm)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth, exec, provider, errPick := manager.pickNextMixed(ctx, providers, model, opts, tried)
		if errPick != nil || auth == nil || exec == nil || provider == "" {
			b.Fatalf("pickNextMixed failed: auth=%v exec=%v provider=%q err=%v", auth, exec, provider, errPick)
		}
	}
}

func BenchmarkManagerPickNextAndMarkResult1000(b *testing.B) {
	manager, _, model := benchmarkManagerSetup(b, 1000, false, false)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	if _, _, errWarm := manager.pickNext(ctx, "gemini", model, opts, tried); errWarm != nil {
		b.Fatalf("warmup pickNext error = %v", errWarm)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		auth, _, errPick := manager.pickNext(ctx, "gemini", model, opts, tried)
		if errPick != nil || auth == nil {
			b.Fatalf("pickNext failed: auth=%v err=%v", auth, errPick)
		}
		manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "gemini", Model: model, Success: true})
	}
}

func benchmarkSchedulerMutationSetup(b *testing.B, total int) (*authScheduler, *Auth, *Auth, string) {
	b.Helper()
	manager, _, model := benchmarkManagerSetup(b, total, false, false)
	if manager.scheduler == nil {
		b.Fatal("expected scheduler to be initialized")
	}
	if _, _, errPick := manager.pickNext(context.Background(), "gemini", model, cliproxyexecutor.Options{}, map[string]struct{}{}); errPick != nil {
		b.Fatalf("warmup pickNext error = %v", errPick)
	}
	auth, ok := manager.GetByID("bench-gemini-0000")
	if !ok || auth == nil {
		b.Fatal("expected seed auth to exist")
	}
	ready := auth.Clone()
	blocked := auth.Clone()
	blocked.Unavailable = true
	blocked.Status = StatusError
	blocked.NextRetryAfter = benchmarkTimeNow().Add(30 * time.Minute)
	return manager.scheduler, ready, blocked, model
}

func benchmarkSchedulerDisabledMutationSetup(b *testing.B, total int) (*authScheduler, *Auth, *Auth, string) {
	b.Helper()
	scheduler, ready, _, model := benchmarkSchedulerMutationSetup(b, total)
	disabled := ready.Clone()
	disabled.Disabled = true
	disabled.Status = StatusDisabled
	return scheduler, ready, disabled, model
}

func benchmarkTimeNow() time.Time {
	return time.Now().UTC()
}

func benchmarkSchedulerConcurrentStateSetup(b *testing.B, total int, mutate func(*Auth, time.Time) *Auth) (*authScheduler, []*Auth, []*Auth, string) {
	b.Helper()
	manager, _, model := benchmarkManagerSetup(b, total, false, false)
	if manager.scheduler == nil {
		b.Fatal("expected scheduler to be initialized")
	}
	ready := make([]*Auth, 0, total)
	alternate := make([]*Auth, 0, total)
	now := benchmarkTimeNow()
	for index := 0; index < total; index++ {
		authID := fmt.Sprintf("bench-gemini-%04d", index)
		auth, ok := manager.GetByID(authID)
		if !ok || auth == nil {
			b.Fatalf("GetByID(%s) returned no auth", authID)
		}
		ready = append(ready, auth)
		alternate = append(alternate, mutate(auth.Clone(), now))
	}
	return manager.scheduler, ready, alternate, model
}

func benchmarkSchedulerConcurrentPickWithUpsertChurn(b *testing.B, total int, mutate func(*Auth, time.Time) *Auth) {
	scheduler, ready, alternate, model := benchmarkSchedulerConcurrentStateSetup(b, total, mutate)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		index := 0
		useAlternate := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			position := index % len(ready)
			if useAlternate {
				scheduler.upsertAuth(alternate[position])
			} else {
				scheduler.upsertAuth(ready[position])
			}
			index++
			useAlternate = !useAlternate
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		tried := map[string]struct{}{}
		for pb.Next() {
			if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
				b.Fatalf("pickSingle under churn error = %v", errPick)
			}
		}
	})
}

func benchmarkSchedulerConcurrentPickWithRemoveReaddChurn(b *testing.B, total int) {
	scheduler, ready, _, model := benchmarkSchedulerConcurrentStateSetup(b, total, func(auth *Auth, now time.Time) *Auth {
		auth.UpdatedAt = now.Add(2 * time.Second)
		return auth
	})
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		index := 0
		removed := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			auth := ready[index%len(ready)]
			if removed {
				scheduler.upsertAuth(auth)
			} else {
				scheduler.removeAuth(auth.ID)
			}
			index++
			removed = !removed
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		tried := map[string]struct{}{}
		for pb.Next() {
			if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
				b.Fatalf("pickSingle under remove/readd churn error = %v", errPick)
			}
		}
	})
}

func BenchmarkSchedulerToggleCooldown256(b *testing.B) {
	scheduler, ready, blocked, _ := benchmarkSchedulerMutationSetup(b, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.upsertAuth(blocked)
		} else {
			scheduler.upsertAuth(ready)
		}
	}
}

func BenchmarkSchedulerConcurrentPickWithCooldownChurn256(b *testing.B) {
	benchmarkSchedulerConcurrentPickWithUpsertChurn(b, 256, func(auth *Auth, now time.Time) *Auth {
		auth.Unavailable = true
		auth.Status = StatusError
		auth.NextRetryAfter = now.Add(30 * time.Minute)
		auth.UpdatedAt = now.Add(2 * time.Second)
		return auth
	})
}

func BenchmarkSchedulerConcurrentPickWithDisabledChurn256(b *testing.B) {
	benchmarkSchedulerConcurrentPickWithUpsertChurn(b, 256, func(auth *Auth, now time.Time) *Auth {
		auth.Disabled = true
		auth.Status = StatusDisabled
		auth.UpdatedAt = now.Add(2 * time.Second)
		return auth
	})
}

func BenchmarkSchedulerConcurrentPickWithRemoveReaddChurn256(b *testing.B) {
	benchmarkSchedulerConcurrentPickWithRemoveReaddChurn(b, 256)
}

func BenchmarkSchedulerToggleCooldown1000(b *testing.B) {
	scheduler, ready, blocked, _ := benchmarkSchedulerMutationSetup(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.upsertAuth(blocked)
		} else {
			scheduler.upsertAuth(ready)
		}
	}
}

func BenchmarkSchedulerToggleCooldownPick256(b *testing.B) {
	scheduler, ready, blocked, model := benchmarkSchedulerMutationSetup(b, 256)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.upsertAuth(blocked)
		} else {
			scheduler.upsertAuth(ready)
		}
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after cooldown toggle error = %v", errPick)
		}
	}
}

func BenchmarkSchedulerToggleCooldownPick1000(b *testing.B) {
	scheduler, ready, blocked, model := benchmarkSchedulerMutationSetup(b, 1000)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.upsertAuth(blocked)
		} else {
			scheduler.upsertAuth(ready)
		}
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after cooldown toggle error = %v", errPick)
		}
	}
}

func BenchmarkSchedulerToggleDisabledPick256(b *testing.B) {
	scheduler, ready, disabled, model := benchmarkSchedulerDisabledMutationSetup(b, 256)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.upsertAuth(disabled)
		} else {
			scheduler.upsertAuth(ready)
		}
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after disabled toggle error = %v", errPick)
		}
	}
}

func BenchmarkSchedulerToggleDisabledPick1000(b *testing.B) {
	scheduler, ready, disabled, model := benchmarkSchedulerDisabledMutationSetup(b, 1000)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.upsertAuth(disabled)
		} else {
			scheduler.upsertAuth(ready)
		}
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after disabled toggle error = %v", errPick)
		}
	}
}

func BenchmarkSchedulerRemoveAdd256(b *testing.B) {
	scheduler, ready, _, _ := benchmarkSchedulerMutationSetup(b, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.removeAuth(ready.ID)
		} else {
			scheduler.upsertAuth(ready)
		}
	}
}

func BenchmarkSchedulerRemoveAdd1000(b *testing.B) {
	scheduler, ready, _, _ := benchmarkSchedulerMutationSetup(b, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			scheduler.removeAuth(ready.ID)
		} else {
			scheduler.upsertAuth(ready)
		}
	}
}

func BenchmarkSchedulerRemoveAddPick256(b *testing.B) {
	scheduler, ready, _, model := benchmarkSchedulerMutationSetup(b, 256)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scheduler.removeAuth(ready.ID)
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after remove error = %v", errPick)
		}
		scheduler.upsertAuth(ready)
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after add error = %v", errPick)
		}
	}
}

func BenchmarkSchedulerRemoveAddPick1000(b *testing.B) {
	scheduler, ready, _, model := benchmarkSchedulerMutationSetup(b, 1000)
	ctx := context.Background()
	opts := cliproxyexecutor.Options{}
	tried := map[string]struct{}{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scheduler.removeAuth(ready.ID)
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after remove error = %v", errPick)
		}
		scheduler.upsertAuth(ready)
		if _, errPick := scheduler.pickSingle(ctx, "gemini", model, opts, tried); errPick != nil {
			b.Fatalf("pick after add error = %v", errPick)
		}
	}
}
