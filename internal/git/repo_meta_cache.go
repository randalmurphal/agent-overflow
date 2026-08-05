package git

import (
	"sync"
	"time"
)

// repoMetaTTL bounds how long the two per-repository probe results below
// — the default-branch name (`git symbolic-ref refs/remotes/origin/HEAD`)
// and the origin-remote identity (`git remote get-url origin`) — are
// served from memory. Both describe repository configuration that changes
// rarely (a remote reconfiguration, a `git remote set-head`), yet both
// are probed by every gitwatch status refresh; without the cache each
// refresh costs two extra subprocesses, which is real money on a repo
// reached over WSL's 9P bridge. We accept the same 5-minute window
// forgeDetectionTTL documents: a remote retargeted out of band can be
// observed as its previous identity for up to this long. Callers that
// know the remote just changed use InvalidateForgeCache, which drops
// these entries too.
const repoMetaTTL = 5 * time.Minute

// repoMetaEntry is one TTL-stamped cache value. The maps holding these
// are keyed by the canonical git common dir (see Core.repoMetaKey) so
// every worktree of one repository shares a single entry.
type repoMetaEntry[T any] struct {
	value     T
	expiresAt time.Time
}

func repoMetaGet[T any](mu *sync.RWMutex, m map[string]repoMetaEntry[T], key string, now time.Time) (T, bool) {
	mu.RLock()
	defer mu.RUnlock()
	entry, ok := m[key]
	if !ok || !entry.expiresAt.After(now) {
		var zero T
		return zero, false
	}
	return entry.value, true
}

// repoMetaPut stores value and sweeps expired sibling entries so the map
// stays bounded by the number of active repositories, mirroring
// recordOrigin's sweep-on-write.
func repoMetaPut[T any](mu *sync.RWMutex, m map[string]repoMetaEntry[T], key string, value T, now time.Time) {
	mu.Lock()
	defer mu.Unlock()
	m[key] = repoMetaEntry[T]{value: value, expiresAt: now.Add(repoMetaTTL)}
	for k, entry := range m {
		if entry.expiresAt.Before(now) {
			delete(m, k)
		}
	}
}

// repoMetaKey resolves the cache identity for cwd: the canonical git
// common dir, so N worktrees of one repository collapse to one probe per
// TTL window instead of N. Returns "" when cwd is not in a repository —
// callers skip the cache and let the raw read report its own answer, so
// a non-repo path can never share a bucket with anything.
func (c *Core) repoMetaKey(cwd string) string {
	key, err := c.CommonDir(cwd)
	if err != nil {
		return ""
	}
	return key
}

// defaultBranchName reports the branch refs/remotes/origin/HEAD points at,
// or "" when the symref does not exist. Positive answers are cached per
// repository for repoMetaTTL; "" is deliberately not cached — it usually
// means the symref has not been materialized yet (a clone creates it, and
// `git remote set-head` or a fetch on git ≥ 2.47 can create it later), and
// caching it would hide that transition for up to the TTL. The common
// cloned-repo case therefore costs one subprocess per window instead of
// one per status refresh, while the symref-less case keeps today's
// probe-every-time behavior.
func (c *Core) defaultBranchName(cwd string) (string, error) {
	key := c.repoMetaKey(cwd)
	now := c.nowFn()
	if key != "" {
		if branch, ok := repoMetaGet(&c.repoMetaMu, c.defaultBranchCache, key, now); ok {
			return branch, nil
		}
	}
	branch, err := c.readDefaultBranchName(cwd)
	if err == nil && branch != "" && key != "" {
		repoMetaPut(&c.repoMetaMu, c.defaultBranchCache, key, branch, now)
	}
	return branch, err
}

// originRemote reports cwd's origin-remote identity, serving repeats from
// the per-repository cache. Only successfully-read identities are cached:
// unknown covers both "no origin remote" and "git failed", and neither is
// evidence worth pinning — a repo that gains an origin must be seen on
// the next probe, not a TTL later. The staleness this does accept (a
// retargeted remote reads as its old URL for up to repoMetaTTL) is the
// same window forgeDetectionTTL already accepts for the classification
// derived from it.
func (c *Core) originRemote(cwd string) originIdentity {
	key := c.repoMetaKey(cwd)
	now := c.nowFn()
	if key != "" {
		if origin, ok := repoMetaGet(&c.repoMetaMu, c.originCache, key, now); ok {
			return origin
		}
	}
	origin := c.readOriginRemote(cwd)
	if origin.known && key != "" {
		repoMetaPut(&c.repoMetaMu, c.originCache, key, origin, now)
	}
	return origin
}

// invalidateRepoMeta drops the cached default branch and origin identity
// for cwd's repository. Called from InvalidateForgeCache so "the user
// reconfigured their remote" invalidates the raw reads together with the
// classification derived from them.
func (c *Core) invalidateRepoMeta(cwd string) {
	key := c.repoMetaKey(cwd)
	if key == "" {
		return
	}
	c.repoMetaMu.Lock()
	delete(c.defaultBranchCache, key)
	delete(c.originCache, key)
	c.repoMetaMu.Unlock()
}
