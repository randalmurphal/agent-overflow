package provider

import (
	"sync"
	"time"
)

// ProbeCache stores recent AccountInfo results keyed by binary path.
// Results older than TTL are considered stale. Safe for concurrent use.
//
// Provider-agnostic: keys are binary paths (strings) and values are
// the shared `AccountInfo` shape. Both Claude and Codex probes
// instantiate one to memoize zero-token startup probes — the cache
// itself has no provider-specific behavior, so a single implementation
// here keeps the two probe packages in sync as the cache contract
// evolves (eviction hooks, observability, error caching).
type ProbeCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]probeCacheEntry
}

type probeCacheEntry struct {
	info    AccountInfo
	storeAt time.Time
}

// NewProbeCache returns a fresh cache with the given entry lifetime.
func NewProbeCache(ttl time.Duration) *ProbeCache {
	return &ProbeCache{
		ttl:     ttl,
		entries: make(map[string]probeCacheEntry),
	}
}

// Get returns a cached AccountInfo for the binary path, if present and
// not expired. Stale entries are deleted on read so the cache stays
// bounded under heavy expiration.
func (c *ProbeCache) Get(key string) (AccountInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return AccountInfo{}, false
	}
	if time.Since(entry.storeAt) > c.ttl {
		delete(c.entries, key)
		return AccountInfo{}, false
	}
	return entry.info, true
}

// Set stores an AccountInfo under the given binary path key.
func (c *ProbeCache) Set(key string, info AccountInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = probeCacheEntry{info: info, storeAt: time.Now()}
}

// Invalidate removes the entry for a single key. Called from
// user-initiated recheck paths (e.g. the auth banner's "Recheck Auth"
// button) so the next probe sees fresh authentication state instead
// of the cached pre-login zero-value. No-op when the key is absent.
func (c *ProbeCache) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key)
}
