# Revert Modes

Every turn is bracketed by a git checkpoint. The frontend offers four
ways to walk a thread back to one. Implementation lives in
`internal/checkpoint/` and `app_checkpoint.go`.

## Checkpoint Storage

Checkpoints are commits pointed at by hidden refs under
`refs/agent-overflow/checkpoints/<b64url(threadID)>/turn/<N>` (see
`ThreadRefPattern` in `internal/checkpoint/ref.go`). They don't appear
in `git log` or `git branch` by default because they sit outside the
`refs/heads` and `refs/tags` namespaces.

`Store.CaptureBaseline` snapshots every tracked-with-changes and
untracked-not-ignored file using a temp `GIT_INDEX_FILE` so the user's
index is never touched. The result is a `commit-tree` OID written with
`update-ref`; the author is always `Agent Overflow
<agent-overflow@users.noreply.github.com>` (see
`Store.CaptureBaseline` in `internal/checkpoint/store.go`).

## When Baselines Are Captured

`Router.handleTurnStart` runs `captureBaselineForTurn` on every
`EventTurnStart` (see `internal/triage/turn_lifecycle.go`). The
capture is idempotent — if a `(thread, turn)` pair already has a ref
(common when Claude re-sends `system.init` after an interrupt), the
existing row and ref are deleted before a fresh baseline is written
so git and SQLite never drift (see `captureBaselineForTurn` for the
delete-then-insert path). Capture failure is non-fatal: the turn
proceeds, and a `checkpoint:error` event surfaces in the UI.

The turn *diff* is not captured explicitly. `GetTurnDiff` (in
`app_checkpoint.go`) derives it on demand as
`diff(turn-N-baseline → turn-(N+1)-baseline)`, or against the current
worktree for the tail turn.

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

In-place modes (`revert-*`) always stop the active session first (see
`RevertToTurn` in `app_checkpoint.go`) — running a turn through a
revert produces undefined interleavings. When the conversation side is
reverted, the code also calls `DeleteItemsAfterTurn` and
`DeleteCheckpointsAfterTurn` so the timeline stops at `turnIndex - 1`
and the ref set stops at `turnIndex`.

Provider-side rollback differs by provider:

- **Codex** has a native `thread/rollback` wire method. The Go-side
  driver `rollbackCodexThread` lives in
  `internal/provider/codex/session_rollback.go` and uses the live
  session when one is active, else resumes a short-lived temp session
  just for the call.
- **Claude** has no equivalent. `revertProviderConversation` (in
  `app_checkpoint.go`) clears `SessionRef` and `PendingForkRef` so the
  next message starts a fresh session. The old session file is left on
  disk.

## Cross-Thread Revert

Not supported. `RevertToTurn` takes a single `threadID` and all paths
(`GetCheckpoint`, `DeleteItemsAfterTurn`, `RestoreWorktree`) scope to
that thread. `fork` mode *creates* a new thread via `App.ForkThread`
(in `app_thread_fork.go`), but the source thread is never modified.

A thread's checkpoints are cleaned up when the thread is deleted via
`checkpoint.Store.CleanupThread` (in `internal/checkpoint/store.go`),
which `update-ref -d`s every ref matching
`ThreadRefPattern(threadID)`.
