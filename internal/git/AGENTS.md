# internal/git/

Wraps `git` and `gh` CLI commands for repository operations used by the
rest of the app: status, diff, branches, commits, worktrees, and pull
requests.

## Layout

- `core.go` — `Core` struct with `Execute` / `runBinary`, the shared
  command runner with timeouts and stdout/stderr size caps.
- `actions.go` — staging, commits, resets, pushes.
- `status.go` — `GitStatus` shape + status aggregation (branch,
  ahead/behind, pending merge/rebase/bisect, open PR).
- `branch_names.go` — branch-name sanitation.
- `commit_context.go` — commit-message context gathering for model-
  assisted commits.
- `github.go` — `gh` CLI wrappers for PR create/list/status.
- `paths.go` — path canonicalization (`CanonicalPath`,
  `SameFilesystemPath`) for the symlink-heavy macOS tmp dir cases.
- `results.go` — small result-type declarations shared across actions.

## Responsibility boundary

- What BELONGS here:
  - Invoking `git` / `gh` subprocesses with safe defaults (timeout,
    size cap, env scrubbing).
  - Parsing their output into typed Go shapes.
  - Canonical path comparison helpers (the one source of truth).
- What does NOT belong here:
  - Decisions about *when* to stage or push; that's `app.go`.
  - Checkpoint / hidden-ref manipulation — `internal/checkpoint` owns
    the hidden namespace.
  - Non-git file operations.

## Extension points

- To wrap a new `git` or `gh` command: add the function to the matching
  file (or create a new one and list it here). Reuse `Core.Execute`;
  do not shell out directly.
- To add a new GitStatus field: extend `GitStatus` + update the
  parser. Cover the new field with a status parser test.

## Anti-patterns

- Do NOT use `os/exec` directly. Route through `Core.Execute` so
  timeouts, size caps, and logging stay consistent.
- Do NOT assume paths are already canonical. `macOS` `/tmp` symlinks
  exist and the test suite hits them — use `CanonicalPath`.
- Do NOT stage silently when the caller asked for a commit. `Commit`
  explicitly refuses to stage; the caller must call `StageAll` first.

## References

- `internal/checkpoint` — hidden-ref snapshots (not regular git ops).
- `internal/testutil/git.go` — `InitGitRepo` / `RunGit` helpers for
  tests. Note `testutil.CanonicalPath` intentionally duplicates this
  package's helper to avoid a circular import.
