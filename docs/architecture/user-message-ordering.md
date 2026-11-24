# User message ordering

A message has three independent facts: its identity, its visible position,
and where the provider consumed it. The response can also belong to a different
logical turn. Do not infer one from another, a timestamp, or a client's last
observed activity state.

## Admission and identity

`internal/app/app_user_message_placement.go` owns App-side placement. Public
composer sends inspect backend activity under the thread action lock. Active
or starting work sends through the existing durable queue. The harness's
awaited send (`HarnessSend` with `atRestOnly`) is refused there instead,
recording nothing, so the turn an accepted one opens is known. Internal
turn-opening sends retain their explicit behavior, including workflow and
edit/resend flows.
An unconsumed correlation marker is not proof of activity: a completed turn can
retain one for a late provider echo.

New turn allocation considers both cached items and known turns, plus pending
response turns. A known turn with no items still reserves its index. Queue
dispatch resolves placement again; a Codex rejected-steer fallback explicitly
allocates a fresh response turn.

The frontend creates one send ID per message and uses that same ID for every
retry. It draws no row of its own for the message. Identified backend messages
derive an opaque row ID from the send ID, and a client send says it finds its
row that way with `reconcileBySendId`. A window older than v0.0.15 does not:
it draws a provisional row and predicts `user:<turn>` for it. Its send is
refused before anything else with `dispatchrefusal.Unreadable`, a repeat of a
send it made before an upgrade included, and its existing rollback restores
the draft. Internal sends without a send ID keep numeric IDs. Moving a
message does not rename it; numeric IDs in stored history remain valid. Only
row coordinates determine ordering.

A direct send is on screen only as the Send button's pending state until the
backend shows evidence of it. The originating pane holds the send ID
(`awaitSendConfirmation` in `thread.svelte.ts`) and releases it on the first
of: the pushed `user_text` row carrying that send ID, whether or not the
loaded window admits the row; that row installed in the window by a history
read, which covers a lost row frame; a `provider:queue_state_changed` or
`provider:queue_flushed` entry naming it; a live-state read listing it, which
covers a missed queue event; or the send's reply, accepted or refused. A
thread switch abandons the wait. Another frontend's send and another thread's
events are never a match.

Same-ID public admissions serialize before their thread/action locks. The
existing history row, durable queue row, or pending provider-echo entry answers
duplicates; there is no separate receipt database. Lookup holds the echo anchor
across those homes so echo pop/persist cannot create an acceptance gap.

Frontend process death does not replay unacknowledged sends: accepted messages
hydrate from their host-owned history/queue/live state. Draft consumption also
belongs to the accepting backend operation. The frontend clears only its local
composer and sends its captured raw `consumeDraft` snapshot; acceptance deletes
only a still-matching persisted draft. New edits on any frontend survive, and
queue dispatch never consumes the draft again. Dirty pending edits are saved
before admission through the per-thread draft writer; ordinary autosaves
coalesce, but cannot overtake a captured send preparation. Failure to prepare
restores the draft without issuing a send. Clean hydrated drafts stay write-free.
Matching uses the persisted
fields, consistent with identical autosaves being no-ops. Generated prompts
send an empty snapshot so they preserve unrelated composer content.

The snapshot is additive: old clients without it retain legacy consumption,
and old hosts ignore it. Both ends must be updated for matching-draft protection.
Losing the client before acceptance must not erase an already saved draft.

This does not promise exactly-once delivery across a host crash between the
provider write and history persistence. Provider transcripts remain crash
recovery's authority; an unconfirmed delivery must not be blindly resent.

App-injected agent sends carry their own identity. A message one thread's
agent sends to another is admitted through the same door as a composer send,
with a SendID naming the request it belongs to: `thread-request:<token>` for
the request itself, `thread-wake:<token>` and `thread-wake-late:<token>` for
the answer carried back to the sender. Retrying a request resolves to the
existing row through that id rather than sending a second copy. These sends
preserve the thread's draft and stamp `usermessage.Meta.Origin` =
`agent-thread` with the sending thread's identity, on the immediate row and on
the queued payload alike, so a message the app wrote on an agent's behalf is
never mistaken for one a person typed. One wake can answer several requests;
it is admitted under the oldest request's id and attributed to that request's
thread, and when it speaks for several threads its text names them all.

## Confirmation and placement

| Path | Visible placement | Response turn |
|---|---|---|
| Direct send | Persisted before provider write; echo attaches identity | Newly allocated turn |
| Explicit Codex steer | Persisted in the active turn; echo attaches identity | Active turn |
| Queued Codex steer | Deferred until matching provider echo | Active turn |
| Queued Claude input during activity | Quiet row reserved in the current display turn, revealed on consumption | Fresh logical turn |
| Queued input without activity | Deferred until matching provider echo | Fresh logical turn |
| Interrupt promotion | Reveal pending input at the interrupt boundary | Consumption still follows the provider echo |

A drain that hands the dispatcher several queued messages at once for a
headless Claude session delivers them as ONE provider message: one stdin
envelope under one uuid, one row whose summary joins the parts with a visible
rule, one message anchor, one response turn, and every member send ID recorded
on that row so any member's retry resolves to it. This records what the CLI
does: it merges a multi-message boundary drain into a single transcript entry
carrying only the last uuid, so a row per queued message would name provider ids
the transcript never contains and revert could not slice at them. A resolution
failure on any member sends nothing and requeues the whole group in order.
Codex and claude-tui keep one message per queued item. See
`internal/app/app_flush_dispatch_join.go` and
[claude-wire.md](../references/claude-wire.md).

A queued message's durable row is what says it is still wanted. Withdrawing
it deletes the row; the dispatch reads its group's rows under the thread's
mutation lock right before writing the group (`commitFlushGroup`), drops the
messages whose rows are gone, and marks the rest as being written. A
withdrawal under the same lock either removes the message before that read or
finds it dispatched, never both. The row stays until the provider write
settles, so a crash at any point before it leaves the message to the boot
sweep. A failed write's requeue and a session end's restore keep only
messages whose rows stand. A message is rowless (`QueuedFlushItem.NoRow`),
and passes those checks, only when its write is known to have retired its
row and the row is not written again, or it had none: the write records
that on its pending send for each queued message it carried
(`FlushQueueMember`, `NoteFlushRowRetired`), and an interrupted Codex
message's resend or a requeued send that still has its timeline row
carries it. An unconfirmed send a failed restore requeues gets its row
again (`requeueUnconfirmedFlushItemsLocked`). Any other message whose row
is gone was withdrawn, and is dropped; an answer keeps its row until its
echo. A joined message decides for each message it carries: a withdrawn
one is cut out of its text and the rest kept, and a requeue puts each one
that still has its row back as its own queue item, the rest as one, so a
later withdrawal takes it alone; the dispatch joins them again. One whose
parts do not describe its text, or whose row does not decode, cannot be cut
where either has to: it goes to the composer whole, as any join that does
not split, and is not queued or sent again (`withoutQueueMembers`,
`writeFlushDraft`); a failed composer write is a thread error. A dropped
copy takes with it the timeline row an earlier dispatch left
(`QueuedFlushItem.StaleUserItemID`). Every handoff carries the dispatch generation it was made in
(`QueuedFlushItem.DispatchGeneration`): one a recovery overtook goes where
that recovery took the rest of the queue, unless a Stop's teardown or a
revert dropped its generation by the time that restore holds the mutation
lock the drops take, and then it is dropped too, rowless messages included
(`FlushGenerationDropped`). Neither goes to a replacement session. A
claimed thread's dispatch waits for the delete's decision holding its batch.

A quiet row is persisted without a `provider:item_event`, so a connected
client keeps showing the message above its composer until the echo emits one.
That marker is live state, not an inference: `LiveStateSnapshotForThread`
lists every unconsumed queued send, deferred or quiet, in `FlushedItems`; a
send anchored at an interrupt is already a timeline row and is not listed.
`GetThreadLiveState` lists a message in exactly one place: in `QueueItems`
while the queue or the dispatch worker still holds it, in `FlushedItems`
once its dispatch hands it over.
A client renders a flushed message in exactly one place at a time and hands
it from the marker to the timeline when the row actually renders
(`docs/architecture/turn-lifecycle.md` § Per-thread send queue). Quiet rows
carry `pendingFlush: true`; loading one from SQLite does not hand its live
preview over to the timeline. Confirmation and interrupt promotion remove the
key atomically with placement, so a settled row carries no trace of it and
matches the same row imported from a session file. The live pending-send
registry owns the preview, so a retained row after session loss still follows
normal history recovery.
Rows anchored at an interrupt are revealed deliberately and carry no preview.

## Confirmation

A pending send is consumed by the first provider fact that names its identity.
Every source runs the same transaction (`confirmConsumedSend`):

- Claude's transcript record: the main-transcript `transcript_mirror` entry
  written under the uuid AO issued. It confirms a prompt that opens a turn,
  before the turn's first output, whatever the prompt says. The CLI withholds
  the echo of such a prompt when its text names a filtered tag
  ([claude-wire.md](../references/claude-wire.md#outbound-user-message--client-supplied-uuid---replay-user-messages)).
- The provider echo. It confirms a Claude prompt drained into a running turn,
  which has no transcript user entry, and every Codex send. An echo of a send
  its record already confirmed is a duplicate.
- Claude's `command_lifecycle` `started` followed by a terminal state, for a
  send both of the above missed. The row lands at the head of a turn the send
  opened; the backstop logs, because it means a confirmation drifted.

A send Claude retires without `started` never reached the model. It leaves
the registry for the thread's undelivered list, and its row is deleted when its
text returns to the composer. After `cancelled` the session lives on, so it
returns at once (`queue_restored`, reason `not_delivered`). After `discarded`
the session is ending: it returns with the rest of the queue in the
session-end restore, in queue order, or when a replacement session starts, or
is dropped with the queue by a Stop.
Otherwise a send stays pending until its session ends, and no longer: a row
carries `pendingFlush` only while its send is in the live registry. Every
teardown takes the registry's unconfirmed sends before the triage cleanup
sweeps it (`settleQueueForTeardown`): a death, a removed worktree or a
reconnect returns them to the composer, and a Stop drops them with the queue,
deleting their rows (`queue_restored`, reason `dropped`). A send the provider
did consume is confirmed instead. Only the person's text ever returns to the
composer: what the app wrote in a send (an answer, a reminder, a completion, a
wake, an outside send) goes back to its owner or is recorded as not delivered
(`agentPartsNotTakenIn`), and a joined send gives up those members by the
parts its dispatch recorded (`agentmeta.JoinedPart`).
A crash runs no teardown: the boot restore (`restoreDurableFlushQueueAtBoot`)
takes back each main-timeline user row still marked `pendingFlush` that is
residue, in its thread's last turn and written by that thread, retires it and
returns the person's text in it to the composer (`unconfirmedRowAtBoot`). A
joined row that carries the app's messages splits by its recorded parts, and
one whose parts no longer describe its text is restored whole. A queue row
that survived beside it as a copy of a message it carries adds no text: the
row is the unit of the person's text. A queue row whose message a row the
provider confirmed carries was delivered and is dropped. A dispatch deletes the queue
rows of every message it carried in one statement. A marked row a
later turn follows is history a later session ran with: the boot leaves it
and the composer alone, and the store refuses to retire it
(`RestoreFlushQueueToDraft`). Migration v152 cleared the marks earlier builds
left on such rows. The draft, the queue rows and the retired rows are written
in one transaction, so a crash cannot repeat or lose the text.

The first matched confirmation captures placement before fallible cache writes. A stable
predecessor identifies that boundary; retries must never ask for the current tail.
Otherwise output received after the failed write moves the retried prompt below
its own response. Unconfirmed rows that may still move cannot act as predecessors.

Store placement applies an ordered user group in one transaction. When later
output already occupies its slots, it shifts the suffix and rebases any numeric
consumption boundaries together. Imported history is localized before mutation.
All changed rows are emitted from the committed result through the existing item
stream. Session-death cache repair uses the same saved
confirmation decision as ordinary echo handling.

The frontend reconciles page boundaries when rows move, before admitting new
rows from the same batch. A moved outlier cannot extend a page across unknown
history; paging must retain coverage of that gap. An edge with no unloaded
rows beyond it follows a moved row, so a confirmed queued message the page
already held is admitted at its new position in the same commit, without
waiting for a later append. Late page replies also preserve boundary
corrections received while their request was in flight.

Interrupt display order and provider consumption order can differ: output already
shown after an interrupted message must stay there even if it preceded that
message's consumption. The separate promotion boundary preserves the provider
prefix used by fork/revert. Buffered output that had not yet been displayed can
instead drain before the confirmed message group. See [revert-modes.md](revert-modes.md).

A confirmation proves consumption even if SQLite fails. Advance the provider lifecycle
before processing its next content event, retain the confirmation for cache repair,
and never put consumed input back on the provider send queue.

## Verification

App placement tests cover both providers, active/start/completed states, rowless
turns, stable identities, rejected-steer fallback, and duplicate admission across
RPC methods. Triage tests preserve strict echo correlation, interrupt/settlement
ordering and failure recovery. Store tests inject transaction failures and later
response rows, including imported overlays and fork/revert boundaries.
An exhaustive store test covers all relative orders of two messages and their
surrounding content, including negative positions, gaps, failed commits and retries.

`threadSendConfirmation.test.ts` checks each confirmation source and cross-client
and cross-thread isolation. `compact-stale-send.spec.ts` drives the real paired phone composer while
withholding activity events; the resulting prompt must appear once in the running
Codex turn and remain before its answer after reload. The existing send-recovery
browser cases drop successful RPC replies to exercise reconnect retries.
