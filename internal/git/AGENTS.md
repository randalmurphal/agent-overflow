# internal/git/

Wraps `git` and forge-CLI commands (`gh` for GitHub, `glab` for
GitLab) for repository operations used by the rest of the app:
status, diff, branches, commits, worktrees, and PR/MR creation.

## Layout

- `core.go` — `Core` struct with `Execute` / `runBinary`, the shared
  command runner with timeouts and stdout/stderr size caps; PR cache
  and forge cache plumbing.
- `actions.go` — staging, commits, push/pull, branch create/checkout/
  rename. Worktree CRUD (`CreateWorktree*`, `RemoveWorktree*`,
  `ListWorktrees`) lives in `core.go` next to the `Worktree` struct
  it returns.
- `stash.go` — `git stash` helpers (push, apply-by-message,
  drop-by-message) used by the worktree carry-over and branch-from
  destructive flows in the app layer. Also `RandomStashSuffix()` —
  short hex token for collision-free stash-message tagging.
- `status.go` — `GitStatus` shape + status aggregation (branch,
  ahead/behind, open PR, detected forge) and small status-related
  primitives (`CountWorkingTreeChanges`, `CountUnpushedCommits`,
  `upstreamFor`, `CurrentBranch`, `BranchIsDefault`).
- `status_branches.go` — `GitBranch` shape, branch-list parsing,
  default-branch helpers, and remote-name helpers.
- `status_pr_cache.go` — open-PR lookup cache used by `Status` /
  `StatusFast`; `InvalidatePRCache` lives here too.
- `status_untracked.go` — untracked-file insertion/file counting for
  the status badge, including the bounded line scanner.
- `status_pending.go` — pending merge/rebase/bisect detection via the
  resolved git directory.
- `watch_roots.go` — live-status watcher root discovery, including
  linked-worktree gitdir/common-dir metadata roots and recursive vs
  non-recursive watch intent.
- `worktree_paths.go` — pure path helpers backing the app layer's
  worktree creation: `SanitizeWorktreePathSegment` (branch → fs-safe
  directory name), `DefaultWorktreesBaseDir` (the `<repo>-worktrees`
  sibling convention), and `UniqueWorktreePath` (collision-free
  `-N` suffixing with a unix-millis fallback).
- `branch_names.go` — branch-name sanitation.
- `commit_context.go` — commit-message context gathering for model-
  assisted commits.
- `forge.go` — `Forge` interface + `PRReference` / `PRMetadata` /
  `PRFile` types; `SplitProjectForForge` per-forge namespace splitter;
  `ValidateProjectSegment` safe-name check; `BuildPRAnchor` /
  `PRAnchorScheme` for the `pr://forge/namespace/repo` pseudo-anchor
  used as a `Project.Path` key when no local clone matches;
  `NormalizePRState` canonical lowercase mapping; nullForge sentinel
  for unsupported remotes.
- `forge_detect.go` — origin URL classification (github / gitlab / "")
  with TTL'd cache shared across `Status` and `forgeFor`. Public
  `Core.InvalidateForgeCache(cwd)` drops the cached entry so callers
  that know the origin URL just changed can skip the TTL window.
- `github.go` — `githubForge` implementation backed by the `gh` CLI;
  thin `Core.CreatePR` / `Core.ListOpenPRs` wrappers that dispatch
  through `forgeFor`.
- `gitlab.go` — `gitlabForge` implementation backed by the `glab` CLI.
- `paths.go` — path canonicalization (`CanonicalPath`,
  `SameFilesystemPath`) for the symlink-heavy macOS tmp dir cases.
- `results.go` — small result-type declarations shared across actions.

## Responsibility boundary

- What BELONGS here:
  - Invoking `git` / `gh` / `glab` subprocesses with safe defaults
    (timeout, size cap, env scrubbing).
  - Parsing their output into typed Go shapes.
  - Forge classification of origin URLs.
  - Canonical path comparison helpers (the one source of truth).
- What does NOT belong here:
  - Decisions about *when* to stage or push; that's `app.go`.
  - Checkpoint / hidden-ref manipulation — `internal/checkpoint` owns
    the hidden namespace.
  - Non-git file operations.

## Extension points

- To wrap a new `git` command: add the function to the matching file
  (or create a new one and list it here). Reuse `Core.Execute`; do not
  shell out directly.
- To add a new forge: implement the `Forge` interface in a new
  `<host>.go` file, register it in `NewCore`'s `forges` map, extend
  `classifyOriginURL` for the new host, and add `<host>_test.go` with
  PATH-mock parity to `github_test.go` / `gitlab_test.go`.
- To add a new GitStatus field: extend `GitStatus`, the typed `Equal`
  comparator, and the parser. Cover with a status parser test.

## Anti-patterns

- Do NOT use `os/exec` directly. Route through `Core.runBinary` so
  timeouts, size caps, and logging stay consistent.
- Do NOT assume paths are already canonical. `macOS` `/tmp` symlinks
  exist and the test suite hits them — use `CanonicalPath`.
- Do NOT stage silently when the caller asked for a commit. `Commit`
  explicitly refuses to stage; the caller must call `StageAll` first.
- Do NOT bypass the `Forge` interface to call `gh` / `glab` directly
  from app code. Add the operation to `Forge` and route through
  `Core.forgeFor` (auto-detect) or `Core.ForgeByID` (caller knows id).

## References

- `internal/checkpoint` — hidden-ref snapshots (not regular git ops).
- `internal/testutil/git.go` — `InitGitRepo` / `RunGit` helpers for
  tests. Note `testutil.CanonicalPath` intentionally duplicates this
  package's helper to avoid a circular import.
