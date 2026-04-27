package git

import (
	"strings"
	"time"
)

// forgeDetectionTTL bounds how long an origin-URL classification stays
// cached. Origin URLs change rarely (the user reconfigures the remote
// or migrates a repo), and the cache prevents `git remote get-url`
// shell-outs in the lookupOpenPR / forgeFor hot path. We accept a
// 5-minute window where a recently-changed remote still classifies as
// the previous forge — a tradeoff documented next to the constant.
const forgeDetectionTTL = 5 * time.Minute

type forgeCacheEntry struct {
	forge     string
	expiresAt time.Time
}

// originURL runs `git remote get-url origin` and returns the URL plus
// a flag for whether the lookup succeeded. Returns ("", false) when
// the cwd has no "origin" remote configured.
func (c *Core) originURL(cwd string) (string, bool) {
	result, err := c.run(cwd, "remote", "get-url", "origin")
	if err != nil || result.exitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(result.stdout), true
}

// DetectForge returns the canonical forge id ("github" | "gitlab") for
// cwd's origin remote, or "" when the remote is missing or the host is
// unsupported. Results are memoized for forgeDetectionTTL on the Core
// instance; callers do not need to invalidate explicitly because
// Core.Status writes to the same cache on every refresh.
func (c *Core) DetectForge(cwd string) string {
	if cwd == "" {
		return ""
	}
	now := c.nowFn()

	c.forgeCacheMu.RLock()
	if entry, ok := c.forgeCache[cwd]; ok && entry.expiresAt.After(now) {
		c.forgeCacheMu.RUnlock()
		return entry.forge
	}
	c.forgeCacheMu.RUnlock()

	url, ok := c.originURL(cwd)
	forge := ""
	if ok {
		forge = classifyOriginURL(url)
	}
	c.storeForgeCache(cwd, forge, now)
	return forge
}

// storeForgeCache memoizes a forge id for cwd. Called by both
// DetectForge (on cache miss) and Status (each refresh) so the two
// paths share the same TTL window. Sweeps expired sibling entries on
// write so the map's footprint stays bounded by the number of active
// (non-expired) workspaces rather than the lifetime cumulative total.
func (c *Core) storeForgeCache(cwd, forge string, now time.Time) {
	c.forgeCacheMu.Lock()
	c.forgeCache[cwd] = forgeCacheEntry{
		forge:     forge,
		expiresAt: now.Add(forgeDetectionTTL),
	}
	for k, entry := range c.forgeCache {
		if entry.expiresAt.Before(now) {
			delete(c.forgeCache, k)
		}
	}
	c.forgeCacheMu.Unlock()
}

// InvalidateForgeCache drops the cached forge classification for cwd.
// Use after a known origin-URL change (e.g., the user reconfigured
// their remote and clicked Refresh) so the next DetectForge / Status
// call re-runs `git remote get-url origin` rather than waiting up to
// forgeDetectionTTL for the cached value to expire.
func (c *Core) InvalidateForgeCache(cwd string) {
	c.forgeCacheMu.Lock()
	defer c.forgeCacheMu.Unlock()
	delete(c.forgeCache, cwd)
}

// classifyOriginURL parses a git remote URL and returns the forge id
// ("github" | "gitlab" | "") for v1's literal hostname matching.
//
// Accepted shapes (per host):
//
//	HTTPS:    https://github.com/owner/repo[.git]
//	SSH alias: git@github.com:owner/repo[.git]
//	SSH URL:  ssh://git@github.com/owner/repo[.git]
//	git://    git://github.com/owner/repo[.git]
//
// Anything else (Bitbucket, Gitea, self-hosted) returns "" so the
// caller surfaces "no forge integration available" to the user.
func classifyOriginURL(remoteURL string) string {
	switch extractRemoteHost(remoteURL) {
	case "github.com":
		return "github"
	case "gitlab.com":
		return "gitlab"
	default:
		return ""
	}
}

// extractRemoteHost reduces a git remote URL to its canonical lowercase
// host. Returns "" for empty or malformed inputs.
//
// Recognised shapes (priority order):
//  1. SSH alias `user@host:path` — the colon after host is the path
//     separator, not a port. Detected before scheme parsing because
//     net/url would mis-interpret this as scheme=user.
//  2. URL form `scheme://[user@]host[:port]/path`.
//  3. Bare `host/path`.
//
// `@` stripping is scoped to the authority segment (between scheme and
// the first path slash) so a URL like `https://github.com/foo@bar/repo`
// is not misclassified by treating the path's `@` as a userinfo
// delimiter.
func extractRemoteHost(remoteURL string) string {
	s := strings.TrimSpace(remoteURL)
	if s == "" {
		return ""
	}

	// SSH alias `user@host:path`: detect before any scheme handling.
	if !strings.Contains(s, "://") {
		if at := strings.Index(s, "@"); at >= 0 {
			rest := s[at+1:]
			if colon := strings.Index(rest, ":"); colon >= 0 {
				return strings.ToLower(rest[:colon])
			}
		}
	}

	// Strip scheme.
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}

	// Isolate the authority by cutting at the first path slash; only
	// then look for `@` so an `@` inside the path can't be mistaken
	// for a userinfo delimiter.
	authority := s
	if slash := strings.Index(s, "/"); slash >= 0 {
		authority = s[:slash]
	}

	// Strip optional `user@` prefix from the authority.
	if at := strings.Index(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}

	// Strip optional `:port` suffix.
	if colon := strings.Index(authority, ":"); colon >= 0 {
		authority = authority[:colon]
	}

	return strings.ToLower(authority)
}
