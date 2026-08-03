package codexusage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider/codex"
)

func int64Ptr(v int64) *int64 { return &v }

func sampleUsage() codex.AccountUsage {
	return codex.AccountUsage{
		LifetimeTokens: int64Ptr(42),
		DailyBuckets: []codex.AccountUsageDailyBucket{
			{StartDate: "2026-08-01", Tokens: 7},
		},
	}
}

func TestCacheServesWithinTTLAndRefetchesAfter(t *testing.T) {
	now := time.Unix(0, 0)
	cache := NewWith(time.Minute, 10*time.Second, func() time.Time { return now })

	calls := 0
	fetch := func(context.Context) (codex.AccountUsage, error) {
		calls++
		return sampleUsage(), nil
	}

	for i := 0; i < 3; i++ {
		if _, err := cache.Get(context.Background(), "k", fetch); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times inside the TTL, want 1", calls)
	}

	now = now.Add(time.Minute + time.Second)
	if _, err := cache.Get(context.Background(), "k", fetch); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch called %d times after expiry, want 2", calls)
	}
}

// TestCacheKeepsFailuresBrieflyAndSharesThem covers the pair of rules that
// keep an error from turning into a fabricated empty report: the failure is
// returned to every caller, and it is retried sooner than a success would be.
func TestCacheKeepsFailuresBrieflyAndSharesThem(t *testing.T) {
	now := time.Unix(0, 0)
	cache := NewWith(time.Hour, 10*time.Second, func() time.Time { return now })

	wantErr := errors.New("boom")
	calls := 0
	fetch := func(context.Context) (codex.AccountUsage, error) {
		calls++
		return codex.AccountUsage{}, wantErr
	}

	if _, err := cache.Get(context.Background(), "k", fetch); !errors.Is(err, wantErr) {
		t.Fatalf("first Get err = %v, want %v", err, wantErr)
	}
	if _, err := cache.Get(context.Background(), "k", fetch); !errors.Is(err, wantErr) {
		t.Fatalf("cached Get err = %v, want the same error", err)
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times inside the error TTL, want 1", calls)
	}

	now = now.Add(11 * time.Second)
	if _, err := cache.Get(context.Background(), "k", fetch); !errors.Is(err, wantErr) {
		t.Fatalf("Get after error TTL err = %v", err)
	}
	if calls != 2 {
		t.Fatalf("fetch called %d times after the error TTL, want 2", calls)
	}
}

func TestCacheSingleFlightsConcurrentCallers(t *testing.T) {
	cache := New()

	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	fetch := func(context.Context) (codex.AccountUsage, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return sampleUsage(), nil
	}

	const callers = 8
	var wg sync.WaitGroup
	results := make([]codex.AccountUsage, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			usage, err := cache.Get(context.Background(), "k", fetch)
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			results[i] = usage
		}(i)
	}
	// Let the goroutines pile onto the in-flight load before releasing it.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("fetch called %d times under concurrency, want 1", calls)
	}
	for i, usage := range results {
		if usage.LifetimeTokens == nil || *usage.LifetimeTokens != 42 {
			t.Fatalf("caller %d got %+v, want the shared result", i, usage)
		}
	}
}

// TestCacheHandsOutIndependentCopies proves a caller cannot reach into the
// cached entry through either reference field.
func TestCacheHandsOutIndependentCopies(t *testing.T) {
	cache := New()
	fetch := func(context.Context) (codex.AccountUsage, error) { return sampleUsage(), nil }

	first, err := cache.Get(context.Background(), "k", fetch)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	*first.LifetimeTokens = 999
	first.DailyBuckets[0].Tokens = 999

	second, err := cache.Get(context.Background(), "k", fetch)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if *second.LifetimeTokens != 42 {
		t.Errorf("lifetime tokens = %d, want the cache to be unreachable through the pointer", *second.LifetimeTokens)
	}
	if second.DailyBuckets[0].Tokens != 7 {
		t.Errorf("bucket tokens = %d, want the cache to be unreachable through the slice", second.DailyBuckets[0].Tokens)
	}
}

func TestCacheKeysAreIndependentAndInvalidatable(t *testing.T) {
	cache := New()
	calls := map[string]int{}
	fetchFor := func(key string) Fetch {
		return func(context.Context) (codex.AccountUsage, error) {
			calls[key]++
			return sampleUsage(), nil
		}
	}

	for _, key := range []string{"acct-a", "acct-b", "acct-a"} {
		if _, err := cache.Get(context.Background(), key, fetchFor(key)); err != nil {
			t.Fatalf("Get(%s): %v", key, err)
		}
	}
	if calls["acct-a"] != 1 || calls["acct-b"] != 1 {
		t.Fatalf("calls = %v, want one per distinct key", calls)
	}

	cache.Invalidate("acct-a")
	if _, err := cache.Get(context.Background(), "acct-a", fetchFor("acct-a")); err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if calls["acct-a"] != 2 {
		t.Fatalf("acct-a fetched %d times, want a refetch after Invalidate", calls["acct-a"])
	}
	if calls["acct-b"] != 1 {
		t.Fatalf("invalidating one key refetched another: %v", calls)
	}
}
