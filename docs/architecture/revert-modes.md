# Message Anchors & Conversation Rollback

Every real user message gets a `message_anchors` row written immediately
after its `items` row persists. The anchor carries the provider-side
identity of the message (Claude's wire uuid + parent uuid, and the AO
`turn_index` Codex anchors resolve against), so the two
message-boundary operations can slice provider history at that message:

- **Fork-from-message** (`app_thread_fork.go`): fork the thread at a
  chosen user message into a new thread that reads the source's history
  before it ([pointer forks](sqlite-store.md#pointer-forks)). The source
  thread is left untouched.
- **Edit-and-resend** (`app_revert_and_resend.go`): stage the edited text in
  `thread_draft_recoveries`, roll back, prepare the provider, then publish the
  cut and persisted replacement together through `user_message:reverted`.
  Composer autosaves remain independent. The thread action lock spans the cut
  and send; a client send ID prevents duplicate replacement dispatch. It
  refuses while a turn runs or a send awaits provider confirmation.

Stop is always a plain interrupt: the message stays in the transcript and the
turn ends interrupted.

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

Both are conversation-level operations. There is no working-tree
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

`rollbackConversationLocked` (`app_conversation_rollback.go`) owns the
provider rollback and cache truncation for edit-and-resend, which owns its
durable recovery and the composer draft. It supports Claude and Codex and
refuses any other provider before it stops or cuts anything; claude-tui keeps
its conversation inside the TUI. Edit-and-resend requires explicit consent to
stop running background tasks. Its guard counts every live background launch
the session stop kills, nested launches under an ended agent included
(`CountLiveRunningBackgroundToolCalls`).

A cut can delete the completion row of a background launch that stays in the
kept history. The launch stays settled with no result card, and nothing writes
it another completion
([background settlement](sqlite-store.md#schema-owned-invariants)).

Provider-side rollback differs by provider:

- **Codex** prefers native `thread/revert` for supported paginated sessions,
  retaining the provider thread identity. Older app servers use `thread/fork`.
  Both cut at turn boundaries. An empty provider prefix starts a fresh thread.
- **Claude** has no rollback RPC. `rollbackClaudeThreadToMessage`
  slices the current Claude JSONL through the end of the turn before
  the selected message using `internal/provider/claude/sessionfork`,
  then points `threads.session_ref` at the new session file. The slice
  keeps every surviving entry's uuid, so the provider ids AO stored stay
  valid in it. The slice boundary is resolved in trust order: the
  anchor's provider uuid when the transcript contains it, then the
  anchor's parent uuid (a retry after the slice already committed), else
  the anchor's `turn_index`. Turn 0 clears the Claude session entirely.
  A row that HAS a stamped provider uuid the transcript does not contain
  does not fall through to the turn index: the ordinal walk has no proof
  its row count still matches the transcript's prompt count and could cut
  a turn too far back, so the rollback fails naming the known cause, a
  Claude queue merge AO could not fold. The one exception is a
  transcript that ends before the anchor's turn, which is a session that
  died before persisting the prompt and is cloned whole.

  A lazy fork that has not started a session of its own
  (`pending_fork_session_ref` set, `session_ref` empty) resumes its
  source at the pinned cut, which holds none of the messages the fork
  sent. Rolling back to such a message, or forking from it, keeps that
  pending resume state and slices nothing, even when the CLI wrote the
  message to a forked session whose `system/init` arrived after the stop.
  A message the fork inherited still slices the source. The rollback
  reads the thread's resume state after the session stops, because that
  `init` can commit the fork's own `session_ref` during the stop.

  Every queued batch is one row with one uuid, so it slices exactly: a
  batch AO dispatched as one joined message is joined at dispatch
  (`app_flush_dispatch_join.go`), and one the CLI merged across separate
  AO drains is folded into the surviving row when its echo arrives
  (`internal/triage/claude_merge_fold.go`).

## Legacy checkpoint refs

Repos touched by older AO versions may still carry hidden
`refs/agent-overflow/*` snapshot refs. Nothing writes them anymore and
AO does not clean them up automatically. Drain a repo manually with:

```sh
git for-each-ref --format='%(refname)' refs/agent-overflow/ | xargs -n1 git update-ref -d
```
