// Package claudecommands holds the list of provider-executed slash commands the
// Claude CLI reports on its zero-token `initialize` probe, per probe identity.
//
// Unlike internal/claudemodels there is no merge policy: AO ships no command
// catalog of its own, so the wire list is the whole answer and every probe
// replaces it wholesale. See AGENTS.md in this directory.
package claudecommands

import (
	"sync"

	"agent-overflow/internal/provider"
)

// maxCacheEntries bounds the map. Keys vary with binary, account, workdir, and
// custom environment; only a real probe (one subprocess, cached for minutes)
// can mint one, so a small cap is generous. The oldest write is evicted, which
// at worst costs that identity its command list until its next probe.
const maxCacheEntries = 8

// Cache holds one probe's command list per probe identity.
//
// Keyed by provider.ProbeCacheKey for the same reason internal/claudemodels is:
// which binary ran, whose credentials it held, from which directory, under
// which custom environment. A user's project-scoped commands live in the
// workdir, and a plugin's live under the credentialed home, so serving one
// identity's list to another would offer commands that do not exist there.
//
// Deliberately NOT tied to the probe cache's TTL or its invalidations — a
// command list has no correctness deadline, dropping it while identity is
// rechecked would empty an open menu for the seconds a re-probe takes, and
// every probe replaces the entry wholesale anyway. The entry count is capped
// instead.
type Cache struct {
	mu      sync.Mutex
	entries map[string][]provider.SlashCommand
	order   []string
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{entries: make(map[string][]provider.SlashCommand)}
}

// Store records what one probe reported under its identity.
//
// wireErr is the decode outcome from claude.ProbeConfig.OnCommands and is
// handled here rather than at the call site so no caller can get the rule
// wrong: an unreadable array is NO information, so the previous entry stands.
// An empty (or absent) array IS information — a binary that reports no
// commands — so it replaces the entry with an empty list, and CommandsFor then
// answers "none" rather than serving a list the running binary disowns.
//
// Reports whether the stored answer changed, so a caller can skip a
// re-emission when a repeat probe said the same thing.
func (c *Cache) Store(key provider.ProbeCacheKey, commands []provider.SlashCommand, wireErr error) bool {
	if wireErr != nil {
		return false
	}

	stored := cloneCommands(commands)

	c.mu.Lock()
	defer c.mu.Unlock()

	encoded := key.String()
	previous, existed := c.entries[encoded]
	c.entries[encoded] = stored
	if !existed {
		c.order = append(c.order, encoded)
		c.evictOldestLocked()
		return len(stored) > 0
	}
	return !sameCommands(previous, stored)
}

// CommandsFor returns the command list one probe identity reported, or nil when
// no probe has reported for it yet. Never spawns: this type only ever reads
// what a probe already handed it, and nil means "nobody has asked the binary",
// which a caller must not render as "this session has no commands".
func (c *Cache) CommandsFor(key provider.ProbeCacheKey) []provider.SlashCommand {
	commands, _ := c.AnswerFor(key)
	return commands
}

// AnswerFor is CommandsFor plus the one fact the slice cannot carry: whether a
// probe has reported for this identity at all.
//
// The list alone cannot say. A binary that reports no commands stores an empty
// answer, and an empty answer clones back as nil — the same nil an identity no
// probe has ever run for produces. A caller putting the answer on a wire has to
// tell those apart, because "this binary has no commands" is a fact a menu may
// act on and "nobody has asked yet" is not.
func (c *Cache) AnswerFor(key provider.ProbeCacheKey) ([]provider.SlashCommand, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	commands, reported := c.entries[key.String()]
	return cloneCommands(commands), reported
}

func (c *Cache) evictOldestLocked() {
	for len(c.order) > maxCacheEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

// cloneCommands copies the slice so neither a caller nor a later probe can
// mutate what another holder is reading. SlashCommand is all value fields, so
// a shallow copy is a deep one.
func cloneCommands(in []provider.SlashCommand) []provider.SlashCommand {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.SlashCommand, len(in))
	copy(out, in)
	return out
}

func sameCommands(a, b []provider.SlashCommand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
