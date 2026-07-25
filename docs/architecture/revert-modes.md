# Message Anchors & Conversation Rollback

Every real user message gets a `message_anchors` row written immediately
after its `items` row persists. The anchor carries the provider-side
identity of the message — Claude's wire uuid + parent uuid, and the AO
`turn_index` Codex anchors resolve against — so the two surviving
message-boundary operations can slice provider history at that message:

- **Fork-from-message** (`app_thread_fork.go`) — clone the thread up to
  a chosen user message into a new thread.
- **Revert-on-interrupt** (`app_revert_on_interrupt.go`) — the Stop/Esc
  un-send: when exactly one user message is in flight with no assistant
  content yet, Stop rolls the message back (conversation only) and
  restores it into the composer draft instead of leaving a dangling
  turn.

Both are conversation-level operations. There is no working-tree
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
shared saga behind both entry points: reject active turns, stop the
provider session, roll provider history back, delete `items`/`turns`
from the selected turn onward, and restore the prompt into
`thread_drafts`.

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
