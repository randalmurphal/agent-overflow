# Revert Modes

Every turn is bracketed by a git checkpoint. The frontend offers two
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

`Router.handleTurnStart` captures checkpoint turn count 0 before the
first turn only (see `internal/triage/turn_lifecycle.go`). Completed
turn checkpoints are captured on `EventTurnComplete`; checkpoint turn
count N represents the workspace after completed turn N. Capture
failure is non-fatal: the turn proceeds, and a `checkpoint:error` event
surfaces in the UI.

The turn *diff* is not captured explicitly. `GetCheckpointRangeDiff`
(in `app_checkpoint.go`) derives it on demand as
`diff(checkpoint-N → checkpoint-M)` for finalized checkpoint ranges.

## Revert Modes

`App.RevertToCheckpoint(threadID, checkpointTurnCount, mode)` picks
one of two branches. Checkpoint turn count 0 is the initial baseline;
checkpoint turn count N is the state after completed turn N, so
conversation rollback keeps timeline turns through `N-1`.

| Mode | Conversation | Working tree | Notes |
|---|---|---|---|
| `conversation-and-files` | truncated + provider-rolled-back | restored from ref | The full undo. |
| `conversation-only` | truncated + provider-rolled-back | untouched | Walk history back, keep on-disk edits. |

Revert always stops the active session first — running a turn through a
revert produces undefined interleavings. The code calls
`DeleteItemsAfterTurn` and `DeleteCheckpointsAfterTurn` so the timeline
stops at `checkpointTurnCount - 1` and the ref set stops at
`checkpointTurnCount`.

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

Not supported. `RevertToCheckpoint` takes a single `threadID` and all
paths (`GetCheckpointByTurnCount`, `DeleteItemsAfterTurn`,
`RestoreWorktree`) scope to that thread.

A thread's checkpoints are cleaned up when the thread is deleted via
`checkpoint.Store.CleanupThread` (in `internal/checkpoint/store.go`),
which `update-ref -d`s every ref matching
`ThreadRefPattern(threadID)`.
