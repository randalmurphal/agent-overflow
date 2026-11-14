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
	// the remote-tracking ref ("origin/main") or the caller's base in its
	// wire form ("main", "refs/tags/v1"). Empty when the caller passed no base at all, i.e. the cut
	// used the repository's current HEAD.
	Ref string
	// FromRemote is true when Ref is origin's tracking ref rather than the
	// local branch.
	FromRemote bool
	// FetchErr is the bounded origin fetch's failure, if any. It is NOT an
	// error the user needs: the cut proceeded from the local base exactly
	// as it would have without any fetch at all. A remote that needs
	// interactive credentials lands here too (this fetch keeps the package
	// default non-interactive env, see gitenv.NonInteractive), which is why it
	// must never become a user-facing failure.
	FetchErr error
}

// CreateWorktreeFromFreshRef cuts newBranch's worktree the way a person
// would if they remembered to fetch first: for a base branch it refreshes
// origin (bounded by worktreeSeedFetchTimeout, throttled by the shared
// fetch window) and then seeds the new branch from `origin/<base>` when
// that ref exists, falling back to the local base on any doubt. A tag or
// commit base is local: nothing is fetched, origin is not preferred, and
// the new branch gets no upstream.
//
// The problem it solves: a local base branch is only as fresh as the last
// time the user fetched, so a thread's worktree silently starts from a
// stale main and every diff, PR, and review that follows inherits that
// staleness.
//
// The rules, in order:
//
//   - The zero base means "cut from HEAD" (CreateWorktreeFromRef's own
//     contract). Nothing is fetched and nothing is resolved.
//   - The fetch is origin-only and skipped entirely when the repository has
//     no origin, or when the shared fetch window (FetchStaleWindow, keyed on
//     the git common dir and shared with MaybeFetchRemotes /
//     FetchRemotesBackground / PruneRemotes) says someone already fetched
//     recently. A recent fetch is as good as ours.
//   - The remote-tracking ref is used when the refs can be trusted: the
//     fetch succeeded, or it was skipped because the window was fresh. A
//     failed or timed-out fetch falls back to the local base, because a
//     stale `origin/<base>` is not a better answer than the branch the user
//     can see, and this call must never surprise anyone with a start point
//     it could not verify.
//   - Unless there is no local branch of that name, in which case the
//     unverified `origin/<base>` is the only start point there is.
//     BaseBranchKnown admits a base that exists on origin alone, so the
//     caller was told this base works; cutting from the bare name would
//     then fail inside git with nothing anyone could act on.
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
func (c *Core) CreateWorktreeFromFreshRef(
	ctx context.Context, cwd, path string, base Ref, newBranch string,
) (WorktreeSeed, error) {
	seed := WorktreeSeed{Ref: base.Wire()}
	if err := base.validate(); err != nil {
		return seed, err
	}
	if !base.IsBranch() {
		return seed, c.CreateWorktreeFromRef(cwd, path, base, newBranch)
	}
	// Read before the fetch, which cannot create a local branch: it is
	// what decides whether an unverified tracking ref is a downgrade or the
	// only start point there is.
	local, err := c.branchExistsChecked(cwd, base.Name)
	if err != nil {
		return seed, err
	}
	trusted, fetchErr := c.fetchOriginForSeed(ctx, cwd)
	seed.FetchErr = fetchErr
	startPoint := base.revision()
	if remoteRef, ok := c.originTrackingRef(cwd, base.Name); ok && (trusted || !local) {
		seed.Ref = remoteRef
		seed.FromRemote = true
		startPoint = "refs/remotes/" + remoteRef
	}
	if err := c.createWorktreeAt(cwd, path, startPoint, newBranch, seed.FromRemote); err != nil {
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
// Concurrent fetches of one repository share the flight
// FetchRemotesBackground uses, so a worktree cut during a background tick
// joins that fetch instead of racing a second one into the same ref locks.
// The 5 second bound ends this caller's wait, and the fetch only when no
// other caller waits for it (fetch_flight.go).
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
	_, err = c.fetchFlight.do(ctx, key, func(ctx context.Context) (bool, error) {
		// Re-check inside the flight: a caller that queued behind a
		// just-finished fetch must not spawn a second one.
		if c.fetchIsFresh(key) {
			return false, nil
		}
		if err := c.fetchFn(ctx, cwd); err != nil {
			return false, err
		}
		c.stampFetchCache(key)
		return true, nil
	})
	if err != nil && ctx.Err() != nil {
		// Our own bound, or the caller shutting down. Either way the refs
		// are unverified and the cut proceeds from the local base.
		return false, fmt.Errorf("git fetch origin for worktree base: %w", ctx.Err())
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// BaseBranchKnown reports whether branch can seed a worktree cut: as a
// local branch, or, unless localOnly, as origin's tracking ref from the
// last fetch. It is a pre-flight check so a caller can refuse a base by
// name before anything durable exists; the cut itself still validates.
func (c *Core) BaseBranchKnown(cwd, branch string, localOnly bool) (bool, error) {
	branch = strings.TrimSpace(branch)
	if err := validateBranchName(branch); err != nil {
		return false, err
	}
	local, err := c.branchExistsChecked(cwd, branch)
	if err != nil {
		return false, err
	}
	if local || localOnly {
		return local, nil
	}
	_, remote := c.originTrackingRef(cwd, branch)
	return remote, nil
}

// BranchToCheckOut reports whether branch already exists for a worktree to
// check out: as a local branch, or as origin's once origin is refreshed. The
// refresh is the one a fresh-base cut makes, shared and bounded the same
// way, so a branch pushed since the last fetch is found and a cut that
// follows does not fetch again. A local branch answers without a fetch.
//
// fetchErr is diagnostics only: a failed or timed-out fetch leaves the
// tracking refs as they stand and the answer is read from them. A name git
// would refuse as a branch names no existing branch.
func (c *Core) BranchToCheckOut(ctx context.Context, cwd, branch string) (exists bool, fetchErr error, err error) {
	branch = strings.TrimSpace(branch)
	if validateBranchName(branch) != nil {
		return false, nil, nil
	}
	local, err := c.branchExistsChecked(cwd, branch)
	if err != nil || local {
		return local, nil, err
	}
	_, fetchErr = c.fetchOriginForSeed(ctx, cwd)
	_, remote := c.originTrackingRef(cwd, branch)
	return remote, fetchErr, nil
}

// WorktreeTarget is what a name typed for a fresh worktree resolved to:
// Ref, a branch or a tag, and whether it Exists. A branch that does not
// exist is one to create; a tag that does not exist names nothing.
type WorktreeTarget struct {
	Ref    Ref
	Exists bool
}

// ResolveWorktreeTarget resolves name for a fresh worktree, once, after the
// bounded origin fetch a cut makes (fetchOriginForSeed), so what the fetch
// brings counts. "refs/heads/<n>" is that branch and "refs/tags/<n>" that
// tag. A bare name is a local branch, else origin's branch, else a tag of
// that name, else a branch to create. A local branch or tag answers with no
// fetch. fetchErr is diagnostics only: the refs are read as they stand.
func (c *Core) ResolveWorktreeTarget(ctx context.Context, cwd, name string) (target WorktreeTarget, fetchErr error, err error) {
	ref, explicit, err := parseRef(name)
	if err != nil {
		return WorktreeTarget{}, nil, err
	}
	if ref.IsZero() {
		return WorktreeTarget{}, nil, fmt.Errorf("a branch or tag name is required")
	}
	if ref.IsBranch() {
		exists, fetchErr, err := c.BranchToCheckOut(ctx, cwd, ref.Name)
		if err != nil || exists || explicit {
			return WorktreeTarget{Ref: ref, Exists: exists}, fetchErr, err
		}
		tag, err := c.TagExists(cwd, ref.Name)
		if err != nil {
			return WorktreeTarget{}, fetchErr, err
		}
		if tag {
			return WorktreeTarget{Ref: TagRef(ref.Name), Exists: true}, fetchErr, nil
		}
		return WorktreeTarget{Ref: ref}, fetchErr, nil
	}
	exists, err := c.TagExists(cwd, ref.Name)
	if err != nil || exists {
		return WorktreeTarget{Ref: ref, Exists: exists}, nil, err
	}
	_, fetchErr = c.fetchOriginForSeed(ctx, cwd)
	exists, err = c.TagExists(cwd, ref.Name)
	return WorktreeTarget{Ref: ref, Exists: exists}, fetchErr, err
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
