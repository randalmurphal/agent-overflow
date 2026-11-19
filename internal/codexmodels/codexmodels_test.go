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
	cache := NewWith(time.Minute, func(_ context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{Slug: "first"}}, nil
	}, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		got, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
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
	if _, err := cache.Get(context.Background(), "codex", procutiltest.Reaper()); err != nil {
		t.Fatalf("post-expiry Get: %v", err)
	}
	if calls != 2 {
		t.Fatalf("lister calls = %d, want 2 after TTL expiry", calls)
	}
}

func TestGet_ReturnsDefensiveClone(t *testing.T) {
	cache := NewWith(time.Minute, func(_ context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{Slug: "m", Capabilities: []string{"a", "b"}}}, nil
	}, nil)
	got, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got[0].Capabilities[0] = "CLOBBERED"

	again, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
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
	cache := NewWith(time.Minute, func(_ context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		close(started)
		<-release
		return []provider.ModelInfo{{Slug: "cached", Capabilities: []string{"fast"}}}, nil
	}, func() time.Time { return now })

	if models, err, ok := cache.Peek("codex"); ok || err != nil || models != nil {
		t.Fatalf("cold Peek = (%v, %v, %v), want cache miss", models, err, ok)
	}
	done := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
		done <- err
	}()
	<-started
	if models, err, ok := cache.Peek("codex"); ok || err != nil || models != nil {
		t.Fatalf("in-flight Peek = (%v, %v, %v), want nonblocking cache miss", models, err, ok)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Get: %v", err)
	}

	models, err, ok := cache.Peek("codex")
	if !ok || err != nil || len(models) != 1 || models[0].Slug != "cached" {
		t.Fatalf("warm Peek = (%v, %v, %v), want cached result", models, err, ok)
	}
	models[0].Capabilities[0] = "mutated"
	again, _, ok := cache.Peek("codex")
	if !ok || again[0].Capabilities[0] != "fast" {
		t.Fatalf("Peek returned shared cache storage: %v", again)
	}

	now = now.Add(2 * time.Minute)
	if models, err, ok := cache.Peek("codex"); ok || err != nil || models != nil {
		t.Fatalf("expired Peek = (%v, %v, %v), want cache miss", models, err, ok)
	}
}

func TestReset_DetachesInFlightLookup(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	calls := 0
	cache := NewWith(time.Hour, func(_ context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
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
		_, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
		firstDone <- err
	}()
	<-firstStarted
	cache.Reset()

	models, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
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

	models, err, ok := cache.Peek("codex")
	if !ok || err != nil || len(models) != 1 || models[0].Slug != "fresh" {
		t.Fatalf("cache after detached completion = (%v, %v, %v), want fresh result", models, err, ok)
	}
}

func TestGet_DropsCachedEntryOnReset(t *testing.T) {
	calls := 0
	cache := NewWith(time.Hour, func(_ context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{Slug: "x"}}, nil
	}, nil)

	if _, err := cache.Get(context.Background(), "codex", procutiltest.Reaper()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cache.Reset()
	if _, err := cache.Get(context.Background(), "codex", procutiltest.Reaper()); err != nil {
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
	cache := NewWith(time.Minute, func(ctx context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
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
			_, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
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
	cache := NewWith(time.Hour, func(_ context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		calls++
		return nil, errors.New("boom")
	}, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if _, err := cache.Get(context.Background(), "codex", procutiltest.Reaper()); err == nil {
			t.Fatalf("iteration %d expected error", i)
		}
	}
	if calls != 1 {
		t.Fatalf("error should be cached briefly: calls=%d, want 1", calls)
	}
	if models, err, ok := cache.Peek("codex"); !ok || err == nil || models != nil {
		t.Fatalf("Peek cached error = (%v, %v, %v), want error hit", models, err, ok)
	}

	now = now.Add(DefaultErrorTTL + time.Second)
	if _, err := cache.Get(context.Background(), "codex", procutiltest.Reaper()); err == nil {
		t.Fatal("post-expiry Get error = nil, want error")
	}
	if calls != 2 {
		t.Fatalf("expired error should retry: calls=%d, want 2", calls)
	}
}

func TestGet_CtxCancelledWhileSingleFlightInflight(t *testing.T) {
	block := make(chan struct{})
	cache := NewWith(time.Minute, func(ctx context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
		<-block
		return nil, nil
	}, nil)

	go func() {
		_, _ = cache.Get(context.Background(), "codex", procutiltest.Reaper())
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Get(ctx, "codex", procutiltest.Reaper()); err == nil {
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
	cache := NewWith(time.Minute, func(ctx context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
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
		_, err := cache.Get(callerA, "codex", procutiltest.Reaper())
		aDone <- err
	}()
	<-started
	bDone := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), "codex", procutiltest.Reaper())
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
	if got, err := cache.Get(context.Background(), "codex", procutiltest.Reaper()); err != nil || len(got) != 1 {
		t.Fatalf("next Get = %v, %v, want the cached answer", got, err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("lister calls = %d, want 1", n)
	}
}

type testClock struct{ nanos atomic.Int64 }

func (c *testClock) now() time.Time           { return time.Unix(0, c.nanos.Load()) }
func (c *testClock) advance(by time.Duration) { c.nanos.Add(int64(by)) }

// Latest is a pure read: cold it is not known, and it never starts a
// lookup, cold or expired. Only Get fetches; Latest then has Get's last
// successful catalog, through expiry and a later failure, until a later
// success replaces it or Reset clears it.
func TestLatest_ReadsTheLastGoodCatalogAndStartsNothing(t *testing.T) {
	type result struct {
		models []provider.ModelInfo
		err    error
	}
	var next atomic.Pointer[result]
	var calls atomic.Int32
	clock := &testClock{}
	clock.advance(time.Hour)
	cache := NewWith(time.Minute, func(context.Context, string, procutil.Reaper) ([]provider.ModelInfo, error) {
		calls.Add(1)
		r := next.Load()
		if r == nil {
			return nil, errors.New("a lookup the test did not ask for")
		}
		return r.models, r.err
	}, clock.now)
	get := func(r result) {
		t.Helper()
		next.Store(&r)
		if _, err := cache.Get(context.Background(), "codex", procutiltest.Reaper()); !errors.Is(err, r.err) {
			t.Fatalf("Get = %v, want %v", err, r.err)
		}
	}
	// latest reads and checks that the read started no lookup: the cache
	// registers a lookup in flight before the call that starts it returns.
	latest := func(when string, lookups int32) ([]provider.ModelInfo, bool) {
		t.Helper()
		models, ok := cache.Latest("codex")
		cache.mu.Lock()
		inflight := len(cache.inflight)
		cache.mu.Unlock()
		if got := calls.Load(); inflight != 0 || got != lookups {
			t.Fatalf("%s: Latest started a lookup: %d in flight, %d made, want 0 and %d", when, inflight, got, lookups)
		}
		return models, ok
	}
	wantSlug := func(when string, models []provider.ModelInfo, ok bool, slug string) {
		t.Helper()
		if !ok || len(models) != 1 || models[0].Slug != slug {
			t.Fatalf("%s: Latest = (%v, %v), want %s", when, models, ok, slug)
		}
	}

	if models, ok := latest("cold", 0); ok || models != nil {
		t.Fatalf("cold Latest = (%v, %v), want not known", models, ok)
	}
	if models, ok := latest("cold again", 0); ok || models != nil {
		t.Fatalf("second cold Latest = (%v, %v), want not known", models, ok)
	}

	get(result{models: []provider.ModelInfo{{Slug: "first", Capabilities: []string{"fast"}}}})
	models, ok := latest("after Get", 1)
	wantSlug("after Get", models, ok, "first")
	models[0].Capabilities[0] = "mutated"
	if again, _ := latest("after mutating a copy", 1); again[0].Capabilities[0] != "fast" {
		t.Fatalf("Latest returned shared cache storage: %v", again)
	}

	clock.advance(2 * time.Minute)
	models, ok = latest("expired", 1)
	wantSlug("expired", models, ok, "first")

	get(result{err: errors.New("initialize for model/list: context deadline exceeded")})
	models, ok = latest("after a failed Get", 2)
	wantSlug("after a failed Get", models, ok, "first")
	clock.advance(DefaultErrorTTL + time.Second)
	models, ok = latest("after the failure expired", 2)
	wantSlug("after the failure expired", models, ok, "first")

	get(result{models: []provider.ModelInfo{{Slug: "second"}}})
	models, ok = latest("after a later Get", 3)
	wantSlug("after a later Get", models, ok, "second")

	cache.Reset()
	if models, ok := latest("after Reset", 3); ok || models != nil {
		t.Fatalf("Latest after Reset = (%v, %v), want not known", models, ok)
	}
	get(result{err: errors.New("down")})
	if _, ok := latest("after a failed Get following Reset", 4); ok {
		t.Fatal("a failed lookup made the catalog known")
	}
}
