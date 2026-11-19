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

// scriptedLister answers each lookup with the next result it is handed and
// reports each lookup's start, so a test orders the background lookups
// Latest starts without sleeping.
type scriptedLister struct {
	started chan struct{}
	results chan scriptedResult
	calls   atomic.Int32
}

type scriptedResult struct {
	models []provider.ModelInfo
	err    error
}

func newScriptedLister() *scriptedLister {
	return &scriptedLister{started: make(chan struct{}, 16), results: make(chan scriptedResult)}
}

func (l *scriptedLister) list(ctx context.Context, _ string, _ procutil.Reaper) ([]provider.ModelInfo, error) {
	l.calls.Add(1)
	l.started <- struct{}{}
	select {
	case result := <-l.results:
		return result.models, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// answer hands the lookup in flight its result and returns once the cache
// holds it: Get joins that lookup and returns after it is stored.
func (l *scriptedLister) answer(t *testing.T, cache *Cache, result scriptedResult) {
	t.Helper()
	select {
	case <-l.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no lookup started")
	}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		_, _ = cache.Get(context.Background(), "codex", procutiltest.Reaper())
	}()
	select {
	case l.results <- result:
	case <-time.After(10 * time.Second):
		t.Fatal("the lookup in flight stopped waiting for its answer")
	}
	<-joined
}

type testClock struct{ nanos atomic.Int64 }

func (c *testClock) now() time.Time           { return time.Unix(0, c.nanos.Load()) }
func (c *testClock) advance(by time.Duration) { c.nanos.Add(int64(by)) }

func TestLatest_NeverWaitsAndKeepsTheLastGoodCatalog(t *testing.T) {
	lister := newScriptedLister()
	clock := &testClock{}
	clock.advance(time.Hour)
	cache := NewWith(time.Minute, lister.list, clock.now)
	ctx, cancel := context.WithCancel(context.Background())
	latest := func() ([]provider.ModelInfo, bool) {
		return cache.Latest(ctx, "codex", procutiltest.Reaper())
	}

	if models, ok := latest(); ok || models != nil {
		t.Fatalf("cold Latest = (%v, %v), want not known", models, ok)
	}
	if _, ok := latest(); ok {
		t.Fatal("Latest is known while the first lookup is in flight")
	}
	// The caller's context ending does not end the lookup it started.
	cancel()
	lister.answer(t, cache, scriptedResult{models: []provider.ModelInfo{{Slug: "first", Capabilities: []string{"fast"}}}})
	if got := lister.calls.Load(); got != 1 {
		t.Fatalf("lookups after two cold reads = %d, want 1", got)
	}

	models, ok := latest()
	if !ok || len(models) != 1 || models[0].Slug != "first" {
		t.Fatalf("Latest after success = (%v, %v)", models, ok)
	}
	models[0].Capabilities[0] = "mutated"
	if again, _ := latest(); again[0].Capabilities[0] != "fast" {
		t.Fatalf("Latest returned shared cache storage: %v", again)
	}
	if got := lister.calls.Load(); got != 1 {
		t.Fatalf("lookups while the catalog is fresh = %d, want 1", got)
	}

	clock.advance(2 * time.Minute)
	for _, when := range []string{"expired", "expired, refresh in flight"} {
		if models, ok := latest(); !ok || len(models) != 1 || models[0].Slug != "first" {
			t.Fatalf("%s: Latest = (%v, %v), want the last good catalog", when, models, ok)
		}
	}
	lister.answer(t, cache, scriptedResult{err: errors.New("initialize for model/list: context deadline exceeded")})
	if got := lister.calls.Load(); got != 2 {
		t.Fatalf("lookups after the catalog expired = %d, want 2", got)
	}

	if models, ok := latest(); !ok || len(models) != 1 || models[0].Slug != "first" {
		t.Fatalf("Latest after a failed refresh = (%v, %v), want the last good catalog", models, ok)
	}
	if got := lister.calls.Load(); got != 2 {
		t.Fatalf("lookups while the failure is fresh = %d, want 2", got)
	}
	clock.advance(DefaultErrorTTL + time.Second)
	if _, ok := latest(); !ok {
		t.Fatal("Latest forgot the last good catalog when the failure expired")
	}
	lister.answer(t, cache, scriptedResult{models: []provider.ModelInfo{{Slug: "second"}}})
	if models, ok := latest(); !ok || len(models) != 1 || models[0].Slug != "second" {
		t.Fatalf("Latest after a later success = (%v, %v)", models, ok)
	}

	cache.Reset()
	if models, ok := latest(); ok || models != nil {
		t.Fatalf("Latest after Reset = (%v, %v), want not known", models, ok)
	}
	lister.answer(t, cache, scriptedResult{err: errors.New("down")})
	if _, ok := latest(); ok {
		t.Fatal("a failed lookup made the catalog known")
	}
	if got := lister.calls.Load(); got != 4 {
		t.Fatalf("lookups = %d, want 4", got)
	}
}

func TestLatest_ConcurrentColdReadsStartOneLookup(t *testing.T) {
	lister := newScriptedLister()
	cache := NewWith(time.Minute, lister.list, nil)
	const n = 16
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if _, ok := cache.Latest(context.Background(), "codex", procutiltest.Reaper()); ok {
				t.Error("a cold Latest was known")
			}
		})
	}
	wg.Wait()
	lister.answer(t, cache, scriptedResult{models: []provider.ModelInfo{{Slug: "only"}}})
	if got := lister.calls.Load(); got != 1 {
		t.Fatalf("lookups = %d, want 1", got)
	}
	if models, ok := cache.Latest(context.Background(), "codex", procutiltest.Reaper()); !ok || models[0].Slug != "only" {
		t.Fatalf("Latest = (%v, %v)", models, ok)
	}
}
