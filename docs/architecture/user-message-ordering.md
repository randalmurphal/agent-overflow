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

This does not promise exactly-once delivery across a host crash between the
provider write and history persistence. Provider transcripts remain crash
recovery's authority; an unconfirmed delivery must not be blindly resent.

## Confirmation and placement

| Path | Visible placement | Response turn |
|---|---|---|
| Direct send | Persisted before provider write; echo attaches identity | Newly allocated turn |
| Explicit Codex steer | Persisted in the active turn; echo attaches identity | Active turn |
| Queued Codex steer | Deferred until matching provider echo | Active turn |
| Queued Claude input during activity | Quiet row in the current display turn, revealed on consumption | Fresh logical turn |
| Queued input without activity | Deferred until matching provider echo | Fresh logical turn |
| Interrupt promotion | Reveal pending input at the interrupt boundary | Consumption still follows the provider echo |

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
