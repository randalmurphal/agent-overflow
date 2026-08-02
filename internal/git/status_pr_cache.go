package git

import "strings"

// lookupOpenPRCached returns cached PR info without making a network
// call. Returns ("", 0, "") on cache miss. A cached lookup failure returns
// the user-facing error together with the last PR the branch was known to
// have (empty when there was none) — see lookupOpenPR. Used by StatusFast
// to keep the initial subscribe path free of network calls.
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

// lookupOpenPR returns the open PR for (cwd, branch), consulting the TTL'd
// cache first and shelling out to the forge CLI on a miss. The third
// return is the user-facing lookup error, which is non-empty exactly when
// the forge lookup failed; the URL/number alongside it are then the last
// values successfully read for this branch, so a `gh` blip degrades the
// badge to "stale, with an error" rather than blanking it.
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
	// Read after ListOpenPRs: its forge dispatch runs DetectForge, which
	// leaves a fresh identity in the forge cache. Unknown here (cold or
	// expired entry) means "not observed" and never invalidates below.
	origin := c.cachedOrigin(cwd)

	c.prCacheMu.Lock()
	if lookupError != "" {
		// A transient forge failure (rate limit, expired token, CLI upgrade)
		// must not blank the PR badge: keep serving the last PR seen for this
		// (cwd, branch) next to the error. The error TTL still governs how
		// soon we retry. Only an origin that reads cleanly *and* differs from
		// the one that PR was found under can retarget the branch, so only
		// that drops the sticky value.
		if prev, ok := c.prCache[key]; ok && prev.url != "" && !origin.retargets(prev.origin) {
			url, number = prev.url, prev.number
			if prev.origin.known {
				// Keep the identity the PR was actually found under; adopt the
				// current one only to upgrade an unknown, which narrows the
				// guard for the next failure.
				origin = prev.origin
			}
		}
	}
	c.prCache[key] = prCacheEntry{
		url:         url,
		number:      number,
		lookupError: lookupError,
		expiresAt:   expiresAt,
		origin:      origin,
	}
	// Sweep entries whose usefulness as a sticky fallback has also lapsed, so
	// the map stays bounded by the recently-active (cwd, branch) pairs rather
	// than the lifetime total. Entries past expiresAt but inside the retention
	// window are kept: they are no longer served as fresh answers (both lookup
	// paths gate on expiresAt) but are still the last thing we knew.
	sweepBefore := now.Add(-prStickyRetention)
	for k, entry := range c.prCache {
		if entry.expiresAt.Before(sweepBefore) {
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
