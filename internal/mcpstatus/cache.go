package mcpstatus

import (
	"context"
	"sync"
	"time"
)

// Cache holds the most recent ServerStatus per Key, behind a TTL the
// caller chooses at construction. Updates land from three sources:
//   - live provider sessions feeding the cache via app-level wiring,
//   - on-demand ephemeral fetchers driven by GetOrFetch,
//   - explicit Put / Invalidate calls (CRUD + OAuth completion).
//
// Per-key single-flight ensures two callers asking for the same
// inactive-thread status produce one fetcher invocation.
//
// The cache is per-App (not process-global); see internal/CLAUDE.md's
// no-globals stance.
type Cache struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	entries map[Key]cacheEntry
	bus     EventBus

	sfMu       sync.Mutex
	sf         map[Key]*inflight
	sfProvider map[Provider]*inflight
}

type cacheEntry struct {
	status   ServerStatus
	storedAt time.Time
}

type inflight struct {
	done    chan struct{}
	results []ServerStatus
	err     error
}

// NewCache constructs a Cache pinned to wall-clock time. ttl ≤ 0
// disables freshness expiry (every Get hits the wire), used only in
// tests. bus may be nil, in which case Put / Invalidate emissions are
// dropped.
func NewCache(ttl time.Duration, bus EventBus) *Cache {
	return NewWith(ttl, bus, time.Now)
}

// NewWith is the test-friendly constructor: it accepts an injectable
// clock. Production code uses NewCache; tests that need deterministic
// TTL behavior pass a controlled time function. Matches the codebase
// pattern in internal/codexmodels.NewWith.
func NewWith(ttl time.Duration, bus EventBus, now func() time.Time) *Cache {
	if bus == nil {
		bus = nullBus{}
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{
		now:        now,
		ttl:        ttl,
		entries:    map[Key]cacheEntry{},
		bus:        bus,
		sf:         map[Key]*inflight{},
		sfProvider: map[Provider]*inflight{},
	}
}

// Get returns the cached entry for k if it's still fresh under the
// configured TTL. ok=false means "no fresh entry" — either missing
// or stale. Stale entries are NOT auto-evicted on read; they sit
// until overwritten so a refetch failure leaves the prior value in
// place rather than blanking the UI.
func (c *Cache) Get(k Key) (ServerStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[k]
	if !ok {
		return ServerStatus{}, false
	}
	if c.ttl > 0 && c.now().Sub(entry.storedAt) > c.ttl {
		return ServerStatus{}, false
	}
	return entry.status, true
}

// Put writes the status into the cache and emits it on the bus.
// Callers (live-session and notification handlers, ephemeral fetcher)
// invoke Put with their own Source value so the UI can disclose how
// fresh the data is.
func (c *Cache) Put(s ServerStatus) {
	if s.CheckedAt.IsZero() {
		s.CheckedAt = c.now()
	}
	c.mu.Lock()
	c.entries[s.Key] = cacheEntry{status: s, storedAt: c.now()}
	c.mu.Unlock()
	c.bus.Emit(s)
}

// Invalidate drops the entry for k and emits a sentinel
// StatusUnknown so subscribers can clear stale UI state immediately
// (CRUD edits, OAuth completion, etc.) without waiting for a refetch
// to arrive.
func (c *Cache) Invalidate(k Key) {
	c.mu.Lock()
	_, had := c.entries[k]
	delete(c.entries, k)
	c.mu.Unlock()
	if had {
		c.bus.Emit(ServerStatus{Key: k, Status: StatusUnknown, Source: SourceEphemeralFetch, CheckedAt: c.now()})
	}
}

// InvalidateProvider drops every entry for the given provider. Used
// when the user adds/removes a server in Settings — the per-server
// list shifts so all known statuses are momentarily stale.
func (c *Cache) InvalidateProvider(p Provider) {
	c.mu.Lock()
	dropped := []Key{}
	for k := range c.entries {
		if k.Provider == p {
			dropped = append(dropped, k)
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
	for _, k := range dropped {
		c.bus.Emit(ServerStatus{Key: k, Status: StatusUnknown, Source: SourceEphemeralFetch, CheckedAt: c.now()})
	}
}

// Snapshot returns every fresh entry as a slice. Order is unspecified.
// Used by the popup/settings to render the cached state on open
// before any explicit refresh fires.
func (c *Cache) Snapshot() []ServerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	out := make([]ServerStatus, 0, len(c.entries))
	for _, entry := range c.entries {
		if c.ttl > 0 && now.Sub(entry.storedAt) > c.ttl {
			continue
		}
		out = append(out, entry.status)
	}
	return out
}

// SnapshotProvider is the per-provider variant of Snapshot. Used by
// ListMcpServerStatuses bindings that scope to one provider.
func (c *Cache) SnapshotProvider(p Provider) []ServerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	out := make([]ServerStatus, 0, len(c.entries))
	for k, entry := range c.entries {
		if k.Provider != p {
			continue
		}
		if c.ttl > 0 && now.Sub(entry.storedAt) > c.ttl {
			continue
		}
		out = append(out, entry.status)
	}
	return out
}

// Fetcher runs a provider-side status fetch and returns every server
// status it can see. The cache treats the slice as authoritative for
// that provider: each entry is Put, and the requested Key's result is
// returned to the caller of GetOrFetch.
type Fetcher interface {
	Fetch(ctx context.Context, provider Provider) ([]ServerStatus, error)
}

// GetOrFetch is the single-flight entry point: if the cache has a
// fresh entry for k it returns immediately; otherwise it invokes
// fetcher.Fetch(provider) at most once even across concurrent callers
// for the same key, Puts every returned entry, and resolves k's
// status. force=true bypasses the cache hit but still single-flights.
func (c *Cache) GetOrFetch(ctx context.Context, k Key, fetcher Fetcher, force bool) (ServerStatus, error) {
	if !force {
		if cached, ok := c.Get(k); ok {
			return cached, nil
		}
	}

	c.sfMu.Lock()
	if existing, ok := c.sf[k]; ok {
		c.sfMu.Unlock()
		select {
		case <-existing.done:
		case <-ctx.Done():
			return ServerStatus{}, ctx.Err()
		}
		return pickResult(existing.results, k), existing.err
	}
	flight := &inflight{done: make(chan struct{})}
	c.sf[k] = flight
	c.sfMu.Unlock()

	results, err := fetcher.Fetch(ctx, k.Provider)
	for i := range results {
		// Stamp source so callers can't accidentally Put a Source-less
		// fetch result. In-place so the slice we hand back (and the
		// one stored on the flight for waiters) matches what landed in
		// the cache.
		if results[i].Source == "" {
			results[i].Source = SourceEphemeralFetch
		}
		c.Put(results[i])
	}
	flight.results = results
	flight.err = err
	close(flight.done)

	c.sfMu.Lock()
	delete(c.sf, k)
	c.sfMu.Unlock()

	return pickResult(results, k), err
}

// RefreshProvider unconditionally re-fetches the provider's whole
// status set under a per-Provider single-flight gate. Unlike
// GetOrFetch, it never reads the cache — callers reach for it when
// they want a forced refresh (the popup's auto-fetch on open, the
// explicit Refresh button). Concurrent callers collapse to one
// fetcher invocation and each receives an INDEPENDENT slice copy so
// caller-side mutation (e.g. sort.Slice) doesn't race with peers.
// Every returned entry is Put so subscribers see the live update.
func (c *Cache) RefreshProvider(ctx context.Context, p Provider, fetcher Fetcher) ([]ServerStatus, error) {
	c.sfMu.Lock()
	if existing, ok := c.sfProvider[p]; ok {
		c.sfMu.Unlock()
		select {
		case <-existing.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return cloneStatuses(existing.results), existing.err
	}
	flight := &inflight{done: make(chan struct{})}
	c.sfProvider[p] = flight
	c.sfMu.Unlock()

	results, err := fetcher.Fetch(ctx, p)
	for i := range results {
		if results[i].Source == "" {
			results[i].Source = SourceEphemeralFetch
		}
		c.Put(results[i])
	}
	flight.results = results
	flight.err = err
	close(flight.done)

	c.sfMu.Lock()
	delete(c.sfProvider, p)
	c.sfMu.Unlock()

	return cloneStatuses(results), err
}

// cloneStatuses returns a shallow copy of in. ServerStatus is all
// value-typed fields (no slices or maps), so shallow copy is enough
// to give every single-flight caller a slice they can sort/mutate
// without racing peers reading from the same flight.results backing
// array.
func cloneStatuses(in []ServerStatus) []ServerStatus {
	if in == nil {
		return nil
	}
	out := make([]ServerStatus, len(in))
	copy(out, in)
	return out
}

func pickResult(results []ServerStatus, k Key) ServerStatus {
	for _, s := range results {
		if s.Key == k {
			return s
		}
	}
	return ServerStatus{Key: k, Status: StatusUnknown, Source: SourceEphemeralFetch}
}
