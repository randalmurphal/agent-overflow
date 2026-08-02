package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// ProbeCacheKey identifies one probe answer. Every dimension a probe's
// result can legitimately depend on belongs here, because the cost of
// omitting one is serving another environment's identity:
//
//   - Binary — a second CLI install can be a different version, or point
//     at a different backend entirely.
//   - AccountID — the managed account whose credentials are installed.
//     "" means unmanaged (no account store attached).
//   - WorkDir — the directory the probe subprocess runs in. Both CLIs
//     discover project-scoped configuration by walking up from their cwd,
//     and a project `.claude/settings.json` env block can redirect the CLI
//     to an entirely different backend (CLAUDE_CODE_USE_BEDROCK and
//     friends). A cwd-blind key would let one project's answer stand in
//     for every other project's, and for the canonical home.
//   - EnvFingerprint — a digest of the user's custom environment for the
//     provider (settings.ProviderEnvVars). ANTHROPIC_BASE_URL and a proxy
//     configuration decide which backend answers "who am I", which is the
//     same class of dimension as WorkDir: without it, editing the base URL
//     would keep serving the previous backend's identity for the rest of the
//     TTL while sessions already ran against the new one. A digest rather
//     than the values because the encoded key is a long-lived in-memory map
//     key and a custom environment may hold a credential.
//
// Field-tagged struct rather than a hand-rolled string: adding a dimension
// later is one edit here plus a compile error at any site still building
// the old encoding, instead of a silent key collision.
type ProbeCacheKey struct {
	Binary         string
	AccountID      string
	WorkDir        string
	EnvFingerprint string
}

// String renders the map key. NUL separators because no path, binary, or
// account id can contain one, so no two distinct keys can collide by
// concatenation. Every field is written verbatim — an empty AccountID
// encodes as empty rather than as a placeholder word, so no real account
// id can ever be mistaken for "no managed account".
func (k ProbeCacheKey) String() string {
	var b strings.Builder
	b.Grow(len(k.Binary) + len(k.AccountID) + len(k.WorkDir) + len(k.EnvFingerprint) + 32)
	b.WriteString(k.Binary)
	b.WriteString("\x00account=")
	b.WriteString(k.AccountID)
	b.WriteString("\x00cwd=")
	b.WriteString(k.WorkDir)
	b.WriteString("\x00env=")
	b.WriteString(k.EnvFingerprint)
	return b.String()
}

// EnvFingerprint digests an override environment into the stable, secret-free
// value ProbeCacheKey carries. Sorted so map iteration order can't produce two
// fingerprints for one environment, and length-prefixed so no pair of
// name/value splits can encode to the same bytes. An empty or nil environment
// fingerprints as "" so keys built before this dimension existed keep their
// encoding.
func EnvFingerprint(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%d:%s=%d:%s\n", len(name), name, len(env[name]), env[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ProbeCache stores recent AccountInfo results keyed by ProbeCacheKey.
// Results older than TTL are considered stale. Safe for concurrent use.
//
// Provider-agnostic: values are the shared `AccountInfo` shape. Both
// Claude and Codex probes instantiate one to memoize zero-token startup
// probes — the cache itself has no provider-specific behavior, so a single
// implementation here keeps the two probe packages in sync as the cache
// contract evolves (eviction hooks, observability, error caching).
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

// Get returns a cached AccountInfo for the key, if present and not
// expired. Stale entries are deleted on read so the cache stays bounded
// under heavy expiration.
func (c *ProbeCache) Get(key ProbeCacheKey) (AccountInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	encoded := key.String()
	entry, ok := c.entries[encoded]
	if !ok {
		return AccountInfo{}, false
	}
	if time.Since(entry.storeAt) > c.ttl {
		delete(c.entries, encoded)
		return AccountInfo{}, false
	}
	return entry.info, true
}

// Set stores an AccountInfo under the given key.
func (c *ProbeCache) Set(key ProbeCacheKey, info AccountInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key.String()] = probeCacheEntry{info: info, storeAt: time.Now()}
}

// Invalidate removes the entry for a single key. Called from
// user-initiated recheck paths (e.g. the auth banner's "Recheck Auth"
// button) so the next probe sees fresh authentication state instead
// of the cached pre-login zero-value. No-op when the key is absent.
func (c *ProbeCache) Invalidate(key ProbeCacheKey) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key.String())
}
