# internal/checkpoint/

Captures hidden git-ref snapshots of a workspace before user messages so
the UI can diff and roll back to message boundaries. Refs live under
`refs/agent-overflow/checkpoints/`.

## Layout

- `ref.go` — pure helpers for the ref namespace: `EncodeThreadID`,
  `ThreadRefPattern`, `ThreadRefPrefix`, `IsThreadRef`. Thread IDs are
  base64url-encoded so every character is path-safe.
- `store.go` — `Store` with `CaptureRef`, ref copy, restore, and diff
  helpers. Shells out to `git` via a temp
  `GIT_INDEX_FILE` so the user's index is never touched. Capture builds
  the temp index with plumbing (`hash-object --no-filters` +
  `update-index`) rather than `git add`, because automatic checkpoints
  must not execute repo-defined clean filters.

## Responsibility boundary

- What BELONGS here:
  - Building and parsing checkpoint ref names.
  - Writing the hidden ref (`git update-ref`) without mutating the
    user-visible index, branches, or worktree.
  - Diff/restore against a previously captured ref.
- What does NOT belong here:
  - Checkpoint row bookkeeping (thread id, user item id, turn index, ref name) —
    `internal/store/checkpoints.go` owns the table.
  - Decisions about *when* to capture — `app_send.go` captures through
    `app_checkpoint.go` immediately before provider send.

## Extension points

- To add a new ref category (e.g. per-file snapshots): introduce a new
  ref suffix under the existing thread prefix. Keep categories under
  `refs/agent-overflow/` so they stay hidden from `git log` /
  `git branch`.
- To change capture scope: modify `captureToRef` in `store.go`; every
  callsite passes through this so a single change sweeps the package.

## Anti-patterns

- Do NOT touch the user's `.git/index` or HEAD. The temp
  `GIT_INDEX_FILE` pattern is the whole reason captures can run
  concurrently with the user editing.
- Do NOT write refs outside `refs/agent-overflow/checkpoints/`. Other
  namespaces are reserved for future categories or user-facing refs.
- Do NOT assume HEAD exists. Fresh-init repos skip the seed step; keep
  the probe.

## References

- `internal/store/checkpoints.go` — row / ref linkage.
- `docs/architecture/revert-modes.md` — fork vs restore semantics.
- Forge's mechanism is the design ancestor; diverge only deliberately.
