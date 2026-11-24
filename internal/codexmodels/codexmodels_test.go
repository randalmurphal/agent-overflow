package codexmodels

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/procutil"
	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

func TestGet_CachesUntilTTLExpires(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	cache := NewWith(time.Minute, func(_ context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{Slug: "first"}}, nil
	}, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		got, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
		if err != nil {
			t.Fatalf("iteration %d Get: %v", i, err)
		}
		if len(got) != 1 || got[0].Slug != "first" {
			t.Fatalf("iteration %d unexpected models: %#v", i, got)
		}
	}
	if calls != 1 {
		t.Fatalf("lister calls = %d, want 1 (TTL should cover all three Gets)", calls)
	}

	now = now.Add(2 * time.Minute)
	if _, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper()); err != nil {
		t.Fatalf("post-expiry Get: %v", err)
	}
	if calls != 2 {
		t.Fatalf("lister calls = %d, want 2 after TTL expiry", calls)
	}
}

func TestGet_ReturnsDefensiveClone(t *testing.T) {
	cache := NewWith(time.Minute, func(_ context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{Slug: "m", Capabilities: []string{"a", "b"}}}, nil
	}, nil)
	got, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got[0].Capabilities[0] = "CLOBBERED"

	again, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if again[0].Capabilities[0] != "a" {
		t.Fatalf("cache mutated through returned slice: %v", again[0].Capabilities)
	}
}

func TestPeek_IsNonBlockingAndReturnsOnlyFreshCachedResults(t *testing.T) {
	now := time.Unix(1000, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	cache := NewWith(time.Minute, func(_ context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		close(started)
		<-release
		return []provider.ModelInfo{{Slug: "cached", Capabilities: []string{"fast"}}}, nil
	}, func() time.Time { return now })

	if models, err, ok := cache.Peek(Launch{Binary: "codex"}); ok || err != nil || models != nil {
		t.Fatalf("cold Peek = (%v, %v, %v), want cache miss", models, err, ok)
	}
	done := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
		done <- err
	}()
	<-started
	if models, err, ok := cache.Peek(Launch{Binary: "codex"}); ok || err != nil || models != nil {
		t.Fatalf("in-flight Peek = (%v, %v, %v), want nonblocking cache miss", models, err, ok)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Get: %v", err)
	}

	models, err, ok := cache.Peek(Launch{Binary: "codex"})
	if !ok || err != nil || len(models) != 1 || models[0].Slug != "cached" {
		t.Fatalf("warm Peek = (%v, %v, %v), want cached result", models, err, ok)
	}
	models[0].Capabilities[0] = "mutated"
	again, _, ok := cache.Peek(Launch{Binary: "codex"})
	if !ok || again[0].Capabilities[0] != "fast" {
		t.Fatalf("Peek returned shared cache storage: %v", again)
	}

	now = now.Add(2 * time.Minute)
	if models, err, ok := cache.Peek(Launch{Binary: "codex"}); ok || err != nil || models != nil {
		t.Fatalf("expired Peek = (%v, %v, %v), want cache miss", models, err, ok)
	}
}

func TestReset_DetachesInFlightLookup(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	calls := 0
	cache := NewWith(time.Hour, func(_ context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		calls++
		if calls == 1 {
			close(firstStarted)
			<-releaseFirst
			return []provider.ModelInfo{{Slug: "stale"}}, nil
		}
		return []provider.ModelInfo{{Slug: "fresh"}}, nil
	}, nil)

	firstDone := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
		firstDone <- err
	}()
	<-firstStarted
	cache.Reset()

	models, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
	if err != nil {
		t.Fatalf("post-reset Get: %v", err)
	}
	if len(models) != 1 || models[0].Slug != "fresh" {
		t.Fatalf("post-reset models = %v, want fresh lookup", models)
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("detached Get: %v", err)
	}

	models, err, ok := cache.Peek(Launch{Binary: "codex"})
	if !ok || err != nil || len(models) != 1 || models[0].Slug != "fresh" {
		t.Fatalf("cache after detached completion = (%v, %v, %v), want fresh result", models, err, ok)
	}
}

func TestGet_DropsCachedEntryOnReset(t *testing.T) {
	calls := 0
	cache := NewWith(time.Hour, func(_ context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{Slug: "x"}}, nil
	}, nil)

	if _, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cache.Reset()
	if _, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper()); err != nil {
		t.Fatalf("post-reset Get: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected reset to invalidate (calls=%d, want 2)", calls)
	}
}

func TestGet_SingleFlightsConcurrentCalls(t *testing.T) {
	gate := make(chan struct{})
	var calls int
	var callMu sync.Mutex
	cache := NewWith(time.Minute, func(ctx context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		callMu.Lock()
		calls++
		callMu.Unlock()
		select {
		case <-gate:
			return []provider.ModelInfo{{Slug: "y"}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, nil)

	const N = 16
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
			results <- err
		}()
	}
	// Let the goroutines park inside the lister or the inflight channel.
	time.Sleep(20 * time.Millisecond)
	close(gate)

	for i := 0; i < N; i++ {
		if err := <-results; err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	callMu.Lock()
	defer callMu.Unlock()
	if calls != 1 {
		t.Fatalf("lister calls = %d, want 1 (single-flight)", calls)
	}
}

func TestGet_ErrorIsCachedBriefly(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	cache := NewWith(time.Hour, func(_ context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		calls++
		return nil, errors.New("boom")
	}, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if _, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper()); err == nil {
			t.Fatalf("iteration %d expected error", i)
		}
	}
	if calls != 1 {
		t.Fatalf("error should be cached briefly: calls=%d, want 1", calls)
	}
	if models, err, ok := cache.Peek(Launch{Binary: "codex"}); !ok || err == nil || models != nil {
		t.Fatalf("Peek cached error = (%v, %v, %v), want error hit", models, err, ok)
	}

	now = now.Add(DefaultErrorTTL + time.Second)
	if _, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper()); err == nil {
		t.Fatal("post-expiry Get error = nil, want error")
	}
	if calls != 2 {
		t.Fatalf("expired error should retry: calls=%d, want 2", calls)
	}
}

func TestGet_CtxCancelledWhileSingleFlightInflight(t *testing.T) {
	block := make(chan struct{})
	cache := NewWith(time.Minute, func(ctx context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		<-block
		return nil, nil
	}, nil)

	go func() {
		_, _ = cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Get(ctx, Launch{Binary: "codex"}, procutiltest.Reaper()); err == nil {
		t.Fatalf("expected context error")
	}
	close(block)
}

// The caller that started a lookup leaving ends only its wait: the lookup
// keeps running for a caller sharing it, and the next Get is served its
// answer rather than the first caller's cancellation.
func TestGet_FirstCallerLeavingEndsOnlyItsWait(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	cache := NewWith(time.Minute, func(ctx context.Context, _ Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []provider.ModelInfo{{Slug: "first"}}, nil
	}, nil)

	callerA, leaveA := context.WithCancel(context.Background())
	aDone := make(chan error, 1)
	go func() {
		_, err := cache.Get(callerA, Launch{Binary: "codex"}, procutiltest.Reaper())
		aDone <- err
	}()
	<-started
	bDone := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper())
		bDone <- err
	}()
	leaveA()
	if err := <-aDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller A after leaving: %v, want context.Canceled", err)
	}
	close(release)
	if err := <-bDone; err != nil {
		t.Fatalf("caller B, sharing the lookup: %v", err)
	}
	if got, err := cache.Get(context.Background(), Launch{Binary: "codex"}, procutiltest.Reaper()); err != nil || len(got) != 1 {
		t.Fatalf("next Get = %v, %v, want the cached answer", got, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("lister calls = %d, want 1", n)
	}
}

// One binary path holds one entry, as on main: a read with another launch
// (working directory or environment) replaces it, and its models and its
// remembered capabilities go with it. Rotating a custom environment value
// therefore never accumulates catalogs.
func TestCacheHoldsOneEntryPerBinary(t *testing.T) {
	t.Parallel()
	var got []Launch
	cache := NewWith(time.Hour, func(_ context.Context, launch Launch, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		got = append(got, launch)
		return []provider.ModelInfo{{Slug: launch.Env["PICK"] + launch.WorkDir}}, nil
	}, time.Now)
	launches := []Launch{
		{Binary: "codex", WorkDir: "/w", Env: map[string]string{"PICK": "a"}},
		{Binary: "codex", WorkDir: "/w", Env: map[string]string{"PICK": "b"}},
		{Binary: " codex ", WorkDir: "/w", Env: map[string]string{"PICK": "b"}},
		{Binary: "codex", WorkDir: "/v", Env: map[string]string{"PICK": "b"}},
		{Binary: "codex", WorkDir: "/w", Env: map[string]string{"PICK": "a"}},
	}
	want := []string{"a/w", "b/w", "b/w", "b/v", "a/w"}
	for i, launch := range launches {
		models, err := cache.Get(context.Background(), launch, procutiltest.Reaper())
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 1 || models[0].Slug != want[i] {
			t.Fatalf("launch %d answered %+v, want %q", i, models, want[i])
		}
		cache.mu.Lock()
		entries := len(cache.entries)
		cache.mu.Unlock()
		if entries != 1 {
			t.Fatalf("after launch %d the cache holds %d entries, want one for the binary", i, entries)
		}
	}
	if len(got) != 4 {
		t.Fatalf("lister ran %d times, want 4: only the trimmed binary reuses the entry before it", len(got))
	}
	if got[1].Env["PICK"] != "b" || got[2].WorkDir != "/v" {
		t.Fatalf("lister launches = %+v, want each launch as given", got)
	}
	if _, _, ok := cache.Peek(launches[1]); ok {
		t.Fatal("Peek read a launch the binary's entry no longer holds")
	}
	if _, ok := cache.KnownModel(launches[1], "b/w"); ok {
		t.Fatal("KnownModel kept a replaced launch's models")
	}
	if _, ok := cache.KnownModel(launches[4], "a/w"); !ok {
		t.Fatal("KnownModel did not read the current launch's catalog")
	}
	if _, _, ok := cache.Peek(Launch{Binary: "codex", WorkDir: "/w"}); ok {
		t.Fatal("a launch without the environment read another launch's catalog")
	}
}
