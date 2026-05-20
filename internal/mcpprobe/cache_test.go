package mcpprobe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/mcp"
)

func newTestCache(stdioTTL, httpTTL time.Duration) *Cache {
	c := NewCache(stdioTTL, httpTTL)
	c.now = func() time.Time { return time.Unix(0, 0) }
	return c
}

func newSpec(name, transport string) mcp.Spec {
	return mcp.Spec{
		Provider:  "claude",
		Name:      name,
		Transport: transport,
		Enabled:   true,
		Command:   "echo",
	}
}

func TestCache_GetMissRunsProbeAndStores(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	var calls atomic.Int32
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		calls.Add(1)
		return Result{CacheKey: spec.CacheKey(), Provider: spec.Provider, ServerName: spec.Name, Status: mcp.StatusReady, ToolCount: 7}
	}
	spec := newSpec("alpha", mcp.TransportStdio)

	got := c.Get(context.Background(), spec, false)
	if got.Status != mcp.StatusReady || got.ToolCount != 7 {
		t.Errorf("first Get = %#v", got)
	}
	if calls.Load() != 1 {
		t.Errorf("probe call count = %d, want 1", calls.Load())
	}

	got = c.Get(context.Background(), spec, false)
	if got.Status != mcp.StatusReady {
		t.Errorf("second Get status = %v", got.Status)
	}
	if calls.Load() != 1 {
		t.Errorf("probe called again on cache hit: %d", calls.Load())
	}
}

func TestCache_ForceBypassesCache(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	var calls atomic.Int32
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		calls.Add(1)
		return Result{CacheKey: spec.CacheKey(), Status: mcp.StatusReady}
	}
	spec := newSpec("alpha", mcp.TransportStdio)

	_ = c.Get(context.Background(), spec, false)
	_ = c.Get(context.Background(), spec, true)
	if calls.Load() != 2 {
		t.Errorf("force=true should bypass cache; calls=%d", calls.Load())
	}
}

func TestCache_InvalidateDropsEntry(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	var calls atomic.Int32
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		calls.Add(1)
		return Result{CacheKey: spec.CacheKey(), Status: mcp.StatusReady}
	}
	spec := newSpec("alpha", mcp.TransportStdio)

	_ = c.Get(context.Background(), spec, false)
	c.Invalidate(spec.CacheKey())
	_ = c.Get(context.Background(), spec, false)
	if calls.Load() != 2 {
		t.Errorf("invalidate did not force re-probe; calls=%d", calls.Load())
	}
}

func TestCache_InvalidateScopedByProvider(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		return Result{CacheKey: spec.CacheKey(), Status: mcp.StatusReady}
	}
	claude := mcp.Spec{Provider: "claude", Name: "github", Transport: mcp.TransportStdio, Enabled: true, Command: "echo"}
	codex := mcp.Spec{Provider: "codex", Name: "github", Transport: mcp.TransportStdio, Enabled: true, Command: "echo"}
	_ = c.Get(context.Background(), claude, false)
	_ = c.Get(context.Background(), codex, false)
	c.Invalidate(claude.CacheKey())
	snap := c.Snapshot()
	if _, ok := snap[claude.CacheKey()]; ok {
		t.Errorf("claude:github should be invalidated")
	}
	if _, ok := snap[codex.CacheKey()]; !ok {
		t.Errorf("codex:github should survive a claude-scoped Invalidate")
	}
}

func TestCache_InvalidateAllDropsEverything(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		return Result{CacheKey: spec.CacheKey(), Status: mcp.StatusReady}
	}
	_ = c.Get(context.Background(), newSpec("a", mcp.TransportStdio), false)
	_ = c.Get(context.Background(), newSpec("b", mcp.TransportHTTP), false)
	if got := c.Snapshot(); len(got) != 2 {
		t.Fatalf("snapshot size before InvalidateAll = %d", len(got))
	}
	c.InvalidateAll()
	if got := c.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot size after InvalidateAll = %d", len(got))
	}
}

func TestCache_TTLExpirationOnLookup(t *testing.T) {
	c := newTestCache(time.Second, time.Second)
	var now atomic.Int64
	now.Store(time.Unix(0, 0).UnixNano())
	c.now = func() time.Time { return time.Unix(0, now.Load()) }
	var calls atomic.Int32
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		calls.Add(1)
		return Result{CacheKey: spec.CacheKey(), Status: mcp.StatusReady}
	}
	spec := newSpec("alpha", mcp.TransportStdio)

	_ = c.Get(context.Background(), spec, false)
	if calls.Load() != 1 {
		t.Fatalf("first Get call count: %d", calls.Load())
	}

	now.Add(int64(2 * time.Second))
	_ = c.Get(context.Background(), spec, false)
	if calls.Load() != 2 {
		t.Errorf("post-expiry Get should re-probe; calls=%d", calls.Load())
	}
}

func TestCache_SnapshotFiltersExpired(t *testing.T) {
	c := newTestCache(time.Second, time.Second)
	var now atomic.Int64
	now.Store(time.Unix(0, 0).UnixNano())
	c.now = func() time.Time { return time.Unix(0, now.Load()) }
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		return Result{CacheKey: spec.CacheKey(), Status: mcp.StatusReady}
	}
	specA := newSpec("a", mcp.TransportStdio)
	specB := newSpec("b", mcp.TransportHTTP)
	_ = c.Get(context.Background(), specA, false)
	now.Add(int64(2 * time.Second))
	_ = c.Get(context.Background(), specB, false)

	snap := c.Snapshot()
	if _, ok := snap[specA.CacheKey()]; ok {
		t.Errorf("expired entry 'a' should not appear in snapshot")
	}
	if _, ok := snap[specB.CacheKey()]; !ok {
		t.Errorf("fresh entry 'b' should appear in snapshot")
	}
}

func TestCache_TTLZeroDisablesCache(t *testing.T) {
	c := newTestCache(0, 0)
	var calls atomic.Int32
	c.probe = func(ctx context.Context, spec mcp.Spec) Result {
		calls.Add(1)
		return Result{CacheKey: spec.CacheKey(), Status: mcp.StatusReady}
	}
	spec := newSpec("alpha", mcp.TransportStdio)
	_ = c.Get(context.Background(), spec, false)
	_ = c.Get(context.Background(), spec, false)
	if calls.Load() != 2 {
		t.Errorf("TTL=0 should disable cache, calls=%d", calls.Load())
	}
}

func TestProbe_DisabledServerReturnsUnknown(t *testing.T) {
	spec := mcp.Spec{
		Provider:  "claude",
		Name:      "alpha",
		Transport: mcp.TransportStdio,
		Command:   "/bin/echo",
		Enabled:   false,
	}
	got := Probe(context.Background(), spec)
	if got.Status != mcp.StatusUnknown {
		t.Errorf("disabled server status = %v, want unknown", got.Status)
	}
	if got.Error == "" {
		t.Errorf("disabled server should carry an explanation in Error")
	}
}

func TestProbe_UnsupportedTransportReturnsFailed(t *testing.T) {
	spec := mcp.Spec{
		Provider:  "claude",
		Name:      "x",
		Transport: "websocket",
		Enabled:   true,
	}
	got := Probe(context.Background(), spec)
	if got.Status != mcp.StatusFailed {
		t.Errorf("unsupported transport status = %v, want failed", got.Status)
	}
}
