// Package codexmodels caches Codex's live `model/list` response per binary
// path. The entry records the launch it was read with (working directory
// and environment, because a launcher can pick a different build from
// either), and a read with another launch replaces it. The lookup spawns a
// Codex CLI subprocess, so settings pages and model menus would otherwise
// fan out one Codex process per render. The Cache TTLs the result and
// single-flights concurrent lookups of one launch so a flurry of UI calls
// collapses into one CLI spawn.
package codexmodels

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/procutil"
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

// Launch is how a lookup starts Codex. The application passes the
// person's configured Codex environment, which a session's app-server also
// starts with, so the catalog lists the build a session would run: a
// version manager's shim can pick the build from its environment or
// directory, not only from the binary path.
type Launch struct {
	Binary  string
	WorkDir string
	Env     map[string]string
}

// key identifies a launch: the binary, its working directory and a digest
// of its environment, which may hold a credential. NUL separates the fields
// because no path contains one.
func (l Launch) key() string {
	return strings.TrimSpace(l.Binary) + "\x00" + l.WorkDir + "\x00" + provider.EnvFingerprint(l.Env)
}

// binary is the entry's map key: one entry per binary path.
func (l Launch) binary() string { return strings.TrimSpace(l.Binary) }

// Lister is the seam tests use to stub out the CLI shell-out. Production
// wires it to codexprovider.ListModels.
type Lister func(ctx context.Context, launch Launch, reaper procutil.Reaper) ([]provider.ModelInfo, error)

// Cache holds one Codex model lookup per binary path, TTLs it, and
// single-flights concurrent calls of one launch. Construct with New; the
// zero value is unusable.
type Cache struct {
	mu   sync.Mutex
	ttl  time.Duration
	list Lister
	now  func() time.Time
	// entries is keyed by binary path; inflight by launch key, and an
	// in-flight load leaves it when it completes.
	entries  map[string]entry
	inflight map[string]*load
}

type entry struct {
	// launch is the key of the launch the entry was read with. A read
	// with another launch misses and replaces the entry.
	launch    string
	models    []provider.ModelInfo
	known     []provider.ModelInfo
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

// Get returns the cached model list for the launch, calling the lister
// at most once per launch across concurrent callers. Concurrent calls
// to Get for the same launch block on a single in-flight lookup and
// share its result. Failed lookups are cached briefly to prevent sequential
// effort and Fast-mode checks from repeating the same bounded subprocess.
//
// A non-empty binary is required; empty input falls through to the
// CodexBinaryPath default that codex.ListModels resolves internally.
// The returned slice is a defensive clone — callers may mutate it. reaper
// ends a lookup's process if the backend dies first.
func (c *Cache) Get(ctx context.Context, launch Launch, reaper procutil.Reaper) ([]provider.ModelInfo, error) {
	launch.Binary = strings.TrimSpace(launch.Binary)
	launch.Env = maps.Clone(launch.Env)
	key := launch.key()
	c.mu.Lock()
	if e, ok := c.entries[launch.binary()]; ok && e.launch == key && c.now().Before(e.expiresAt) {
		models := provider.CloneModels(e.models)
		c.mu.Unlock()
		return models, e.err
	}
	l, joined := c.inflight[key]
	if !joined {
		l = &load{done: make(chan struct{})}
		c.inflight[key] = l
		go c.run(context.WithoutCancel(ctx), key, launch, reaper, l)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return provider.CloneModels(l.models), l.err
	}
}

// run is one shared lookup. It belongs to no caller: it keeps the first
// caller's values but not its cancellation, ends at the lister's own
// bound, and each caller waits on its own context.
func (c *Cache) run(ctx context.Context, key string, launch Launch, reaper procutil.Reaper, l *load) {
	models, err := c.list(ctx, launch, reaper)
	cloned := provider.CloneModels(models)

	c.mu.Lock()
	l.models = cloned
	l.err = err
	// Reset detaches in-flight loads. A detached load still completes for
	// the callers already waiting on it, but it must not overwrite a newer
	// lookup that started after the reset.
	if c.inflight[key] == l {
		delete(c.inflight, key)
		// Remembered capabilities belong to the launch that observed them;
		// another launch's entry is replaced without them.
		var known []provider.ModelInfo
		if previous := c.entries[launch.binary()]; previous.launch == key {
			known = previous.known
		}
		if err == nil {
			known = rememberModels(known, models)
		}
		entryTTL := c.ttl
		if err != nil {
			entryTTL = DefaultErrorTTL
		}
		c.entries[launch.binary()] = entry{
			launch:    key,
			models:    provider.CloneModels(models),
			known:     known,
			err:       err,
			expiresAt: c.now().Add(entryTTL),
		}
	}
	close(l.done)
	c.mu.Unlock()
}

// KnownModel returns previously observed capabilities without waiting for a refresh.
// Expiry and refresh failures do not revoke that evidence; a read with another
// launch replaces it, and Reset clears it when the provider identity changes.
// The latest catalog returned by Get stays separate.
func (c *Cache) KnownModel(launch Launch, slug string) (provider.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[launch.binary()]
	if e.launch != launch.key() {
		return provider.ModelInfo{}, false
	}
	for _, model := range e.known {
		if model.Slug == slug {
			return provider.CloneModelInfo(model), true
		}
	}
	return provider.ModelInfo{}, false
}

// Bound historical evidence by the same model limit as the provider catalog.
// New entries take precedence when the limit is reached.
func rememberModels(previous, current []provider.ModelInfo) []provider.ModelInfo {
	const limit = 1000
	result := provider.CloneModels(current)
	if len(result) > limit {
		result = result[:limit:limit]
	}
	indices := make(map[string]int, len(result))
	for i, model := range result {
		indices[model.Slug] = i
	}
	for _, old := range previous {
		index, found := indices[old.Slug]
		if !found {
			if len(result) < limit {
				result = append(result, provider.CloneModelInfo(old))
			}
			continue
		}
		model := &result[index]
		for _, capability := range old.Capabilities {
			if !slices.Contains(model.Capabilities, capability) {
				model.Capabilities = append(model.Capabilities, capability)
			}
		}
		if model.FastModeTier == nil && old.FastModeTier != nil {
			tier := *old.FastModeTier
			model.FastModeTier = &tier
		}
		for _, effort := range old.ReasoningEfforts {
			if !slices.ContainsFunc(model.ReasoningEfforts, func(v provider.ReasoningEffortOption) bool { return v.Slug == effort.Slug }) {
				effort.Default = false
				model.ReasoningEfforts = append(model.ReasoningEfforts, effort)
			}
		}
		for _, window := range old.ContextWindows {
			if !slices.ContainsFunc(model.ContextWindows, func(v provider.ContextWindowOption) bool { return v.Tokens == window.Tokens }) {
				window.Default = false
				model.ContextWindows = append(model.ContextWindows, window)
			}
		}
	}
	return result
}

// Peek returns a fresh cached result without starting or waiting for a model
// lookup. The final return value is false for a miss, an expired entry, or a
// lookup that is still in flight. Cached errors are returned as hits so callers
// can use the same fallback they would use after Get without retrying the CLI.
func (c *Cache) Peek(launch Launch) ([]provider.ModelInfo, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[launch.binary()]
	if !ok || e.launch != launch.key() || !c.now().Before(e.expiresAt) {
		return nil, nil, false
	}
	return provider.CloneModels(e.models), e.err, true
}

// Reset drops every cached entry and detaches in-flight lookups. Detached
// lookups finish for their existing callers but cannot populate the cache or
// capture later callers. The next Get always starts a fresh lookup.
func (c *Cache) Reset() {
	c.mu.Lock()
	c.entries = make(map[string]entry)
	c.inflight = make(map[string]*load)
	c.mu.Unlock()
}

func defaultLister(ctx context.Context, launch Launch, reaper procutil.Reaper) ([]provider.ModelInfo, error) {
	return codexprovider.ListModels(ctx, codexprovider.ModelListConfig{
		Binary:  launch.Binary,
		WorkDir: launch.WorkDir,
		Env:     launch.Env,
		Reaper:  reaper,
	})
}
