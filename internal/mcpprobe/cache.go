package mcpprobe

import (
	"context"
	"sync"
	"time"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/store"
)

// Cache holds the most recent Result per library server id, behind a
// TTL the App sets at construction time. Hits skip the handshake;
// misses run Probe and store the answer. The cache is process-global
// in practice (one instance lives on App) and explicit-invalidate-on-
// write — TTL is the safety net, not the freshness contract.
type Cache struct {
	mu     sync.Mutex
	now    func() time.Time
	probe  func(ctx context.Context, server store.MCPServer) Result
	stdio  time.Duration
	http   time.Duration
	values map[string]cacheEntry
}

type cacheEntry struct {
	result    Result
	transport string
	storedAt  time.Time
}

// NewCache wires a Cache with the provided per-transport TTLs. Pass
// non-positive durations to disable cache reads for that transport
// (every call hits the probe).
func NewCache(stdioTTL, httpTTL time.Duration) *Cache {
	return &Cache{
		now:    time.Now,
		probe:  Probe,
		stdio:  stdioTTL,
		http:   httpTTL,
		values: map[string]cacheEntry{},
	}
}

// Get returns the cached result for the server if one is still fresh,
// otherwise runs the probe and stores it. Pass force=true to bypass
// the cache and always run a fresh probe — used by the user-facing
// "Refresh" button.
func (c *Cache) Get(ctx context.Context, server store.MCPServer, force bool) Result {
	if !force {
		if cached, ok := c.lookup(server); ok {
			return cached
		}
	}
	result := c.probe(ctx, server)
	c.store(server, result)
	return result
}

// Invalidate drops the cached entry for one server. Callers MUST
// invalidate after editing the library row or after the OAuth flow
// completes — otherwise the popup would show stale "ready" against a
// now-broken config.
func (c *Cache) Invalidate(serverID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, serverID)
}

// InvalidateAll drops every entry. Used on settings imports or app
// shutdown housekeeping.
func (c *Cache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = map[string]cacheEntry{}
}

// Snapshot returns a copy of every fresh cached result keyed by
// server id. Used by the binding that hydrates the popup on first
// open before any forced probe runs.
func (c *Cache) Snapshot() map[string]Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	out := make(map[string]Result, len(c.values))
	for id, entry := range c.values {
		ttl := c.ttlFor(entry.transport)
		if ttl > 0 && now.Sub(entry.storedAt) > ttl {
			continue
		}
		out[id] = entry.result
	}
	return out
}

func (c *Cache) lookup(server store.MCPServer) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.values[server.ID]
	if !ok {
		return Result{}, false
	}
	ttl := c.ttlFor(server.Transport)
	if ttl <= 0 {
		return Result{}, false
	}
	if c.now().Sub(entry.storedAt) > ttl {
		delete(c.values, server.ID)
		return Result{}, false
	}
	return entry.result, true
}

func (c *Cache) store(server store.MCPServer, result Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[server.ID] = cacheEntry{
		result:    result,
		transport: server.Transport,
		storedAt:  c.now(),
	}
}

func (c *Cache) ttlFor(transport string) time.Duration {
	switch transport {
	case mcp.TransportStdio:
		return c.stdio
	case mcp.TransportHTTP, mcp.TransportSSE:
		return c.http
	default:
		return 0
	}
}
