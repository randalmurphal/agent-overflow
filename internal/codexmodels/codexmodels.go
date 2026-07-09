// Package codexmodels caches Codex's live `model/list` response per
// binary path. The lookup spawns a Codex CLI subprocess, so settings
// pages and model menus would otherwise fan out one Codex process per
// render. The Cache TTLs the result and single-flights concurrent
// lookups against the same binary so a flurry of UI calls collapses
// into one CLI spawn.
package codexmodels

import (
	"context"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/provider"
	codexprovider "agent-overflow/internal/provider/codex"
)

// DefaultTTL is the lifetime of a successful catalog entry. Five
// minutes keeps the cache responsive to binary swaps in settings while
// still letting the model picker render without spawning a CLI for
// every render pass. Failures use a much shorter TTL so paired capability
// checks share one failed subprocess without masking recovery for long.
const (
	DefaultTTL      = 5 * time.Minute
	DefaultErrorTTL = 15 * time.Second
)

// Lister is the seam tests use to stub out the CLI shell-out. Production
// wires it to codexprovider.ListModels.
type Lister func(ctx context.Context, binary string) ([]provider.ModelInfo, error)

// Cache TTLs Codex model lookups per binary path and single-flights
// concurrent calls. Construct with New; the zero value is unusable.
type Cache struct {
	mu       sync.Mutex
	ttl      time.Duration
	list     Lister
	now      func() time.Time
	entries  map[string]entry
	inflight map[string]*load
}

type entry struct {
	models    []provider.ModelInfo
	err       error
	expiresAt time.Time
}

type load struct {
	done   chan struct{}
	models []provider.ModelInfo
	err    error
}

// New returns a Cache with the standard codex.ListModels wiring and the
// default TTL.
func New() *Cache {
	return NewWith(DefaultTTL, defaultLister, time.Now)
}

// NewWith returns a Cache wired with a custom TTL, lister, and clock.
// Used by tests to bypass the CLI spawn and exercise the cache
// directly.
func NewWith(ttl time.Duration, list Lister, now func() time.Time) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if list == nil {
		list = defaultLister
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{
		ttl:      ttl,
		list:     list,
		now:      now,
		entries:  make(map[string]entry),
		inflight: make(map[string]*load),
	}
}

// Get returns the cached model list for the binary, calling the lister
// at most once per binary across concurrent callers. Concurrent calls
// to Get for the same binary block on a single in-flight lookup and
// share its result. Failed lookups are cached briefly to prevent sequential
// effort and Fast-mode checks from repeating the same bounded subprocess.
//
// A non-empty binary is required; empty input falls through to the
// CodexBinaryPath default that codex.ListModels resolves internally.
// The returned slice is a defensive clone — callers may mutate it.
func (c *Cache) Get(ctx context.Context, binary string) ([]provider.ModelInfo, error) {
	binary = strings.TrimSpace(binary)
	for {
		c.mu.Lock()
		if e, ok := c.entries[binary]; ok && c.now().Before(e.expiresAt) {
			models := cloneModels(e.models)
			c.mu.Unlock()
			return models, e.err
		}
		if existing, ok := c.inflight[binary]; ok {
			done := existing.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				return cloneModels(existing.models), existing.err
			}
		}

		l := &load{done: make(chan struct{})}
		c.inflight[binary] = l
		c.mu.Unlock()

		models, err := c.list(ctx, binary)
		cloned := cloneModels(models)

		c.mu.Lock()
		l.models = cloned
		l.err = err
		delete(c.inflight, binary)
		entryTTL := c.ttl
		if err != nil {
			entryTTL = DefaultErrorTTL
		}
		c.entries[binary] = entry{
			models:    cloneModels(models),
			err:       err,
			expiresAt: c.now().Add(entryTTL),
		}
		close(l.done)
		c.mu.Unlock()

		return cloneModels(models), err
	}
}

// Reset drops every cached entry. In-flight lookups are not cancelled —
// they finish but their results are not stored. Callers use this after
// the user changes the Codex binary path so the next Get spawns a
// fresh lookup against the new binary.
func (c *Cache) Reset() {
	c.mu.Lock()
	c.entries = make(map[string]entry)
	c.mu.Unlock()
}

func defaultLister(ctx context.Context, binary string) ([]provider.ModelInfo, error) {
	return codexprovider.ListModels(ctx, codexprovider.ModelListConfig{Binary: binary})
}

func cloneModels(models []provider.ModelInfo) []provider.ModelInfo {
	if models == nil {
		return nil
	}
	cloned := make([]provider.ModelInfo, len(models))
	for i, m := range models {
		cloned[i] = m
		cloned[i].Capabilities = append([]string(nil), m.Capabilities...)
		cloned[i].ContextWindows = append([]provider.ContextWindowOption(nil), m.ContextWindows...)
		cloned[i].ReasoningEfforts = append([]provider.ReasoningEffortOption(nil), m.ReasoningEfforts...)
	}
	return cloned
}
