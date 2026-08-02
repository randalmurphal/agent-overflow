package git

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// CommonDir returns the canonical path of cwd's shared git directory —
// `git rev-parse --git-common-dir`. Every worktree of a repository
// resolves to the SAME value (a linked worktree's private gitdir under
// `.git/worktrees/<name>` is not it), which makes it the only correct
// identity for "this underlying repository" when the app must do
// something once per repo rather than once per checkout.
//
// Errors when cwd is not inside a repository, so callers keying state on
// the result never silently share a "" bucket across unrelated paths.
//
// Successful resolutions are memoized per cwd for the same reason
// resolveGitDir memoizes: a repository's git directory never moves under
// a live path, and the background-fetch cadence would otherwise spend one
// subprocess per project per tick just to decide it has nothing to do.
// Failures are never cached, so a later `git init` is seen; the entry is
// dropped when a worktree is removed at that path (RemoveWorktreeForce).
func (c *Core) CommonDir(cwd string) (string, error) {
	c.commonDirCacheMu.RLock()
	cached, ok := c.commonDirCache[cwd]
	c.commonDirCacheMu.RUnlock()
	if ok {
		return cached, nil
	}

	commonDir, ok, err := c.revParsePath(cwd, "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("git common dir: %s is not a git repository", cwd)
	}
	commonDir = CanonicalPath(commonDir)

	c.commonDirCacheMu.Lock()
	c.commonDirCache[cwd] = commonDir
	c.commonDirCacheMu.Unlock()
	return commonDir, nil
}

// FetchRemotesBackground runs `git fetch --quiet origin` for cwd's
// repository on behalf of a cadence nobody asked for, so that ahead/behind
// counts stop freezing at the user's last manual fetch. Returns
// (true, nil) when a fetch actually ran, (false, nil) when it was
// deliberately skipped.
//
// Skips, in order:
//   - the repository was fetched (by this path, by the branch picker's
//     MaybeFetchRemotes, or by PruneRemotes) less than FetchStaleWindow
//     ago — the throttle is keyed on the git common dir, so N worktrees
//     of one repository collapse to one fetch per window instead of N;
//   - the repository has no remote named "origin". A repo with no remote
//     at all has nothing to be behind, and a repo whose only remote is
//     named something else is out of scope for an automatic fetch: the
//     throttle is per-repository while "which remote is my upstream" is
//     per-branch, so chasing them here would make what gets fetched
//     depend on which worktree happened to win the tick.
//
// Concurrent calls for the same repository share one subprocess
// (single-flight keyed on the common dir): the branch picker's warm-up
// and this cadence otherwise race into two `git fetch` processes
// contending for the same ref locks.
//
// Deliberately NOT `--prune` and NOT `--tags`: both silently move or
// delete local refs the user can see, which is a surprise when it comes
// from a background timer instead of a button. Pruning stays on the
// explicit PruneRemotes action.
//
// This is a background command by construction, so it keeps the package
// default non-interactive env (see nonInteractiveEnv) — against a
// credential-requiring remote it fails fast with a readable error rather
// than raising a credential dialog behind the app window. The caller
// decides what a failure is worth saying out loud; for the app cadence
// that is one memoized log line per repo (app_git_background_fetch.go).
//
// ctx bounds the subprocess: cancelling it kills the fetch, so the owner
// of the cadence can shut down without waiting out a fetch hanging on a
// dead network. A joined single-flight caller rides the first caller's
// ctx — they are the same cadence, with the same lifetime.
func (c *Core) FetchRemotesBackground(ctx context.Context, cwd string) (bool, error) {
	key, err := c.CommonDir(cwd)
	if err != nil {
		return false, err
	}
	if c.fetchIsFresh(key) {
		return false, nil
	}

	fetched, err, _ := c.fetchFlight.Do(key, func() (any, error) {
		// Re-check inside the flight: a caller that queued behind a
		// just-finished fetch must not spawn a second one.
		if c.fetchIsFresh(key) {
			return false, nil
		}
		if !slices.Contains(c.listRemoteNames(cwd), "origin") {
			return false, nil
		}
		if err := c.fetchFn(ctx, cwd); err != nil {
			return false, err
		}
		c.stampFetchCache(key)
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return fetched.(bool), nil
}

// fetchOriginQuiet is the production body behind Core.fetchFn.
func (c *Core) fetchOriginQuiet(ctx context.Context, cwd string) error {
	_, stderr, err := c.executeSpec(commandSpec{
		binary: "git",
		cwd:    cwd,
		ctx:    ctx,
		args:   []string{"fetch", "--quiet", "origin"},
	})
	if err != nil {
		if message := strings.TrimSpace(stderr); message != "" {
			return fmt.Errorf("git fetch origin: %s", message)
		}
		return fmt.Errorf("git fetch origin: %w", err)
	}
	return nil
}

// fetchIsFresh reports whether key's last successful fetch is inside
// FetchStaleWindow.
func (c *Core) fetchIsFresh(key string) bool {
	c.fetchCacheMu.RLock()
	defer c.fetchCacheMu.RUnlock()
	last, ok := c.fetchCache[key]
	return ok && c.nowFn().Sub(last) < FetchStaleWindow
}
