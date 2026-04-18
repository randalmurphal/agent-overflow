# Revert Modes

Every turn is bracketed by a git checkpoint. The frontend offers four
ways to walk a thread back to one. Implementation lives in
`internal/checkpoint/` and `app_checkpoint.go`.

## Checkpoint Storage

Checkpoints are commits pointed at by hidden refs under
`refs/agent-overflow/checkpoints/<b64url(threadID)>/turn/<N>`
(`internal/checkpoint/ref.go:18`). They don't appear in `git log` or
`git branch` by default because they sit outside the `refs/heads` and
`refs/tags` namespaces.

`Store.CaptureBaseline` snapshots every tracked-with-changes and
untracked-not-ignored file using a temp `GIT_INDEX_FILE` so the user's
index is never touched. The result is a `commit-tree` OID written with
`update-ref`; the author is always `Agent Overflow
<agent-overflow@users.noreply.github.com>`
(`internal/checkpoint/store.go:24`).

## When Baselines Are Captured

`Router.handleTurnStart` runs `captureBaselineForTurn` on every
`EventTurnStart` (`internal/triage/router.go:292`). The capture is
idempotent — if a `(thread, turn)` pair already has a ref (common when
Claude re-sends `system.init` after an interrupt), the existing row
and ref are deleted before a fresh baseline is written so git and
SQLite never drift
(`internal/triage/router.go:353`). Capture failure is non-fatal: the
turn proceeds, and a `checkpoint:error` event surfaces in the UI.

The turn *diff* is not captured explicitly. `GetTurnDiff` derives it on
demand as `diff(turn-N-baseline → turn-(N+1)-baseline)`, or against
the current worktree for the tail turn (`app_checkpoint.go:32`).

## Revert Modes

`App.RevertToTurn(threadID, turnIndex, mode)` picks one of four
branches. The checkpoint at `turnIndex` is the state *before* turn N
ran, so every mode drops turn N and everything after it.

| Mode | Conversation | Working tree | Notes |
|---|---|---|---|
| `fork` | untouched | untouched | Creates a new thread via `ForkThread`, leaves source alone. Returns the new thread ID. |
| `revert-both` | truncated + provider-rolled-back | restored from ref | The full undo. |
| `revert-conversation` | truncated + provider-rolled-back | untouched | Walk history back, keep on-disk edits. |
| `revert-code` | untouched | restored from ref | Restore files, keep talking about the same turns. |

In-place modes (`revert-*`) always stop the active session first
(`app_checkpoint.go:163`) — running a turn through a revert produces
undefined interleavings. When the conversation side is reverted, the
code also calls `DeleteItemsAfterTurn` and `DeleteCheckpointsAfterTurn`
so the timeline stops at `turnIndex - 1` and the ref set stops at
`turnIndex`.

Provider-side rollback differs by provider:

- **Codex** has a native `thread/rollback` wire method
  (`internal/provider/codex/session_rollback.go`). `rollbackCodexThread`
  uses the live session when one is active, else resumes a short-lived
  temp session just for the call.
- **Claude** has no equivalent. `revertProviderConversation` clears
  `SessionRef` and `PendingForkRef` (`app_checkpoint.go:220`) so the
  next message starts a fresh session. The old session file is left on
  disk.

## Cross-Thread Revert

Not supported. `RevertToTurn` takes a single `threadID` and all paths
(`GetCheckpoint`, `DeleteItemsAfterTurn`, `RestoreWorktree`) scope to
that thread. `fork` mode *creates* a new thread via `ForkThread`
(`app_thread_fork.go:31`), but the source thread is never modified.

A thread's checkpoints are cleaned up when the thread is deleted via
`Store.CleanupThread` (`internal/checkpoint/store.go:223`), which
`update-ref -d`s every ref matching `ThreadRefPattern(threadID)`.
