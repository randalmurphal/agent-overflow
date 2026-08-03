# internal/git/

Wraps `git` and forge-CLI commands (`gh` for GitHub, `glab` for
GitLab) for repository operations used by the rest of the app:
status, diff, branches, commits, worktrees, and PR/MR creation.

## Layout

- `core.go` — `Core` struct with `Execute` / `runBinary`, the shared
  command runner (`runSpec` + the `commandSpec` it takes) with timeouts
  and stdout/stderr size caps; PR cache, forge cache, and fetch cache
  plumbing; `revParsePath` (the shared directory-valued `rev-parse`
  helper behind watch-root discovery and `CommonDir`). Every
  subprocess runs with `GIT_OPTIONAL_LOCKS=0` so the background status
  cadence never opportunistically rewrites `.git/index` (which would
  fire fs events back into gitwatch); mandatory locks (add/commit) are
  unaffected. `runLocaleC` / `executeLocaleC` add `LC_ALL=C LANG=C` for
  the specific invocations whose output this package pattern-matches in
  English (`status`, `rev-parse` in watch_roots, `stash push`,
  `merge-tree`) — a git built with NLS translates those messages. It is
  per-command on purpose: `LC_ALL` also pins date/number/collation
  formatting other commands' output may want in the user's locale.
  Every subprocess ALSO runs with `nonInteractiveEnv`
  (`GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=`, `SSH_ASKPASS=`,
  `SSH_ASKPASS_REQUIRE=never`, `GCM_INTERACTIVE=never`) unless it opts
  out via `commandSpec.allowCredentialPrompt` — the default is the safe
  one so a background caller cannot raise a credential dialog nobody
  asked for. Only user-initiated network commands opt out:
  `Push` (but not `PushUnattended`), `Pull`, `SyncBranch`,
  `PruneRemotes`, `FetchRefOID`, `FetchBranch`, and the forge CLIs'
  `CreatePR` paths (which shell out to `git push` themselves).
  Neither `MaybeFetchRemotes` nor `FetchRemotesBackground` opts out —
  nobody asked for either of them.
  `GIT_TERMINAL_PROMPT=0` alone is not enough: git tries an askpass
  helper first, so `GIT_ASKPASS=` (empty, not unset) is what closes the
  chain. Coverage: `noninteractive_test.go`.
- `fetch_background.go` — `CommonDir` (canonical `git rev-parse
  --git-common-dir`, memoized per cwd) and `FetchRemotesBackground`, the
  throttled fetch behind the app's 5-minute cadence
  (`app_git_background_fetch.go`, settings `BackgroundGitFetch`). The
  fetch cache in `core.go` is keyed on the common dir, NOT the repo
  root, so N worktrees of one repository share one window — and that
  window is shared with `MaybeFetchRemotes` / `PruneRemotes` so the
  branch picker and the cadence can never double-fetch. Background
  fetches are additionally single-flighted per repository. Origin only,
  `--quiet`, never `--prune` and never extra tags: a timer must not
  move or delete refs the user can see.
- `actions.go` — staging, commits, push/pull, branch create/checkout/
  rename. Worktree CRUD (`CreateWorktree*`, `RemoveWorktree*`,
  `ListWorktrees`) lives in `core.go` next to the `Worktree` struct
  it returns.
- `disposition.go` — clean ff-or-merge-commit disposition with conflict
  preflight and durable-result SHA reporting.
- `stash.go` — `git stash` helpers (push, apply-by-message,
  drop-by-message) used by the worktree carry-over and branch-from
  destructive flows in the app layer. Also `RandomStashSuffix()` —
  short hex token for collision-free stash-message tagging.
- `status.go` — `GitStatus` shape + status aggregation (branch,
  ahead/behind, open PR, detected forge) and small status-related
  primitives (`CountWorkingTreeChanges`, `CountUnpushedCommits`,
  `upstreamFor`, `CurrentBranch`, `BranchIsDefault`).
  `baseStatus` fans its six independent probes (status, numstat diff,
  default branch, origin remote, untracked scan, pending operation) out
  concurrently and joins with the serial version's exact error
  precedence — the refresh costs max(probe) instead of the sum, which is
  what makes the gitwatch cadence usable on a repo reached over WSL's 9P
  bridge (2.4s serial vs 0.6s fanned out there; 14ms vs 6ms on ext4).
- `status_branches.go` — `GitBranch` shape, branch-list parsing,
  default-branch helpers, and remote-name helpers.
- `status_pr_cache.go` — open-PR lookup cache used by `Status` /
  `StatusFast`; `InvalidatePRCache` lives here too. A failed forge
  lookup keeps serving the branch's last successfully-read PR next to
  the error (the badge must not blink out through a `gh` rate-limit or
  auth blip) — dropped only by a successful lookup that finds none, by
  explicit invalidation, or when the origin remote reads cleanly as a
  *different* URL than the one that PR was found under. A failed read
  of the origin is unknown, never "no remote", and never invalidates.
  Entries survive `prStickyRetention` past their refresh TTL so an
  unrelated branch's sweep can't delete the fallback.
- `status_untracked.go` — untracked-file insertion/file counting for
  the status badge, including the bounded line scanner and the
  per-workspace (size, mtime)-keyed line-count memo that keeps the
  gitwatch refresh cadence from re-reading every untracked file's
  content on each scan. Cache hits replay before the budget gate
  (hits cost no I/O); files written within the last ~2s are not
  memoized (git's "racily clean" analog).
- `status_pending.go` — pending merge/rebase/bisect detection via the
  resolved git directory.
- `watch_roots.go` — live-status watcher root discovery. Prunes
  git-ignored subtrees from the workspace watch (ignored content can
  never change status; node_modules alone is thousands of inotify
  watches): ancestors of pruned boundaries become non-recursive
  `KindAncestor` roots, surviving subtrees recursive `KindSubtree`
  roots. `WatchRootKind` tells the watcher which events under a root
  can invalidate the root set (kinds are ordered by trigger surface so
  normalization merges duplicates by max). Git metadata is watched
  narrowly as `KindGitMeta` (git dir non-recursive + refs/ + info/,
  never objects/; index/exclude/config writes are rebuild triggers); a
  linked worktree's private gitdir plus the shared common dir get the
  same treatment. The global ignore file (core.excludesFile) is watched
  via its parent dir with `TriggerFile` narrowing events to that one
  basename. Root count is capped at `maxPrunedWatchRoots` (1024 — real
  Python repos measure 300-500); overflow degrades by depth (retry with
  boundaries at most 3, 2, 1 segments deep — shallow boundaries are the
  big trees worth pruning, deep scattered `__pycache__` is what explodes
  the count) before falling back to the single recursive root.
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
- `pr_url.go` — parses the PR/MR URLs returned by forge `CreatePR` calls
  back into validated `PRReference` coordinates for later review reads.
- `forge_detect.go` — origin URL classification (github / gitlab / "")
  with TTL'd cache shared across `Status` and `forgeFor`. `recordOrigin`
  is the only writer, so a classification is never cached apart from the
  `originIdentity` it was derived from — `status_pr_cache.go` reads that
  identity back via `cachedOrigin` instead of re-shelling `git remote
  get-url`. Public `Core.InvalidateForgeCache(cwd)` drops the cached
  entry so callers that know the origin URL just changed can skip the
  TTL window.
- `github.go` — `githubForge` implementation backed by the `gh` CLI;
  thin `Core.CreatePR` / `Core.ListOpenPRs` wrappers that dispatch
  through `forgeFor`. Every `--json` field list in this file is governed
  by the version-drift rule on the `githubForge` type: `gh` omits fields
  older releases never had (`headRepository.nameWithOwner` before 2.47),
  so a new field must decode as optional or an old `gh` silently drops
  the row. Today's lists are narrow enough to be immune by accident —
  read the rule before widening one.
- `gitlab.go` — `gitlabForge` implementation backed by the `glab` CLI.
- `ci.go` — forge-agnostic CI shapes (`CIPipeline`/`CIStage`/`CIJob`/
  `CIStep`), the normalized status vocabulary + `NormalizeCIStatus` /
  `AggregateCIStatus`, and `ValidateCIJobID`. "Stage" is GitLab's
  pipeline stage or the GitHub workflow name.
- `ci_github.go` / `ci_gitlab.go` — per-forge `ListPRCIJobs` +
  `GetCIJobLog`: GitHub resolves workflow runs from the rollup's
  `detailsUrl` job links and fans out `gh run view --json jobs` (steps
  included); GitLab reads the MR's `head_pipeline` then pages
  `/pipelines/:id/jobs` (stage order recovered by ascending job id)
  and fetches `/jobs/:id/trace`, cleaned by `cleanGitLabTrace`
  (timestamps kept; stream flags, `section_start/end` markers,
  erase-line escapes, and `\r` overwrites resolved). Logs are capped
  at `maxCILogBytes`.
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
  - Review-pane diff computation — `internal/gitdiff` owns those
    subprocess pipelines (temp-index worktree snapshots, per-commit
    patches, commit lists).
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

- `internal/gitdiff` — review-pane diff subprocess pipelines (not
  regular git ops).
- `internal/testutil/git.go` — `InitGitRepo` / `RunGit` helpers for
  tests. Note `testutil.CanonicalPath` intentionally duplicates this
  package's helper to avoid a circular import.
