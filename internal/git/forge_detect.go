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
	origin    originIdentity
	expiresAt time.Time
}

// originIdentity is what a workspace's `origin` remote was observed to be.
// `known` distinguishes "we read the remote config" from "we could not"
// — a failed read is an absence of information, never evidence that the
// remote is gone, and callers that act on a change of identity must treat
// the two differently.
type originIdentity struct {
	url   string
	known bool
}

// retargets reports whether o is a successfully-read identity that differs
// from other, i.e. the remote demonstrably moved. Unknown on either side
// yields false: nothing is known to have changed.
func (o originIdentity) retargets(other originIdentity) bool {
	return o.known && other.known && o.url != other.url
}

// readOriginRemote runs `git remote get-url origin` and reports the URL.
// The returned identity is unknown when the cwd has no "origin" remote, is
// not a repository, or git failed for any other reason. Callers go through
// originRemote (repo_meta_cache.go), which fronts this read with the
// per-repository TTL cache.
func (c *Core) readOriginRemote(cwd string) originIdentity {
	result, err := c.run(cwd, "remote", "get-url", "origin")
	if err != nil || result.exitCode != 0 {
		return originIdentity{}
	}
	return originIdentity{url: strings.TrimSpace(result.stdout), known: true}
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

	return c.recordOrigin(cwd, c.originRemote(cwd), now)
}

// recordOrigin classifies origin, memoizes the classification together
// with the identity it was derived from, and returns the forge id. Called
// by both DetectForge (on cache miss) and Status (each refresh) so the two
// paths share the same TTL window — and so classification can never be
// cached apart from the origin it describes. Sweeps expired sibling
// entries on write so the map's footprint stays bounded by the number of
// active (non-expired) workspaces rather than the lifetime total.
func (c *Core) recordOrigin(cwd string, origin originIdentity, now time.Time) string {
	forge := ""
	if origin.known {
		forge = classifyOriginURL(origin.url, c.gitLabHostsSnapshot())
	}
	c.forgeCacheMu.Lock()
	c.forgeCache[cwd] = forgeCacheEntry{
		forge:     forge,
		origin:    origin,
		expiresAt: now.Add(forgeDetectionTTL),
	}
	for k, entry := range c.forgeCache {
		if entry.expiresAt.Before(now) {
			delete(c.forgeCache, k)
		}
	}
	c.forgeCacheMu.Unlock()
	return forge
}

// cachedOrigin returns the origin identity recorded alongside a live forge
// classification for cwd, without shelling out. A miss or an expired entry
// returns an unknown identity — callers must read that as "no information
// about the remote", never as "no remote".
func (c *Core) cachedOrigin(cwd string) originIdentity {
	c.forgeCacheMu.RLock()
	defer c.forgeCacheMu.RUnlock()
	if entry, ok := c.forgeCache[cwd]; ok && entry.expiresAt.After(c.nowFn()) {
		return entry.origin
	}
	return originIdentity{}
}

// InvalidateForgeCache drops the cached forge classification for cwd,
// together with the cached origin identity and default branch it was
// derived from. Use after a known origin-URL change (e.g., the user
// reconfigured their remote and clicked Refresh) so the next DetectForge
// / Status call re-runs `git remote get-url origin` rather than waiting
// up to forgeDetectionTTL / repoMetaTTL for the cached values to expire.
func (c *Core) InvalidateForgeCache(cwd string) {
	c.forgeCacheMu.Lock()
	delete(c.forgeCache, cwd)
	c.forgeCacheMu.Unlock()
	c.invalidateRepoMeta(cwd)
}

// InvalidateAllForgeCache drops every cached forge classification.
// Use when the configured GitLab self-hosted host list changes — every
// cwd's classification could have flipped, and waiting up to
// forgeDetectionTTL for stale entries to expire would leave Ship
// Changes showing "remote is not GitHub or GitLab" against repos that
// just became recognised.
func (c *Core) InvalidateAllForgeCache() {
	c.forgeCacheMu.Lock()
	defer c.forgeCacheMu.Unlock()
	clear(c.forgeCache)
}

// SetGitLabHosts replaces the self-hosted GitLab hostname snapshot
// classifyOriginURL consults. Callers (the app's settings side-effect
// hook) typically pair this with InvalidateAllForgeCache so the next
// Status / DetectForge call reclassifies under the new list rather
// than waiting for the per-cwd TTL window to expire. A defensive copy
// is taken so the caller is free to mutate its slice afterwards.
func (c *Core) SetGitLabHosts(hosts []string) {
	var snapshot []string
	if len(hosts) > 0 {
		snapshot = make([]string, len(hosts))
		copy(snapshot, hosts)
	}
	c.gitlabHostsMu.Lock()
	c.gitlabHosts = snapshot
	c.gitlabHostsMu.Unlock()
}

// gitLabHostsSnapshot returns a read-locked copy of the current
// allowlist for classifyOriginURL. Returns nil when no hosts are
// configured — nil iterates zero times in the classifier's loop.
func (c *Core) gitLabHostsSnapshot() []string {
	c.gitlabHostsMu.RLock()
	defer c.gitlabHostsMu.RUnlock()
	if len(c.gitlabHosts) == 0 {
		return nil
	}
	out := make([]string, len(c.gitlabHosts))
	copy(out, c.gitlabHosts)
	return out
}

// classifyOriginURL parses a git remote URL and returns the forge id
// ("github" | "gitlab" | "") for literal hostname matching.
//
// Accepted shapes (per host):
//
//	HTTPS:    https://github.com/owner/repo[.git]
//	SSH alias: git@github.com:owner/repo[.git]
//	SSH URL:  ssh://git@github.com/owner/repo[.git]
//	git://    git://github.com/owner/repo[.git]
//
// `gitlabHosts` is the user-configured allowlist of self-hosted GitLab
// hostnames; entries match against the canonicalised extractRemoteHost
// value, so all four URL shapes work for self-hosted GitLab too.
// Entries are expected to be pre-normalised (lowercase, bare hostname);
// settings.validateGitLabHosts enforces that on write.
//
// Anything else (Bitbucket, Gitea, unrecognised self-hosted) returns ""
// so the caller surfaces "no forge integration available" to the user.
func classifyOriginURL(remoteURL string, gitlabHosts []string) string {
	host := extractRemoteHost(remoteURL)
	switch host {
	case "github.com":
		return "github"
	case "gitlab.com":
		return "gitlab"
	}
	if host == "" {
		return ""
	}
	for _, h := range gitlabHosts {
		if h == host {
			return "gitlab"
		}
	}
	return ""
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
