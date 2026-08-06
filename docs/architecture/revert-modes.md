# Message Anchors & Conversation Rollback

Every real user message gets a `message_anchors` row written immediately
after its `items` row persists. The anchor carries the provider-side
identity of the message — Claude's wire uuid + parent uuid, and the AO
`turn_index` Codex anchors resolve against — so the three
message-boundary operations can slice provider history at that message:

- **Fork-from-message** (`app_thread_fork.go`) — clone the thread up to
  a chosen user message into a new thread. The source thread is left
  untouched.
- **Revert-on-interrupt** (`app_revert_on_interrupt.go`) — the Stop/Esc
  un-send: when exactly one user message is in flight with no assistant
  content yet, Stop rolls the message back (conversation only) and
  restores it into the composer draft instead of leaving a dangling
  turn.
- **Edit-and-resend** (`app_revert_and_resend.go`) — the edit-in-place
  affordance on a past user message on an IDLE thread: one saga stages
  the edited text in `thread_drafts` as a crash copy (merged ahead of
  any composer work-in-progress), rolls back, emits
  `user_message:reverted` with `draftPendingResend`, resends the
  replacement through `sendMessageLocked`, and settles the draft row
  back to the user's work-in-progress. The whole sequence holds one
  acquisition of the thread's action lock, so no send, revert, or
  session start can slip between the truncation and the replacement.

All three are conversation-level operations. There is no working-tree
revert: the per-message git-checkpoint machinery (hidden
`refs/agent-overflow/*` snapshot refs, `thread_tracked_files`,
revert-to-message with file restore) was removed — it flooded repo
tooling with hundreds of hidden refs and the file-restore path was
never used. Agents revert their own edits when asked.

## Anchor storage

SQLite only (`internal/store/message_anchors.go`). Primary key
`(thread_id, user_item_id)`, `ON DELETE CASCADE` with `items`.
Provider replay stamps `provider_user_message_id` and, for Claude,
`provider_parent_uuid` onto the row when the wire echoes them. A
missing or drifted anchor is synthesized from the item's persisted meta
(`resolveMessageAnchor`), so pre-migration threads keep working.

## Rollback sequence

`rollbackConversationLocked` (`app_conversation_rollback.go`) is the
shared destructive tail behind the un-send and the edit-and-resend saga:
stop the provider session, roll provider history back, delete
`items`/`turns` from the selected turn onward, and — only when the
caller passed a `promptDraft` — restore the prompt into
`thread_drafts`.

That last step is caller-owned. A nil `promptDraft` means the caller
already put a durable copy in the draft row and settles it itself: the
un-send passes the rolled-back prompt so the composer rehydrates, while
edit-and-resend passes nil because there is nothing to rehydrate (the
replacement is being sent) and the row is holding its crash copy. The
active-turn rejection lives in the entry points, not here — the un-send
interrupts the live turn first, and edit-and-resend refuses outright.

Provider-side rollback differs by provider:

- **Codex** has a native `thread/rollback` wire method
  (`internal/provider/codex/session_rollback.go`); it uses the live
  session when one is active, else resumes a short-lived temp session
  just for the call. Rolling back to turn 0 — or to a prefix with no
  provider-backed turns — starts a fresh thread instead.
- **Claude** has no rollback RPC. `rollbackClaudeThreadToMessage`
  slices the current Claude JSONL through the end of the turn before
  the selected message using `internal/provider/claude/sessionfork`,
  then points `threads.session_ref` at the new session file. The slice
  boundary is resolved in trust order: the anchor's provider uuid when
  the transcript contains it, else the anchor's `turn_index`. Turn 0
  clears the Claude session entirely.

## Legacy checkpoint refs

Repos touched by older AO versions may still carry hidden
`refs/agent-overflow/*` snapshot refs. Nothing writes them anymore and
AO does not clean them up automatically — drain a repo manually with:

```sh
git for-each-ref --format='%(refname)' refs/agent-overflow/ | xargs -n1 git update-ref -d
```
