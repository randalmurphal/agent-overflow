package git

import (
	"context"
	"strings"
	"time"
)

// lookupOpenPRCached returns cached PR info without making a network
// call. The last result reports whether the cache answered: false is a
// miss (cold or expired), and the PR fields with it say nothing about the
// branch. A cached lookup failure returns the user-facing error together
// with the last PR the branch was known to have (empty when there was
// none) — see lookupOpenPR. Used by StatusFast to keep the initial
// subscribe path free of network calls.
func (c *Core) lookupOpenPRCached(cwd, branch string) (url string, number int, lookupError string, cached bool) {
	if branch == "" {
		return "", 0, "", true
	}
	key := prCacheKey(cwd, branch)
	c.prCacheMu.RLock()
	defer c.prCacheMu.RUnlock()
	if entry, ok := c.prCache[key]; ok && entry.expiresAt.After(c.nowFn()) {
		return entry.url, entry.number, entry.lookupError, true
	}
	return "", 0, "", false
}

// lookupOpenPR returns the open PR for (cwd, branch), consulting the TTL'd
// cache first and asking the forge on a miss. A branch origin does not
// have (no refs/remotes/origin/<branch>) cannot head a PR, so it is
// answered "no PR" from that local check without a forge request. The
// third return is the user-facing lookup error, which is non-empty exactly
// when the forge lookup failed; the URL/number alongside it are then the
// last values successfully read for this branch, so a forge blip degrades
// the badge to "stale, with an error" rather than blanking it. A failure
// is cached for prLookupErrorDelay of its consecutive count. ctx bounds
// the forge call on a miss.
func (c *Core) lookupOpenPR(ctx context.Context, cwd, branch string) (string, int, string) {
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

	pushed, err := c.originHasBranch(ctx, cwd, branch)
	if err != nil && ctx.Err() != nil {
		return "", 0, err.Error()
	}
	if err == nil && !pushed {
		c.storePRLookup(key, now, prCacheEntry{expiresAt: now.Add(prLookupTTL), origin: c.cachedOrigin(cwd)})
		return "", 0, ""
	}
	// A failed local check says nothing about the branch; the forge
	// answers instead.

	// Slow path: ask the forge. Done outside the lock because the lookup is
	// a network call and unrelated lookups should not queue behind it. A
	// concurrent caller may double-fetch in the rare race window; the second
	// writer wins and both end up with the same value.
	url, number, lookupError := "", 0, ""
	pulls, err := c.ListOpenPRs(ctx, cwd, branch)
	if err != nil && ctx.Err() != nil {
		// The caller gave up. That says nothing about the forge, so the
		// cache keeps what it held.
		return "", 0, err.Error()
	}
	if err != nil {
		lookupError = err.Error()
	} else if len(pulls) > 0 {
		url = pulls[0].URL
		number = pulls[0].Number
	}
	// Read after ListOpenPRs: its forge dispatch runs DetectForge, which
	// leaves a fresh identity in the forge cache. Unknown here (cold or
	// expired entry) means "not observed" and never invalidates below.
	origin := c.cachedOrigin(cwd)

	c.prCacheMu.Lock()
	expiresAt := now.Add(prLookupTTL)
	failures := 0
	if lookupError != "" {
		failures = 1
		if prev, ok := c.prCache[key]; ok {
			failures = prev.failures + 1
		}
		expiresAt = now.Add(prLookupErrorDelay(failures))
		// A transient forge failure (rate limit, expired token, CLI upgrade)
		// must not blank the PR badge: keep serving the last PR seen for this
		// (cwd, branch) next to the error. The backoff still governs how
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
	c.storePRLookupLocked(key, now, prCacheEntry{
		url:         url,
		number:      number,
		lookupError: lookupError,
		expiresAt:   expiresAt,
		origin:      origin,
		failures:    failures,
	})
	c.prCacheMu.Unlock()
	return url, number, lookupError
}

// prLookupErrorDelay is how long the failures-th consecutive failed
// lookup is cached: prLookupErrorBase doubling up to prLookupErrorMax.
func prLookupErrorDelay(failures int) time.Duration {
	delay := prLookupErrorBase
	for i := 1; i < failures && delay < prLookupErrorMax; i++ {
		delay *= 2
	}
	return min(delay, prLookupErrorMax)
}

// originHasBranch reports whether cwd holds refs/remotes/origin/<branch>,
// the local evidence that the branch was pushed to origin. An error means
// git could not answer, not that the ref is absent.
func (c *Core) originHasBranch(ctx context.Context, cwd, branch string) (bool, error) {
	result, err := c.runSpec(commandSpec{ctx: ctx, binary: "git", cwd: cwd,
		args: []string{"rev-parse", "--verify", "--quiet", "refs/remotes/origin/" + branch}})
	if err != nil {
		return false, err
	}
	switch result.exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, commandFailure("git rev-parse --verify refs/remotes/origin/"+branch, result)
}

func (c *Core) storePRLookup(key string, now time.Time, entry prCacheEntry) {
	c.prCacheMu.Lock()
	defer c.prCacheMu.Unlock()
	c.storePRLookupLocked(key, now, entry)
}

// storePRLookupLocked caches one lookup answer and sweeps entries whose
// usefulness as a sticky fallback has also lapsed, so the map stays
// bounded by the recently-active (cwd, branch) pairs rather than the
// lifetime total. Entries past expiresAt but inside the retention window
// are kept: they are no longer served as fresh answers (both lookup paths
// gate on expiresAt) but are still the last thing we knew. Callers hold
// prCacheMu.
func (c *Core) storePRLookupLocked(key string, now time.Time, entry prCacheEntry) {
	c.prCache[key] = entry
	sweepBefore := now.Add(-prStickyRetention)
	for k, held := range c.prCache {
		if held.expiresAt.Before(sweepBefore) {
			delete(c.prCache, k)
		}
	}
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
