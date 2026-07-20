# gitdiff/

Review-pane diff sources computed by shelling out to `git`. Every
function takes a workspace path and returns bytes/values — no state, no
constructors.

## What this package owns

- `worktree.go` — `DiffWorkspaceVsHead` (uncommitted tracked changes +
  untracked-not-ignored files, `git status` semantics) and
  `DiffBranchBaseToWorktree` (merge-base of the base branch → a
  synthetic tree of the current worktree, so committed + staged +
  unstaged + untracked share one patch stream). Plus the
  `IsGitRepository` / `HasHeadCommit` probes.
- `commits.go` — `ListCommits` / `ListCommitsRange` (the per-commit
  selector rows, `base..head` newest first, capped at
  `maxListedCommits`), `CommitDiff` (a single commit's patch:
  first-parent for merge commits — matching how GitHub/GitLab render a
  commit — `diff-tree --root` for a root commit), and
  `ShowFileAtCommit` (hunk-gap expansion when a commit is selected).
- `legacyrefs.go` — `CleanupLegacyCheckpointRefs`, the every-boot
  sweeper draining the hidden `refs/agent-overflow/*` namespace the
  removed checkpoint machinery wrote. Called from
  `app_legacy_checkpoint_refs.go`.
- `run.go` — subprocess plumbing: `runGit` variants with env scrubbing
  (`GIT_EXTERNAL_DIFF` / `GIT_DIFF_OPTS` cleared), a hard
  `maxDiffOutputBytes` stdout cap, and `WaitDelay` so a wedged pipe
  can't hang a review-pane load.

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

- Regular git operations (branch, commit, push, fetch, forge CLIs) —
  `internal/git` owns those.
- Deciding which thread/workspace to diff — `app_review_diffs.go` /
  `app_forge_review.go` resolve threads and call in.

## References

- `app_review_diffs.go` — the workspace / branch / per-commit bindings.
- `app_forge_review.go` — PR-scope commit listing over a local clone.
- `docs/architecture/review-pane-design.md` — the surface these diffs
  feed.
