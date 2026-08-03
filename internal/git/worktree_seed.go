package git

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// worktreeSeedFetchTimeout bounds the origin fetch that runs before a new
// worktree picks its start point. It is deliberately far below the shared
// subprocess timeout (defaultTimeout, 45s): the user pressed "new
// worktree" and is watching, so an unreachable remote must cost a blink
// and then fall back to the local branch — never turn worktree creation
// into a 45s stall. A var so tests can drive the timeout path without
// sleeping; never reassigned in production code.
var worktreeSeedFetchTimeout = 5 * time.Second

// WorktreeSeed reports how CreateWorktreeFromFreshBase chose the start
// point of the branch it cut. Callers use it for diagnostics only — every
// combination is a successful worktree.
type WorktreeSeed struct {
	// Ref is the revision the new branch was actually cut from: either
	// the remote-tracking ref ("origin/main") or the caller's base branch
	// ("main"). Empty when the caller passed no base at all, i.e. the cut
	// used the repository's current HEAD.
	Ref string
	// FromRemote is true when Ref is origin's tracking ref rather than the
	// local branch.
	FromRemote bool
	// FetchErr is the bounded origin fetch's failure, if any. It is NOT an
	// error the user needs: the cut proceeded from the local base exactly
	// as it would have without any fetch at all. A remote that needs
	// interactive credentials lands here too (this fetch keeps the package
	// default non-interactive env, see nonInteractiveEnv), which is why it
	// must never become a user-facing failure.
	FetchErr error
}

// CreateWorktreeFromFreshBase cuts newBranch's worktree the way a person
// would if they remembered to fetch first: it refreshes origin (bounded by
// worktreeSeedFetchTimeout, throttled by the shared fetch window) and then
// seeds the new branch from `origin/<baseBranch>` when that ref exists,
// falling back to the local baseBranch on any doubt.
//
// The problem it solves: a local base branch is only as fresh as the last
// time the user fetched, so a thread's worktree silently starts from a
// stale main and every diff, PR, and review that follows inherits that
// staleness.
//
// The rules, in order:
//
//   - An empty baseBranch means "cut from HEAD" (CreateWorktreeFromBranch's
//     own contract). Nothing is fetched and nothing is resolved.
//   - The fetch is origin-only and skipped entirely when the repository has
//     no origin, or when the shared fetch window (FetchStaleWindow, keyed on
//     the git common dir and shared with MaybeFetchRemotes /
//     FetchRemotesBackground / PruneRemotes) says someone already fetched
//     recently. A recent fetch is as good as ours.
//   - The remote-tracking ref is used ONLY when the refs can be trusted:
//     the fetch succeeded, or it was skipped because the window was fresh.
//     A failed or timed-out fetch falls back to the local base — a stale
//     `origin/<base>` is not a better answer than the branch the user can
//     see, and this call must never surprise anyone with a start point it
//     could not verify.
//   - `origin/<base>` only. This function fetches origin and nothing else,
//     so seeding from a remote it never refreshed (a fork's `upstream/*`)
//     would promise a freshness it did not deliver.
//
// It moves no ref the user can see: the remote-tracking ref is a start
// point, never a target, so a stale local `main` stays exactly where it
// was. `--no-track` is passed when cutting from the remote ref so the new
// branch does not silently acquire `origin/<base>` as its upstream — that
// would make its first `git push` argue about a name mismatch and would
// change what ahead/behind counts mean, neither of which anyone asked for
// by creating a worktree.
func (c *Core) CreateWorktreeFromFreshBase(
	ctx context.Context, cwd, path, baseBranch, newBranch string,
) (WorktreeSeed, error) {
	baseBranch = strings.TrimSpace(baseBranch)
	seed := WorktreeSeed{Ref: baseBranch}
	if baseBranch != "" {
		if err := validateBranchName(baseBranch); err != nil {
			return seed, err
		}
		trusted, err := c.fetchOriginForSeed(ctx, cwd)
		seed.FetchErr = err
		if trusted {
			if remoteRef, ok := c.originTrackingRef(cwd, baseBranch); ok {
				seed.Ref = remoteRef
				seed.FromRemote = true
			}
		}
	}
	if err := c.createWorktreeAt(cwd, path, seed.Ref, newBranch, seed.FromRemote); err != nil {
		return seed, err
	}
	return seed, nil
}

// fetchOriginForSeed refreshes origin's remote-tracking refs for cwd's
// repository, bounded by worktreeSeedFetchTimeout. The bool reports
// whether the refs may be trusted as current — true when this call
// fetched, and true when it skipped because the shared window says a fetch
// already happened inside FetchStaleWindow.
//
// (false, nil) is the "nothing to do" answer: no origin remote, so there
// are no remote-tracking refs worth waiting for. Only a genuine failure
// returns an error, and only as diagnostics — see WorktreeSeed.FetchErr.
//
// Concurrent fetches of one repository share the group FetchRemotesBackground
// uses, so a worktree cut during a background tick joins that fetch instead
// of racing a second one into the same ref locks. It waits on the flight
// through DoChan rather than blocking in Do: joining must not inherit the
// background cadence's app-lifetime deadline, or a "5 second" bound would
// silently become a 45 second one.
func (c *Core) fetchOriginForSeed(ctx context.Context, cwd string) (bool, error) {
	key, err := c.CommonDir(cwd)
	if err != nil {
		return false, err
	}
	if c.fetchIsFresh(key) {
		return true, nil
	}
	if !c.originRemoteExists(cwd) {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, worktreeSeedFetchTimeout)
	defer cancel()
	flight := c.fetchFlight.DoChan(key, func() (any, error) {
		// Re-check inside the flight: a caller that queued behind a
		// just-finished fetch must not spawn a second one.
		if c.fetchIsFresh(key) {
			return nil, nil
		}
		if err := c.fetchFn(ctx, cwd); err != nil {
			return nil, err
		}
		c.stampFetchCache(key)
		return nil, nil
	})
	select {
	case result := <-flight:
		if result.Err != nil {
			return false, result.Err
		}
		return true, nil
	case <-ctx.Done():
		// Our own bound, or the caller shutting down. Either way the refs
		// are unverified and the cut proceeds from the local base.
		return false, fmt.Errorf("git fetch origin for worktree base: %w", ctx.Err())
	}
}

// originTrackingRef reports origin's tracking ref for branch
// ("origin/<branch>") when it exists as a commit. The lookup is
// fully-qualified (`refs/remotes/...`) so a local branch or tag of the same
// name can never answer for it.
func (c *Core) originTrackingRef(cwd, branch string) (string, bool) {
	ref := "origin/" + branch
	result, err := c.run(cwd, "rev-parse", "--verify", "--quiet", "refs/remotes/"+ref+"^{commit}")
	if err != nil || result.exitCode != 0 {
		return "", false
	}
	return ref, strings.TrimSpace(result.stdout) != ""
}
