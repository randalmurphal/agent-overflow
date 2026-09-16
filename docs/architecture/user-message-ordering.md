# User message ordering

A message has three independent facts: its identity, its visible position,
and where the provider consumed it. The response can also belong to a different
logical turn. Do not infer one from another, a timestamp, or a client's last
observed activity state.

## Admission and identity

`internal/app/app_user_message_placement.go` owns App-side placement. Public
composer sends inspect backend activity under the thread action lock. Active
or starting work sends through the existing durable queue when the caller
negotiates send-ID reconciliation. Internal turn-opening
sends retain their explicit behavior, including workflow and edit/resend flows.
An unconsumed correlation marker is not proof of activity: a completed turn can
retain one for a late provider echo.

New turn allocation considers both cached items and known turns, plus pending
response turns. A known turn with no items still reserves its index. Queue
dispatch resolves placement again; a Codex rejected-steer fallback explicitly
allocates a fresh response turn.

The frontend creates one send ID before drawing its provisional row and uses
that same ID for every retry. Identified backend messages derive an opaque row
ID from it when the caller opts into `reconcileBySendId`. Older frontends
already send idempotency IDs but predict `user:<turn>` for their provisional
row: without this opt-in, direct sends retain numeric IDs. An idle-style send
from such a caller is rejected before acceptance if the backend is active or
starting, allowing its existing rollback to restore the draft. Explicit queue
and steer paths already consume backend-resolved item IDs and remain available.
Moving a modern message does not rename it; legacy numeric IDs remain valid.
Only row coordinates determine ordering. Queue, dispatched, and canonical-item
acknowledgements reconcile the originating frontend's provisional row by send
ID, including after a missed event. Another frontend's row is never a match.

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

A quiet row is persisted without a `provider:item_event`, so a connected
client keeps showing the message above its composer until the echo emits one.
That marker is live state, not an inference: `LiveStateSnapshotForThread`
publishes every unconsumed queued send as a pending flush item, and each
pending send appears in exactly one of the snapshot's two lists: the
composer marker or the deferred timeline rows a SQLite slice is blind to.
A client renders a flushed message in exactly one place at a time and hands
it from the marker to the timeline when the row actually renders
(`docs/architecture/turn-lifecycle.md` § Per-thread send queue). A window
read is the one place that shows a quiet row before its echo: the reserved
row is in SQLite, so a thread switch or gap refresh loads it and the marker
hands over early. Rows anchored at an interrupt are revealed deliberately and
carry no marker at all.

The first matched echo captures placement before fallible cache writes. A stable
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
history; paging must retain coverage of that gap. Late page replies also preserve
boundary corrections received while their request was in flight.

Interrupt display order and provider consumption order can differ: output already
shown after an interrupted message must stay there even if it preceded that
message's consumption. The separate promotion boundary preserves the provider
prefix used by fork/revert. Buffered output that had not yet been displayed can
instead drain before the confirmed message group. See [revert-modes.md](revert-modes.md).

An echo proves consumption even if SQLite fails. Advance the provider lifecycle
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

`threadOptimisticSend.test.ts` checks acknowledgement permutations and cross-client
isolation. `compact-stale-send.spec.ts` drives the real paired phone composer while
withholding activity events; the resulting prompt must appear once in the running
Codex turn and remain before its answer after reload. The existing send-recovery
browser cases drop successful RPC replies to exercise reconnect retries.
