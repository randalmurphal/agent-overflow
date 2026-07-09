package codexmodels

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestGet_CachesUntilTTLExpires(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	cache := NewWith(time.Minute, func(_ context.Context, _ string) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{Slug: "first"}}, nil
	}, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		got, err := cache.Get(context.Background(), "codex")
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
	if _, err := cache.Get(context.Background(), "codex"); err != nil {
		t.Fatalf("post-expiry Get: %v", err)
	}
	if calls != 2 {
		t.Fatalf("lister calls = %d, want 2 after TTL expiry", calls)
	}
}

func TestGet_ReturnsDefensiveClone(t *testing.T) {
	cache := NewWith(time.Minute, func(_ context.Context, _ string) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{Slug: "m", Capabilities: []string{"a", "b"}}}, nil
	}, nil)
	got, err := cache.Get(context.Background(), "codex")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got[0].Capabilities[0] = "CLOBBERED"

	again, err := cache.Get(context.Background(), "codex")
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if again[0].Capabilities[0] != "a" {
		t.Fatalf("cache mutated through returned slice: %v", again[0].Capabilities)
	}
}

func TestGet_DropsCachedEntryOnReset(t *testing.T) {
	calls := 0
	cache := NewWith(time.Hour, func(_ context.Context, _ string) ([]provider.ModelInfo, error) {
		calls++
		return []provider.ModelInfo{{Slug: "x"}}, nil
	}, nil)

	if _, err := cache.Get(context.Background(), "codex"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cache.Reset()
	if _, err := cache.Get(context.Background(), "codex"); err != nil {
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
	cache := NewWith(time.Minute, func(ctx context.Context, _ string) ([]provider.ModelInfo, error) {
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
			_, err := cache.Get(context.Background(), "codex")
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
	cache := NewWith(time.Hour, func(_ context.Context, _ string) ([]provider.ModelInfo, error) {
		calls++
		return nil, errors.New("boom")
	}, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if _, err := cache.Get(context.Background(), "codex"); err == nil {
			t.Fatalf("iteration %d expected error", i)
		}
	}
	if calls != 1 {
		t.Fatalf("error should be cached briefly: calls=%d, want 1", calls)
	}

	now = now.Add(DefaultErrorTTL + time.Second)
	if _, err := cache.Get(context.Background(), "codex"); err == nil {
		t.Fatal("post-expiry Get error = nil, want error")
	}
	if calls != 2 {
		t.Fatalf("expired error should retry: calls=%d, want 2", calls)
	}
}

func TestGet_CtxCancelledWhileSingleFlightInflight(t *testing.T) {
	block := make(chan struct{})
	cache := NewWith(time.Minute, func(ctx context.Context, _ string) ([]provider.ModelInfo, error) {
		<-block
		return nil, nil
	}, nil)

	go func() {
		_, _ = cache.Get(context.Background(), "codex")
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Get(ctx, "codex"); err == nil {
		t.Fatalf("expected context error")
	}
	close(block)
}
