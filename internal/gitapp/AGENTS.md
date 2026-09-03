# internal/gitapp/

Application coordination around `internal/git` and `internal/gitwatch`.

## Ownership

- `WorkspaceRef` and `ResolveWorkspace`: the one trust boundary where a
  caller-supplied workspace path is accepted, plus simple git reads/actions,
  branch-prune preview and exact-tip revalidation.
- One workspace-keyed git-status pump per canonical cwd, caller handle
  refcounts, bounded admission, and shutdown join.
- The unattended background-fetch cadence, common-dir deduplication, live
  settings gate, cancellation-before-join, and per-subject error memo.

`internal/app` retains stable Wails methods, transport connection cleanup,
typed event projection, lifecycle order, and git mutations whose safety depends
on live thread/session ordering.

## Invariants

- Stop background fetch by cancelling its context before waiting; the fetch
  subprocess must not hold shutdown until its network timeout.
- Close gitwatch and join every pump before the event transport or store closes.
- A stale prune preview never authorizes deletion: re-read eligibility and
  require the exact confirmed tip.
- Status streams are keyed by canonical workspace, not caller handle. Multiple
  panes share one upstream subscription and one emitted update.
- Keep background fetch non-interactive and origin-only through
  `internal/git`; this package never shells out directly.
- Workspace-scoped git RPCs take a `WorkspaceRef`, never a thread id. A
  workspace is a directory; a thread is a conversation that happens to sit in
  one, and two threads sharing a checkout is first-class. Resolving a
  directory out of a conversation is what made a draft placeholder unable to
  ask for git status at all.
- Agent activity never gates a BRANCH change. Checkout, create-branch, pull
  and sync run whenever the user asks, whatever any thread in the directory is
  doing (user ruling, 2026-09-02). Only deleting the directory
  (`RemoveOtherWorktree`) and moving a thread to another checkout keep their
  activity checks.
- `ResolveWorkspace` is the only place a caller-supplied path becomes a
  directory this process operates on. It accepts an empty path (the project
  root), the project root, or one of that project's worktrees; anything else
  is refused. No RPC may keep a private path-resolution path beside it.
- `ResolveWorkspace` MUST NOT spawn git. It runs per @-mention keystroke, per
  hunk-gap click and per status subscribe, so membership is answered by
  `gitroot` (filesystem reads of git's own layout, never a subprocess) —
  `MainRoot(workspace)` equal to the project path, AND a `.git` entry on the
  workspace itself so a mere SUBDIRECTORY of the project is refused.
  `worktreeapp.Find` still asks `git worktree list`, because it needs the
  worktree's branch RECORD, which the on-disk layout does not carry.
  `TestResolveWorkspaceSpawnsNoGit` empties PATH to hold this.
