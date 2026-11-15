# internal/gitapp/

Application coordination around `internal/git` and `internal/gitwatch`.

## Ownership

- `WorkspaceRef` and `ResolveWorkspace`: the one checked path-resolution
  entry point for caller-supplied workspace paths, plus simple git
  reads/actions, branch-prune preview and exact-tip revalidation.
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
- Every workspace status a method answers with is read through
  `publishStatus` (`gitwatch.Manager.Publish`), so it carries the `Seq` of its
  read and is what the workspace's stream publishes, or the read itself when
  nothing could publish it (nobody watches, or a `Suppress` hold). Do not
  read `Core.Status` or `Core.StatusFast` for a client beside it, and take
  anything derived from the answer, such as the branch a checkout records,
  from that status. With no watch manager the answer carries no `Seq`.
- Keep background fetch non-interactive and origin-only through
  `internal/git`; this package never shells out directly.
- Workspace-scoped git RPCs take a `WorkspaceRef`, never a thread id. A
  workspace can serve multiple threads or a draft with no thread yet.
- Agent activity never gates a BRANCH change. Checkout, create-branch, pull
  and sync run whenever the user asks, whatever any thread in the directory is
  doing. Only deleting the directory
  (`RemoveOtherWorktree`) and moving a thread to another checkout keep their
  activity checks.
- `ResolveWorkspace` is the only place a caller-supplied path becomes a
  directory this process operates on. It accepts an empty path (the project
  root), the project root, or one of that project's worktrees; anything else
  is refused. No RPC may keep a private path-resolution path beside it.
- `ResolveWorkspace` MUST NOT spawn git. It runs per @-mention keystroke, per
  hunk-gap click and per status subscribe, so membership is answered by
  `gitroot` (filesystem reads of git's own layout, never a subprocess);
  `MainRoot(workspace)` equal to the project path, AND a `.git` entry on the
  workspace itself so a mere SUBDIRECTORY of the project is refused.
  `worktreeapp.Find` still asks `git worktree list`, because it needs the
  worktree's branch RECORD, which the on-disk layout does not carry.
  `TestResolveWorkspaceSpawnsNoGit` empties PATH to hold this.
- Workspace status emits plain Git state before asynchronous PR/MR enrichment.
  Consumers must react to the enriched update rather than capture PR state
  only at mount.
