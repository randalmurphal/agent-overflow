// Package codexskills owns the caller-facing shape of a Codex skill and a
// TTL'd, single-flighted cache in front of the `skills/list` read that
// produces it. The read is never free — it either rides a live session's
// connection or costs a whole short-lived `codex app-server` process, and
// it re-scans the filesystem for every requested directory — so composer
// menus and settings panes must not fan out one lookup per render.
package codexskills

import (
	"context"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTTL is the lifetime of a successful entry. Skills are files on
	// disk, and the authoritative invalidation is the app-server's own
	// `skills/changed` notification (wired to Reset), so this TTL only has
	// to cover the windows where no session is live to send one — an
	// external edit while AO is idle, or a scope AO has never opened a
	// session in.
	DefaultTTL = 5 * time.Minute
	// DefaultErrorTTL keeps a failed lookup briefly so a burst of renders
	// shares one failure instead of one subprocess each, without masking
	// recovery for long.
	DefaultErrorTTL = 30 * time.Second
)

// Scope values as they appear on the wire (`SkillScope`, snake_case).
// Listed rather than typed as a closed Go enum because the set is
// upstream's to grow: an unknown scope must render as itself, not collapse
// into a default.
const (
	ScopeUser   = "user"
	ScopeRepo   = "repo"
	ScopeSystem = "system"
	ScopeAdmin  = "admin"
)

// Skill is one Codex skill as AO consumes it: the identity a composer
// menu needs plus the text an invocation seeds. It is deliberately a
// subset of `SkillMetadata` — icons, brand colour and tool dependencies
// are carried on the wire but have no AO surface, and decoding fields
// nothing renders invites a caller to depend on one we do not maintain.
//
// Name is the `$name` token Codex scans for in turn text and the `name`
// field of a structured skill input; Path is the absolute SKILL.md path
// that same structured input carries. Both are required for invocation,
// so a skill missing either is dropped at parse rather than offered.
type Skill struct {
	Name string `json:"name"`
	// Description is the model-facing description from SKILL.md. Always
	// present on the wire.
	Description string `json:"description"`
	// ShortDescription is the one-line human label, preferring the
	// SKILL.json interface over the legacy top-level field (upstream's own
	// stated preference order).
	ShortDescription string `json:"shortDescription,omitempty"`
	// DisplayName is the interface's human name; empty means "use Name".
	DisplayName string `json:"displayName,omitempty"`
	// DefaultPrompt is the text a UI pre-fills when the user picks the
	// skill from a menu. Empty is common and means "no suggestion".
	DefaultPrompt string `json:"defaultPrompt,omitempty"`
	// Path is the absolute path to the skill's SKILL.md.
	Path string `json:"path"`
	// Scope is where the skill came from (see the Scope* constants).
	Scope string `json:"scope"`
	// Enabled is false when the user has disabled the skill in Codex's own
	// config. Disabled skills are still returned so a UI can show them as
	// off rather than silently omitting them.
	Enabled bool `json:"enabled"`
	// PluginID preserves Codex's owning plugin when the skill came from one.
	// Older servers omit it.
	PluginID string `json:"pluginId,omitempty"`
}

// LoadError is one directory Codex could not read skills from. Carried
// rather than dropped: a permissions or parse failure that silently
// produced a shorter menu would be indistinguishable from having no
// skills there.
type LoadError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// CwdSkills is the `skills/list` answer for ONE working directory. Cwd
// echoes the requested directory, not the resolved absolute form, so it
// joins back to the request that asked for it.
type CwdSkills struct {
	Cwd    string      `json:"cwd"`
	Skills []Skill     `json:"skills"`
	Errors []LoadError `json:"errors"`
}

// Clone deep-copies both slices so a caller cannot mutate a cached entry
// later callers will be handed.
func (c CwdSkills) Clone() CwdSkills {
	if c.Skills != nil {
		skills := make([]Skill, len(c.Skills))
		copy(skills, c.Skills)
		c.Skills = skills
	}
	if c.Errors != nil {
		errs := make([]LoadError, len(c.Errors))
		copy(errs, c.Errors)
		c.Errors = errs
	}
	return c
}

// Key is the cache identity of a skills lookup, and the one place its
// shape is decided.
//
// Two dimensions, both load-bearing:
//
//   - binary, because a different codex build resolves a different bundled
//     skill set and a different config schema (same reason
//     internal/codexmodels keys on it);
//   - cwd, because skills are directory-scoped: the repo-scope tier comes
//     from the workspace itself, so two workspaces genuinely have
//     different answers and a cwd-less key would serve one project's
//     skills to another.
//
// The active account is deliberately NOT a dimension, which is the
// difference from internal/codexusage. Skills resolve from the canonical
// CODEX_HOME (every AO spawn unsets the override) plus the cwd's repo;
// switching logins replaces auth.json and nothing a skill scan reads. If
// AO ever starts pointing skill reads at a per-account home, this key has
// to grow with it.
func Key(binary, cwd string) string {
	return strings.TrimSpace(binary) + "\x00" + strings.TrimSpace(cwd)
}

// Fetch reads the skills for one working directory. The Cache never
// chooses HOW to read: the caller supplies a closure that prefers a live
// session's already-open connection and falls back to an ephemeral
// process, because only the App knows which sessions exist.
type Fetch func(ctx context.Context) (CwdSkills, error)

// Cache TTLs skills lookups per Key and single-flights concurrent calls.
// Construct with New; the zero value is unusable.
type Cache struct {
	mu       sync.Mutex
	ttl      time.Duration
	errorTTL time.Duration
	now      func() time.Time
	entries  map[string]entry
	inflight map[string]*load
	// generation is bumped by every Invalidate/Reset. A load that started
	// under an older generation still answers its waiters — that is the
	// answer they asked for — but does not populate the cache, so an
	// invalidation racing an in-flight read can never be undone by it.
	generation uint64
}

type entry struct {
	skills    CwdSkills
	err       error
	expiresAt time.Time
}

type load struct {
	done   chan struct{}
	skills CwdSkills
	err    error
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

// Get returns the cached skills for the key, calling fetch at most once
// per key across concurrent callers. Concurrent calls for the same key
// block on one in-flight lookup and share its result — including its
// error, so a failure cannot be silently converted into "this workspace
// has no skills" for the losers of the race. An empty skill list is a
// legitimate answer here, which is exactly why an error must never be
// allowed to look like one.
//
// The returned value is a defensive copy.
func (c *Cache) Get(ctx context.Context, key string, fetch Fetch) (CwdSkills, error) {
	return c.get(ctx, key, fetch, false)
}

// Refresh is Get with the cached entry ignored: it always calls fetch
// (still single-flighted, so a burst of refresh clicks collapses into one
// read) and repopulates the entry. This is the AO-side half of a forced
// reload; the Codex-side half is the `forceReload` flag the caller's Fetch
// closure sets, which bypasses the app-server's own on-disk skill cache.
func (c *Cache) Refresh(ctx context.Context, key string, fetch Fetch) (CwdSkills, error) {
	return c.get(ctx, key, fetch, true)
}

func (c *Cache) get(ctx context.Context, key string, fetch Fetch, bypass bool) (CwdSkills, error) {
	key = strings.TrimSpace(key)
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && !bypass && c.now().Before(e.expiresAt) {
		skills := e.skills.Clone()
		err := e.err
		c.mu.Unlock()
		return skills, err
	}
	if existing, ok := c.inflight[key]; ok {
		done := existing.done
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return CwdSkills{}, ctx.Err()
		case <-done:
			return existing.skills.Clone(), existing.err
		}
	}

	l := &load{done: make(chan struct{})}
	c.inflight[key] = l
	startedAt := c.generation
	c.mu.Unlock()

	skills, err := fetch(ctx)

	c.mu.Lock()
	if c.generation == startedAt {
		ttl := c.ttl
		if err != nil {
			ttl = c.errorTTL
		}
		c.entries[key] = entry{skills: skills.Clone(), err: err, expiresAt: c.now().Add(ttl)}
	}
	l.skills = skills
	l.err = err
	delete(c.inflight, key)
	c.mu.Unlock()
	close(l.done)
	return skills.Clone(), err
}

// Invalidate drops the cached entry for one key so the next Get refetches.
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, strings.TrimSpace(key))
	c.generation++
	c.mu.Unlock()
}

// Reset drops every cached entry. This is what `skills/changed` maps to:
// the notification carries no payload at all (upstream types it as an
// empty struct and documents it as an invalidation signal), so there is no
// scope to narrow the drop to — a skill file that moved between two
// watched roots would otherwise leave a stale entry behind under the key
// it left.
//
// In-flight lookups are not cancelled — their callers still want an
// answer — but their results are NOT cached, because a read that started
// before the change cannot be known to have observed it. Without that,
// a `skills/changed` arriving mid-read would be undone by the read it
// raced and the stale list would survive a full TTL.
func (c *Cache) Reset() {
	c.mu.Lock()
	c.entries = make(map[string]entry)
	c.generation++
	c.mu.Unlock()
}
