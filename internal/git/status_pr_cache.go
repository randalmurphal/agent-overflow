package git

import "strings"

// lookupOpenPRCached returns cached PR info without making a network
// call. Returns ("", 0, "") on cache miss. Cached lookup failures return an
// empty URL/number plus the user-facing lookup error. Used by StatusFast to
// keep the initial subscribe path free of network calls.
func (c *Core) lookupOpenPRCached(cwd, branch string) (string, int, string) {
	if branch == "" {
		return "", 0, ""
	}
	key := prCacheKey(cwd, branch)
	c.prCacheMu.RLock()
	defer c.prCacheMu.RUnlock()
	if entry, ok := c.prCache[key]; ok && entry.expiresAt.After(c.nowFn()) {
		return entry.url, entry.number, entry.lookupError
	}
	return "", 0, ""
}

func (c *Core) lookupOpenPR(cwd, branch string) (string, int, string) {
	if branch == "" {
		return "", 0, ""
	}
	key := prCacheKey(cwd, branch)
	now := c.nowFn()

	c.prCacheMu.RLock()
	if entry, ok := c.prCache[key]; ok && entry.expiresAt.After(now) {
		c.prCacheMu.RUnlock()
		return entry.url, entry.number, entry.lookupError
	}
	c.prCacheMu.RUnlock()

	// Slow path: shell out. Done outside the lock because `gh pr list` is a
	// network call and unrelated lookups should not queue behind it. A
	// concurrent caller may double-fetch in the rare race window; the second
	// writer wins and both end up with the same value.
	url, number, lookupError := "", 0, ""
	pulls, err := c.ListOpenPRs(cwd, branch)
	expiresAt := now.Add(prLookupTTL)
	if err != nil {
		lookupError = err.Error()
		expiresAt = now.Add(prLookupErrorTTL)
	} else if len(pulls) > 0 {
		url = pulls[0].URL
		number = pulls[0].Number
	}

	c.prCacheMu.Lock()
	c.prCache[key] = prCacheEntry{
		url:         url,
		number:      number,
		lookupError: lookupError,
		expiresAt:   expiresAt,
	}
	// Sweep expired sibling entries on each write so the map stays bounded by
	// the number of recently-active (cwd, branch) pairs rather than the lifetime
	// total.
	for k, entry := range c.prCache {
		if entry.expiresAt.Before(now) {
			delete(c.prCache, k)
		}
	}
	c.prCacheMu.Unlock()
	return url, number, lookupError
}

// InvalidatePRCache drops every cached open-PR entry for cwd. Call after a
// successful CreatePR (or any action that materially changes PR state) so the
// next status refresh sees the new PR immediately rather than waiting up to
// prLookupTTL.
func (c *Core) InvalidatePRCache(cwd string) {
	prefix := cwd + "\x00"
	c.prCacheMu.Lock()
	defer c.prCacheMu.Unlock()
	for key := range c.prCache {
		if strings.HasPrefix(key, prefix) {
			delete(c.prCache, key)
		}
	}
}
