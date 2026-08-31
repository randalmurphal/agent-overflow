# internal/worktreeapp/

Read-only worktree membership, status, picker-safety, and shared-workspace
activity application service.

## Ownership

- Symlink-canonical membership against git's registered worktree list.
- Thread references by either `workspace_path` or `worktree_path`.
- Directory-wide activity aggregated across every matching thread, including
  transient background-task ids supplied by root.
- Worktree dirty/unpushed/upstream/attachment status and picker
  `DeleteBlocked` projection.

## Boundary

Filesystem deletion and workspace switching remain in root. Those sagas order
thread locks, activity rechecks, worktree-setup cancellation, git removal,
thread persistence, Claude transcript relocation, events, and provider-session
restart. Moving them here would require an App-shaped host and hide the
destructive order this package exists to inform.

## Invariants

- Canonicalize the target once, then compare both stored path columns.
- Activity and picker blocking are unscoped by project: shared directories are
  first-class and a thread from any project row can make one unsafe.
- A force flag may bypass loss-of-work confirmation in root; it must never
  bypass registered-worktree membership.
- This service performs no filesystem mutation and owns no goroutine.
