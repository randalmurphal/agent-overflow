package git

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// lookupOpenPRCached returns the cached open-PR answer for (cwd, branch)
// without a network call. The last result reports whether that answer is
// current: false means a full lookup still has to answer, and the PR
// fields with it are then the last PR the branch was known to have (empty
// when there was none), which readers show until that answer lands. A
// stale PR is withheld once the origin reads as a different remote than
// the one it was found under. A current cached failure returns the
// user-facing error together with the last known PR (see lookupOpenPR).
// Used by StatusFast to keep the initial subscribe path free of network
// calls.
func (c *Core) lookupOpenPRCached(cwd, branch string) (url string, number int, lookupError string, current bool) {
	if branch == "" {
		return "", 0, "", true
	}
	origin := c.cachedOrigin(cwd)
	c.prCacheMu.RLock()
	defer c.prCacheMu.RUnlock()
	entry, ok := c.prCache[prCacheKey(cwd, branch)]
	switch {
	case !ok:
		return "", 0, "", false
	case entry.expiresAt.After(c.nowFn()):
		return entry.url, entry.number, entry.lookupError, true
	case origin.retargets(entry.origin):
		return "", 0, "", false
	}
	return entry.url, entry.number, "", false
}

// openPRRecheckAt is when the cached failed lookup for (cwd, branch) is
// asked again, or zero when its cached answer is not a failure. Status
// reports it so the status watcher can re-check without a local change.
func (c *Core) openPRRecheckAt(cwd, branch string) time.Time {
	c.prCacheMu.RLock()
	defer c.prCacheMu.RUnlock()
	if entry, ok := c.prCache[prCacheKey(cwd, branch)]; ok && entry.lookupError != "" {
		return entry.expiresAt
	}
	return time.Time{}
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
	epoch := c.prCacheEpochs[cwd]
	c.prCacheMu.RUnlock()

	pushed, err := c.originHasBranch(ctx, cwd, branch)
	if err != nil && ctx.Err() != nil {
		return "", 0, err.Error()
	}
	if err == nil && !pushed {
		c.storePRLookup(cwd, key, epoch, now, prCacheEntry{expiresAt: now.Add(prLookupTTL), origin: c.cachedOrigin(cwd)})
		c.observeOpenPR(cwd, branch, nil)
		return "", 0, ""
	}
	// A failed local check says nothing about the branch; the forge
	// answers instead.

	// Slow path: ask the forge. Done outside the lock because the lookup is
	// a network call and unrelated lookups should not queue behind it. A
	// concurrent caller may double-fetch in the rare race window; the second
	// writer wins and both end up with the same value.
	url, number, lookupError := "", 0, ""
	var open *GitPR
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
		open = &pulls[0]
	}
	// Read after ListOpenPRs: its forge dispatch runs DetectForge, which
	// leaves a fresh identity in the forge cache. Unknown here (cold or
	// expired entry) means "not observed" and never invalidates below.
	origin := c.cachedOrigin(cwd)
	// The answer's lifetime, and a failure's wait before the forge is asked
	// again, run from when the forge answered: a request that hung for most
	// of the read timeout must not use up the wait that follows it.
	answered := c.nowFn()

	c.prCacheMu.Lock()
	expiresAt := answered.Add(prLookupTTL)
	failures := 0
	if lookupError != "" {
		failures = 1
		if prev, ok := c.prCache[key]; ok {
			failures = prev.failures + 1
		}
		expiresAt = answered.Add(c.prLookupErrorDelay(failures))
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
	c.storePRLookupLocked(cwd, key, epoch, answered, prCacheEntry{
		url:         url,
		number:      number,
		lookupError: lookupError,
		expiresAt:   expiresAt,
		origin:      origin,
		failures:    failures,
	})
	c.prCacheMu.Unlock()
	if lookupError == "" {
		c.observeOpenPR(cwd, branch, open)
	}
	return url, number, lookupError
}

// prLookupErrorDelay is how long the failures-th consecutive failed
// lookup is cached: the Core's base (prLookupErrorBase unless
// WithPRLookupErrorBase replaced it) doubling up to prLookupErrorMax.
func (c *Core) prLookupErrorDelay(failures int) time.Duration {
	delay := c.prLookupErrorBase
	if delay <= 0 {
		delay = prLookupErrorBase
	}
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

func (c *Core) storePRLookup(cwd, key string, epoch uint64, now time.Time, entry prCacheEntry) {
	c.prCacheMu.Lock()
	defer c.prCacheMu.Unlock()
	c.storePRLookupLocked(cwd, key, epoch, now, entry)
}

// storePRLookupLocked caches one lookup answer. An entry outlives its
// expiry as the last thing known about its branch: it is never served as
// a current answer then (both lookup paths gate on expiresAt), but a
// failed lookup carries its PR forward and StatusFast shows it while the
// next lookup runs. So no entry is dropped for its age. The map is bounded
// by prCacheMaxEntries instead, evicting the entry stored longest ago, and
// a removed worktree's entries go with it (ForgetPRCache). Callers hold
// prCacheMu. epoch is cwd's prCacheEpochs count when the lookup started;
// an invalidation of cwd since then discards the answer, which may
// predate it.
func (c *Core) storePRLookupLocked(cwd, key string, epoch uint64, now time.Time, entry prCacheEntry) {
	if epoch != c.prCacheEpochs[cwd] {
		return
	}
	if _, ok := c.prCache[key]; !ok && len(c.prCache) >= prCacheMaxEntries {
		oldestKey, oldest := "", time.Time{}
		for k, held := range c.prCache {
			if oldestKey == "" || held.storedAt.Before(oldest) {
				oldestKey, oldest = k, held.storedAt
			}
		}
		delete(c.prCache, oldestKey)
	}
	entry.storedAt = now
	c.prCache[key] = entry
}

// InvalidatePRCache makes every cached open-PR answer for cwd stale, so the
// next lookup asks the forge again, and discards any lookup of cwd already
// in flight. Call after a successful CreatePR, on an explicit attach, or
// after any action that may have changed PR state, so the next status
// refresh does not wait up to prLookupTTL. Each answer stays the last
// known PR of its branch: a failed re-check still shows it, and only an
// answer from the forge replaces it.
func (c *Core) InvalidatePRCache(cwd string) {
	prefix := cwd + "\x00"
	c.prCacheMu.Lock()
	defer c.prCacheMu.Unlock()
	c.prCacheEpochs[cwd]++
	for key, entry := range c.prCache {
		if strings.HasPrefix(key, prefix) {
			entry.expiresAt = time.Time{}
			c.prCache[key] = entry
		}
	}
}

// ForgetPRCache drops every cached open-PR answer for cwd, last known PRs
// included, and discards any lookup of cwd in flight. For a workspace that
// no longer exists: a checkout created at the same path later is another
// workspace, which the old answers say nothing about.
func (c *Core) ForgetPRCache(cwd string) {
	prefix := cwd + "\x00"
	c.prCacheMu.Lock()
	defer c.prCacheMu.Unlock()
	c.prCacheEpochs[cwd]++
	for key := range c.prCache {
		if strings.HasPrefix(key, prefix) {
			delete(c.prCache, key)
		}
	}
}

// prCreateCommand matches a shell command that runs the GitHub or GitLab
// CLI's create subcommand (`gh pr create`, `glab mr create`, or their `new`
// aliases), wherever it sits in a pipeline or behind a shell wrapper.
var prCreateCommand = regexp.MustCompile(`(?:^|[\s;&|(/` + "`" + `"'])(?:gh\s+(?:\S+\s+)*?pr|glab\s+(?:\S+\s+)*?mr)\s+(?:create|new)(?:\s|$|[;&|)"'])`)

// CreatesPullRequest reports whether command runs a forge CLI's create
// subcommand. A match only prompts a fresh open-PR lookup, which answers
// from the forge, so a command that merely mentions the words costs one
// lookup and shows nothing.
func CreatesPullRequest(command string) bool {
	if !strings.Contains(command, "pr ") && !strings.Contains(command, "mr ") {
		return false
	}
	return prCreateCommand.MatchString(command)
}
