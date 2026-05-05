# Revert Modes

Every real user message gets a Git checkpoint captured immediately before the
message is submitted to the provider. Reverting "to" a message means returning
the conversation to the state before that message: the selected prompt and all
later timeline rows are deleted, and the selected prompt is restored into the
composer draft.

Implementation lives in `internal/checkpoint/`, `app_checkpoint.go`, and the
message hover actions in `frontend/src/lib/components/chat/UserMessage.svelte`.

## Checkpoint Storage

Checkpoints are commits pointed at by hidden refs under
`refs/agent-overflow/checkpoints/<b64url(threadID)>/message/<uuid>` (see
`ThreadRefPrefix` in `internal/checkpoint/ref.go`). They don't appear in
`git log` or `git branch` by default because they sit outside the
`refs/heads` and `refs/tags` namespaces.

`Store.CaptureRef` snapshots every tracked-with-changes and
untracked-not-ignored file using a temp `GIT_INDEX_FILE` so the user's index is
never touched. The result is a `commit-tree` OID written with `update-ref`; the
author is always `Agent Overflow <agent-overflow@users.noreply.github.com>`.
Capture builds the temp index with `hash-object --no-filters` and
`update-index`, not `git add`, so repo-defined clean filters are not executed
by automatic checkpointing.

SQLite stores one row per real user message in `thread_checkpoints`. The
canonical lookup key is `(thread_id, user_item_id)`. `turn_index` is retained
for ordering and provider turn-boundary operations. Provider replay later stamps
`provider_user_message_id` and, for Claude, `provider_parent_uuid` onto the
same row for provider-message correlation. Claude in-thread revert and
message-fork rollback slice the current session by the checkpoint's
`turn_index` boundary because Claude session forks rewrite JSONL UUIDs; old
provider UUIDs are not stable after the first rollback.

## When Baselines Are Captured

`SendMessage` persists the optimistic user `items` row first, then calls
`captureMessageCheckpoint` before sending the prompt to the provider. This
ordering gives the checkpoint a stable `user_item_id` while preserving
Claude-Code-style semantics: the Git snapshot is the workspace state before
that prompt runs.

Tool path tracking is separate from checkpoint capture. Triage stages
structured edit/file-change paths on tool start, writes them into
`thread_tracked_files` after successful completion, and tags them with the
current `turn_index`. Conversation-and-files revert restores paths touched from
the selected message turn onward; it does not attempt to infer Bash side
effects or failed edits.

## Revert Modes

`App.RevertToMessageCheckpoint(threadID, userItemID, mode)` picks one of two
branches:

| Mode | Conversation | Working tree | Notes |
|---|---|---|---|
| `conversation-and-files` | selected user message and newer rows are deleted; provider history is rolled back | tracked agent-touched paths are restored from the selected message checkpoint | The full undo. |
| `conversation-only` | selected user message and newer rows are deleted; provider history is rolled back | untouched | Walk conversation history back while keeping on-disk edits. |

Revert rejects active turns, stops the provider session, rolls provider history
back, optionally restores files, deletes `items`/`turns` from the selected
turn onward, deletes checkpoint rows/refs from the selected message onward,
and writes the selected prompt back to `thread_drafts`.

Provider-side rollback differs by provider:

- **Codex** has a native `thread/rollback` wire method. The Go-side driver
  `rollbackCodexThread` lives in `internal/provider/codex/session_rollback.go`
  and uses the live session when one is active, else resumes a short-lived temp
  session just for the call.
- **Claude** has no rollback RPC. `revertClaudeThreadToMessage` slices the
  current Claude JSONL through the end of the turn immediately before the
  selected message using `internal/provider/claude/sessionfork`, then points
  `threads.session_ref` at the new session file. Turn 0 clears the Claude
  session entirely; later turns require a Claude session reference because
  silently clearing the whole provider context would be an off-by-one rollback.

## Cross-Thread Revert

Not supported. `RevertToMessageCheckpoint` takes a single `threadID`, validates
that the checkpoint ref belongs to that thread's namespace, and only deletes
rows/refs owned by that thread.

A thread's checkpoint refs are cleaned up when the thread is deleted via
`checkpoint.Store.CleanupThread` (in `internal/checkpoint/store.go`), which
`update-ref -d`s every ref matching `ThreadRefPattern(threadID)`.
