package mcpprobe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/store"
)

func newTestCache(stdioTTL, httpTTL time.Duration) *Cache {
	c := NewCache(stdioTTL, httpTTL)
	c.now = func() time.Time { return time.Unix(0, 0) }
	return c
}

func newServer(id, transport string) store.MCPServer {
	return store.MCPServer{ID: id, Name: id, Transport: transport, Enabled: true, Command: "echo"}
}

func TestCache_GetMissRunsProbeAndStores(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	var calls atomic.Int32
	c.probe = func(ctx context.Context, server store.MCPServer) Result {
		calls.Add(1)
		return Result{ServerID: server.ID, Status: mcp.StatusReady, ToolCount: 7}
	}
	server := newServer("alpha", mcp.TransportStdio)

	got := c.Get(context.Background(), server, false)
	if got.Status != mcp.StatusReady || got.ToolCount != 7 {
		t.Errorf("first Get = %#v", got)
	}
	if calls.Load() != 1 {
		t.Errorf("probe call count = %d, want 1", calls.Load())
	}

	got = c.Get(context.Background(), server, false)
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
	c.probe = func(ctx context.Context, server store.MCPServer) Result {
		calls.Add(1)
		return Result{ServerID: server.ID, Status: mcp.StatusReady}
	}
	server := newServer("alpha", mcp.TransportStdio)

	_ = c.Get(context.Background(), server, false)
	_ = c.Get(context.Background(), server, true)
	if calls.Load() != 2 {
		t.Errorf("force=true should bypass cache; calls=%d", calls.Load())
	}
}

func TestCache_InvalidateDropsEntry(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	var calls atomic.Int32
	c.probe = func(ctx context.Context, server store.MCPServer) Result {
		calls.Add(1)
		return Result{ServerID: server.ID, Status: mcp.StatusReady}
	}
	server := newServer("alpha", mcp.TransportStdio)

	_ = c.Get(context.Background(), server, false)
	c.Invalidate(server.ID)
	_ = c.Get(context.Background(), server, false)
	if calls.Load() != 2 {
		t.Errorf("invalidate did not force re-probe; calls=%d", calls.Load())
	}
}

func TestCache_InvalidateAllDropsEverything(t *testing.T) {
	c := newTestCache(time.Hour, time.Hour)
	c.probe = func(ctx context.Context, server store.MCPServer) Result {
		return Result{ServerID: server.ID, Status: mcp.StatusReady}
	}
	_ = c.Get(context.Background(), newServer("a", mcp.TransportStdio), false)
	_ = c.Get(context.Background(), newServer("b", mcp.TransportHTTP), false)
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
	c.probe = func(ctx context.Context, server store.MCPServer) Result {
		calls.Add(1)
		return Result{ServerID: server.ID, Status: mcp.StatusReady}
	}
	server := newServer("alpha", mcp.TransportStdio)

	_ = c.Get(context.Background(), server, false)
	if calls.Load() != 1 {
		t.Fatalf("first Get call count: %d", calls.Load())
	}

	// Advance past the TTL — next Get should run a fresh probe.
	now.Add(int64(2 * time.Second))
	_ = c.Get(context.Background(), server, false)
	if calls.Load() != 2 {
		t.Errorf("post-expiry Get should re-probe; calls=%d", calls.Load())
	}
}

func TestCache_SnapshotFiltersExpired(t *testing.T) {
	c := newTestCache(time.Second, time.Second)
	var now atomic.Int64
	now.Store(time.Unix(0, 0).UnixNano())
	c.now = func() time.Time { return time.Unix(0, now.Load()) }
	c.probe = func(ctx context.Context, server store.MCPServer) Result {
		return Result{ServerID: server.ID, Status: mcp.StatusReady}
	}
	_ = c.Get(context.Background(), newServer("a", mcp.TransportStdio), false)
	now.Add(int64(2 * time.Second))
	_ = c.Get(context.Background(), newServer("b", mcp.TransportHTTP), false)

	snap := c.Snapshot()
	if _, ok := snap["a"]; ok {
		t.Errorf("expired entry 'a' should not appear in snapshot")
	}
	if _, ok := snap["b"]; !ok {
		t.Errorf("fresh entry 'b' should appear in snapshot")
	}
}

func TestCache_TTLZeroDisablesCache(t *testing.T) {
	c := newTestCache(0, 0)
	var calls atomic.Int32
	c.probe = func(ctx context.Context, server store.MCPServer) Result {
		calls.Add(1)
		return Result{ServerID: server.ID, Status: mcp.StatusReady}
	}
	server := newServer("alpha", mcp.TransportStdio)
	_ = c.Get(context.Background(), server, false)
	_ = c.Get(context.Background(), server, false)
	if calls.Load() != 2 {
		t.Errorf("TTL=0 should disable cache, calls=%d", calls.Load())
	}
}

func TestProbe_DisabledServerReturnsUnknown(t *testing.T) {
	server := store.MCPServer{
		ID:        "a",
		Name:      "alpha",
		Transport: mcp.TransportStdio,
		Command:   "/bin/echo",
		Enabled:   false,
	}
	got := Probe(context.Background(), server)
	if got.Status != mcp.StatusUnknown {
		t.Errorf("disabled server status = %v, want unknown", got.Status)
	}
	if got.Error == "" {
		t.Errorf("disabled server should carry an explanation in Error")
	}
}

func TestProbe_UnsupportedTransportReturnsFailed(t *testing.T) {
	server := store.MCPServer{
		ID:        "a",
		Transport: "websocket",
		Enabled:   true,
	}
	got := Probe(context.Background(), server)
	if got.Status != mcp.StatusFailed {
		t.Errorf("unsupported transport status = %v, want failed", got.Status)
	}
}
