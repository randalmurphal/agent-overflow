# internal/gitapp/

Application coordination around `internal/git` and `internal/gitwatch`.

## Ownership

- Thread/project path resolution for git bindings, simple git reads/actions,
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
