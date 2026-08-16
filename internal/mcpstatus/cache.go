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
	// errorFrom is the Source that PRODUCED status.Error, which can
	// differ from status.Source after a carry-forward: an error-less
	// probe that re-stores a notification's error keeps errorFrom =
	// notification, so the retention rule chains across any number of
	// consecutive probes instead of evaporating on the second one.
	// Zero when status.Error is empty.
	errorFrom Source
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

// Put writes the status into the cache, emits it on the bus, and
// returns the status as stored — callers that hand their input onward
// (the single-flight fetch paths) must return Put's result, not their
// input, or the wire answer and the cache disagree about the same
// entry. Callers (live-session and notification handlers, ephemeral
// fetcher) invoke Put with their own Source value so the UI can
// disclose how fresh the data is.
//
// One narrow carry-forward: an ephemeral probe carries no error string
// at all (a status list answers what state a server is in, never why),
// so a probe that merely AGREES a server is still failed would otherwise
// erase the cause a live provider reported — "failed: invalid_grant"
// collapsing into a bare "failed" the user cannot act on. When an
// error-less fetch lands on an entry whose error a notification or live
// session produced (errorFrom — carry-forwards preserve it, so the
// retention chains across any number of consecutive probes) with the
// SAME status, that error carries onto both the stored and the emitted
// status. Everything else stores verbatim: a status change, an incoming
// error, or an incoming source that is not a fetch.
//
// The retention is deliberately event-bounded, not time-bounded: it
// ends when a provider speaks again or the status changes, never by
// clock. The probe just CONFIRMED the failed state is current; aging
// the explanation out while the state persists would reintroduce the
// bare unactionable "failed" this rule exists to prevent. The residual
// risk — the cause changed while the status didn't — is bounded by the
// same events.
func (c *Cache) Put(s ServerStatus) ServerStatus {
	if s.CheckedAt.IsZero() {
		s.CheckedAt = c.now()
	}
	entry := cacheEntry{status: s}
	if s.Error != "" {
		entry.errorFrom = s.Source
	}
	c.mu.Lock()
	if prior, ok := c.entries[s.Key]; ok && retainsPriorError(prior, s) {
		entry.status.Error = prior.status.Error
		entry.errorFrom = prior.errorFrom
	}
	entry.storedAt = c.now()
	c.entries[s.Key] = entry
	c.mu.Unlock()
	c.bus.Emit(entry.status)
	return entry.status
}

// retainsPriorError reports whether incoming is an error-less probe of a
// state a provider already explained. See Put for why this is scoped to
// fetch-over-provider-explanation with an unchanged status.
func retainsPriorError(prior cacheEntry, incoming ServerStatus) bool {
	if incoming.Source != SourceEphemeralFetch || incoming.Error != "" {
		return false
	}
	if prior.status.Error == "" || prior.status.Status != incoming.Status {
		return false
	}
	return prior.errorFrom == SourceNotification || prior.errorFrom == SourceLiveSession
}

// Invalidate drops the entry for k and emits a sentinel
// StatusUnknown so subscribers can clear stale UI state immediately
// (CRUD edits, OAuth completion, etc.) without waiting for a refetch
// to arrive.
//
// The emission is UNCONDITIONAL — it does not depend on an entry
// having been present. The sentinel says "the authoritative answer
// for this server moved", which is a fact about the toggle/OAuth that
// just landed, not about this cache's bookkeeping; a caller that has
// never asked for the status is exactly the caller whose listing is
// most obviously wrong. It is also the frontend's ONE re-list trigger
// after a toggle (mcpServers.svelte.ts), so a cold cache silently
// eating the sentinel meant the menu never refreshed.
func (c *Cache) Invalidate(k Key) {
	c.mu.Lock()
	delete(c.entries, k)
	c.mu.Unlock()
	c.bus.Emit(ServerStatus{Key: k, Status: StatusUnknown, Source: SourceEphemeralFetch, CheckedAt: c.now()})
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

// CachedStatus is a snapshot entry annotated with TTL freshness, for
// callers that treat the cache as two signals: which servers EXIST
// (membership — a plugin server the config files cannot name doesn't
// stop existing because 30s passed) and what their last-known status
// was (fresh vs stale).
type CachedStatus struct {
	ServerStatus
	Fresh bool
}

// SnapshotProviderWithFreshness returns every entry for the provider —
// expired ones included — with Fresh reporting whether the entry is
// still within TTL. Backs the config-fallback MCP listing, where
// dropping an expired plugin entry would make the server flicker out
// of the menu until the next ephemeral fetch; a stale entry instead
// renders with its last-known status and triggers a background
// refresh.
func (c *Cache) SnapshotProviderWithFreshness(p Provider) []CachedStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	out := make([]CachedStatus, 0, len(c.entries))
	for k, entry := range c.entries {
		if k.Provider != p {
			continue
		}
		out = append(out, CachedStatus{
			ServerStatus: entry.status,
			Fresh:        c.ttl <= 0 || now.Sub(entry.storedAt) <= c.ttl,
		})
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
		// fetch result. Written back in place — including Put's
		// carry-forward — so the slice we hand back (and the one stored
		// on the flight for waiters) matches what landed in the cache.
		if results[i].Source == "" {
			results[i].Source = SourceEphemeralFetch
		}
		results[i] = c.Put(results[i])
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
		results[i] = c.Put(results[i])
	}
	flight.results = results
	flight.err = err
	close(flight.done)

	c.sfMu.Lock()
	delete(c.sfProvider, p)
	c.sfMu.Unlock()

	return cloneStatuses(results), err
}

// cloneStatuses copies in so every single-flight caller gets a slice it
// can sort/mutate without racing peers on the same flight.results
// backing array. Tools is the one reference field on ServerStatus, so
// it gets its own copy per entry — a shallow copy would hand every
// caller the same backing array, exactly the shared state the clone
// exists to prevent.
func cloneStatuses(in []ServerStatus) []ServerStatus {
	if in == nil {
		return nil
	}
	out := make([]ServerStatus, len(in))
	copy(out, in)
	for i := range out {
		if len(out[i].Tools) > 0 {
			out[i].Tools = append([]string(nil), out[i].Tools...)
		}
	}
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
