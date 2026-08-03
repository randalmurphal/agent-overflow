// Package codexusage caches Codex's account-level token-usage report
// (`account/usage/read`). The read is not local — the app-server forwards it
// to the ChatGPT backend — and it may cost a whole subprocess when no Codex
// session is live, so opening the usage overlay must not fan out one fetch
// per render or per component. The Cache TTLs the answer per account and
// single-flights concurrent lookups against the same key.
package codexusage

import (
	"context"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider/codex"
)

const (
	// DefaultTTL is the lifetime of a successful entry. The report is a
	// per-day rollup plus lifetime totals, so it moves on the order of hours;
	// five minutes keeps a manual refresh responsive without making the
	// overlay a traffic generator.
	DefaultTTL = 5 * time.Minute
	// DefaultErrorTTL keeps a failed lookup briefly so a burst of renders
	// shares one failure, without masking recovery for long. It also covers
	// the "this codex has no such method" answer, which cannot change under a
	// running binary but can change under an upgrade.
	DefaultErrorTTL = 30 * time.Second
)

// Fetch reads the account usage report. The Cache never chooses HOW to read —
// the caller supplies a closure that prefers a live session's connection and
// falls back to an ephemeral process, because only the App knows which
// sessions exist.
type Fetch func(ctx context.Context) (codex.AccountUsage, error)

// Cache TTLs account usage lookups per key and single-flights concurrent
// calls. Construct with New; the zero value is unusable.
//
// The key is the caller's identity for "whose usage is this" — the binary
// plus the active account id. Dropping the account dimension would serve one
// login's lifetime totals as another's after an account switch.
type Cache struct {
	mu       sync.Mutex
	ttl      time.Duration
	errorTTL time.Duration
	now      func() time.Time
	entries  map[string]entry
	inflight map[string]*load
}

type entry struct {
	usage     codex.AccountUsage
	err       error
	expiresAt time.Time
}

type load struct {
	done  chan struct{}
	usage codex.AccountUsage
	err   error
}

// New returns a Cache with the default TTLs and the real clock.
func New() *Cache { return NewWith(DefaultTTL, DefaultErrorTTL, time.Now) }

// NewWith returns a Cache wired with custom TTLs and clock, for tests.
func NewWith(ttl, errorTTL time.Duration, now func() time.Time) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if errorTTL <= 0 {
		errorTTL = DefaultErrorTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{
		ttl:      ttl,
		errorTTL: errorTTL,
		now:      now,
		entries:  make(map[string]entry),
		inflight: make(map[string]*load),
	}
}

// Get returns the cached report for the key, calling fetch at most once per
// key across concurrent callers. Concurrent calls for the same key block on
// one in-flight lookup and share its result — including its error, so a
// failure cannot be silently converted into an empty report for the losers of
// the race.
//
// The returned value is a defensive copy: the daily-bucket slice is cloned so
// a caller cannot mutate the cached entry other callers will be handed.
func (c *Cache) Get(ctx context.Context, key string, fetch Fetch) (codex.AccountUsage, error) {
	key = strings.TrimSpace(key)
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expiresAt) {
		usage := cloneUsage(e.usage)
		err := e.err
		c.mu.Unlock()
		return usage, err
	}
	if existing, ok := c.inflight[key]; ok {
		done := existing.done
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return codex.AccountUsage{}, ctx.Err()
		case <-done:
			return cloneUsage(existing.usage), existing.err
		}
	}

	l := &load{done: make(chan struct{})}
	c.inflight[key] = l
	c.mu.Unlock()

	usage, err := fetch(ctx)

	c.mu.Lock()
	ttl := c.ttl
	if err != nil {
		ttl = c.errorTTL
	}
	c.entries[key] = entry{usage: cloneUsage(usage), err: err, expiresAt: c.now().Add(ttl)}
	l.usage = usage
	l.err = err
	delete(c.inflight, key)
	c.mu.Unlock()
	close(l.done)
	return cloneUsage(usage), err
}

// Invalidate drops the cached entry for a key so the next Get refetches. Used
// by the manual refresh path and after an account switch.
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, strings.TrimSpace(key))
	c.mu.Unlock()
}

// cloneUsage deep-copies every reference field — the bucket slice AND the
// optional summary pointers. Handing out the cached pointers would let one
// caller rewrite the number every later caller reads; the copy makes that
// structurally impossible rather than relying on nobody trying.
func cloneUsage(usage codex.AccountUsage) codex.AccountUsage {
	usage.LifetimeTokens = cloneInt64(usage.LifetimeTokens)
	usage.PeakDailyTokens = cloneInt64(usage.PeakDailyTokens)
	usage.LongestRunningTurnSec = cloneInt64(usage.LongestRunningTurnSec)
	usage.CurrentStreakDays = cloneInt64(usage.CurrentStreakDays)
	usage.LongestStreakDays = cloneInt64(usage.LongestStreakDays)
	if usage.DailyBuckets != nil {
		buckets := make([]codex.AccountUsageDailyBucket, len(usage.DailyBuckets))
		copy(buckets, usage.DailyBuckets)
		usage.DailyBuckets = buckets
	}
	return usage
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
