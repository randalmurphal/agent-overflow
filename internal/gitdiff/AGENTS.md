# gitdiff/

Review-pane diff sources computed by shelling out to `git`. Every
function takes a workspace path and returns bytes/values. No state, no
constructors.

## What this package owns

- `worktree.go`: `DiffWorkspaceVsHead` (uncommitted tracked changes +
  untracked-not-ignored files, `git status` semantics) and
  `DiffBranchBaseToWorktree` (merge-base of the base branch → a
  synthetic tree of the current worktree, so committed + staged +
  unstaged + untracked share one patch stream). Plus the
  `IsGitRepository` probe.
- `commits.go`: `ListCommits` / `ListCommitsRange` (the per-commit
  selector rows, `base..head` newest first, capped at
  `maxListedCommits`), `ListRecentCommits` (plain `git log` from HEAD,
  merges included. Codex's own review-picker source, backing the
  `/review` commit completion; unborn HEAD is an empty answer),
  `CommitDiff` (a single commit's patch:
  first-parent for merge commits (matching how GitHub/GitLab render a
  commit), `diff-tree --root` for a root commit), and
  `ShowFileAtCommit` (hunk-gap expansion when a commit is selected).
- `refs.go`: the two ref resolvers, biased in opposite directions on
  purpose:
  - `resolveBaseRef` is the BASE side of every comparison (what a branch
    is measured against). Prefers the remote-tracking ref: the base
    branch's configured `@{upstream}` when it names one (a fork's `main`
    tracking `upstream/main`), else `origin/<base>`, else the local ref,
    else the remote-DWIM fallback below. A local `main` is only as fresh
    as the user's last fetch, and the merge base against a stale one
    under-reports the diff; preferring the remote-tracking ref makes the
    review pane agree with what the forge will show for the same branch.
    An upstream configured as another LOCAL branch (`branch.x.remote =
    .`) is rejected. It is not evidence of a remote. No fetch happens
    here: this reads whatever the last fetch left (worktree cuts and the
    background cadence own the refreshing).
  - `resolveNamedRef` is the ref the caller is DESCRIBING (the branch
    whose own commits are listed, e.g. `ListBranchCommits`' second
    argument). Local first, remote only as a fallback, because
    "which commits would deleting this branch lose" must count unpushed
    ones. Also what maps the picker's short names onto revisions: the
    picker projects "origin/feature" to "feature", which git's revision
    resolution won't DWIM on its own.
- `run.go` holds the subprocess plumbing: `runGit` variants with env scrubbing
  (`GIT_EXTERNAL_DIFF` / `GIT_DIFF_OPTS` cleared), a hard
  `maxDiffOutputBytes` stdout cap, and `WaitDelay` so a wedged pipe
  can't hang a review-pane load.
- `options.go`: `Options`, the last parameter of every patch producer
  (`DiffWorkspaceVsHead`, `DiffBranchBaseToWorktree`, `CommitDiff`).
  Its `gitArgs` builds the argv, so the canonical flag set
  (`--patch --minimal --no-color --no-ext-diff --no-textconv`) is
  declared once instead of per call site. Zero value = the exact
  patch.

## Ignore-whitespace (`Options.IgnoreWhitespace`)

The review pane's "hide whitespace changes" toggle. Passes `-w`
(`--ignore-all-space`) and nothing else. Deliberately NOT
`--ignore-blank-lines`, which would change which lines *exist* rather
than how they compare.

**Line numbering stays canonical.** `-w` narrows and drops hunks, but
the `@@` ranges it emits are still true file line numbers on both
sides, so a `(path, line)` anchor read off a `-w` patch names the same
physical line it would on the full patch. That is what lets the
diff-review comment flow keep creating comments while the toggle is
on; `TestIgnoreWhitespaceKeepsCanonicalLineNumbers` is the guard, and
the frontend's `-w`-aware orphan detection covers the one remaining
case (a draft whose line left the displayed patch).

## Safety invariants

- **The user's index is never touched.** Worktree snapshots build a
  temp `GIT_INDEX_FILE` + temp object dir (repo objects as
  alternates); nothing lands in the user's `.git`.
- **Repo-defined filters never execute.** Staging uses plumbing
  (`hash-object --no-filters` + `update-index --cacheinfo`), so an
  opened repo's clean/smudge filter commands are not an execution
  surface for automatic diffs.
- **Ref arguments are validated before hitting argv.** Commit SHAs
  must match `commitSHAPattern`; base refs reject empty and
  leading-`-` values; paths are `:`-joined into a single `show`
  argument.

## What does NOT belong here

- Regular git operations (branch, commit, push, fetch, forge CLIs).
  `internal/git` owns those.
- Deciding which thread/workspace to diff. `app_review_diffs.go` /
  `app_forge_review.go` resolve threads and call in.

## References

- `app_review_diffs.go`: the workspace / branch / per-commit bindings.
- `app_forge_review.go`: PR-scope commit listing over a local clone.
- `docs/architecture/review-pane-design.md`: the surface these diffs
  feed.
