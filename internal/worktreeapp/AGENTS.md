# internal/worktreeapp/

Read-only worktree membership, status, picker-safety, and shared-workspace
activity application service.

## Ownership

- Symlink-canonical membership against git's registered worktree list. This
  asks `git worktree list` because callers need the worktree's branch RECORD;
  a caller that only needs membership must use `gitroot` instead, the way
  `gitapp.ResolveWorkspace` does — that path is per-keystroke hot and cannot
  afford a subprocess.
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
- `Activity` is the one busy-thread projection workspace REMOVAL gates on
  (`App.ensureWorkspaceChangeAllowed`), so a live frontend affordance and a
  backend refusal cannot disagree. Branch changes (checkout, create-branch,
  pull, sync) are deliberately NOT gated on it: the user owns the branch and
  switches it whenever they like, agent or no agent. Never add the check to
  them. Moving ONE thread out of its workspace
  (PrepareThreadWorktree / AttachThreadWorktree) keeps its own per-thread
  check; do not widen those to the directory.
- This service performs no filesystem mutation and owns no goroutine.
