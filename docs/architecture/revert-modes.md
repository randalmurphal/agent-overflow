# Message Anchors & Conversation Rollback

Every real user message gets a `message_anchors` row written immediately
after its `items` row persists. The anchor carries the provider-side
identity of the message (Claude's wire uuid + parent uuid, and the AO
`turn_index` Codex anchors resolve against), so the three
message-boundary operations can slice provider history at that message:

- **Fork-from-message** (`app_thread_fork.go`): clone the thread up to
  a chosen user message into a new thread. The source thread is left
  untouched.
- **Revert-on-interrupt** (`app_revert_on_interrupt.go`): the Stop/Esc
  un-send. When exactly one user message is in flight with no assistant
  content yet, Stop rolls the message back (conversation only) and
  restores it into the composer draft instead of leaving a dangling
  turn.
- **Edit-and-resend** (`app_revert_and_resend.go`): stage the edited text in
  `thread_draft_recoveries`, roll back, prepare the provider, then publish the
  cut and persisted replacement together through `user_message:reverted`.
  Composer autosaves remain independent. The thread action lock spans the cut
  and send; a client send ID prevents duplicate replacement dispatch.

Early Stop immediately restores the raw submitted draft and hides the outgoing
turn. Background-task checks and provider cleanup run behind a Send gate while
editing remains available. Draft autosaves wait behind restoration so switching
panes cannot let the backend overwrite newer typing. A background guard or a
raced assistant response declines the rollback and retains ordinary interrupt
behavior.

Older-message replacement keeps the editor loading until preparation completes.
The client transfers follow intent before applying the cut and replacement in
one render transaction, using the normal structural scroll spring. RPC results
carry the same cut as the event and distinguish a refusal from a committed cut
whose send failed. Channel sequence boundaries fence delayed pre-cut item and
turn events; history revisions deduplicate cut delivery. After a lost response,
a locked read confirms the outcome before Send unlocks.

Recovery staging is removed after acceptance. On failure it merges atomically
into the composer draft. Startup recovers unfinished edits without dispatching
them and retires copies whose send ID is already accepted. Outstanding recovery
prevents empty-thread cleanup, transfer export and snapshot rewind.

All three are conversation-level operations. There is no working-tree
revert: the per-message git-checkpoint machinery (hidden
`refs/agent-overflow/*` snapshot refs, `thread_tracked_files`,
revert-to-message with file restore) was removed. It flooded repo
tooling with hundreds of hidden refs and the file-restore path was
never used. Agents revert their own edits when asked.

## Anchor storage

Visible placement and provider consumption can differ after interrupt. Their
separate boundaries and retry rules are defined in
[user-message-ordering.md](user-message-ordering.md); neither boundary is inferred
from the user row's ID.

SQLite only (`internal/store/message_anchors.go`). Primary key
`(thread_id, user_item_id)`, `ON DELETE CASCADE` with `items`.
Provider replay stamps `provider_user_message_id` and, for Claude,
`provider_parent_uuid` onto the row when the wire echoes them. A
missing or drifted anchor is synthesized from the item's persisted meta
(`resolveMessageAnchor`), so pre-migration threads keep working.

## Rollback sequence

`rollbackConversationLocked` (`app_conversation_rollback.go`) owns the shared
provider rollback and cache truncation. Early un-send also restores a prompt
draft; edit/resend owns separate durable recovery. Background-task guards remain
in the entry points. Early Stop declines rollback while tasks run; older-message
replacement requires explicit consent to stop them.

Provider-side rollback differs by provider:

- **Codex** prefers native `thread/revert` for supported paginated sessions,
  retaining the provider thread identity. Older app servers use `thread/fork`.
  Both cut at turn boundaries. An empty provider prefix starts a fresh thread.
  Native revert owns active-turn shutdown; it needs no preceding interrupt RPC.
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
AO does not clean them up automatically. Drain a repo manually with:

```sh
git for-each-ref --format='%(refname)' refs/agent-overflow/ | xargs -n1 git update-ref -d
```
