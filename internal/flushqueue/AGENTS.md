# internal/flushqueue/

Pure projectors backing the per-thread flush queue: the wire-side
`QueuedItem` shape the frontend mirrors, its inner `Payload` JSON, the
`triage.QueuedFlushItem → QueuedItem` projection, the same projection
from a DURABLE row (`store.FlushQueueItem → QueuedItem`), and the
`queue:<uuid>` id allocator.

## The queue is durable, and this package projects both copies

A queued message has two representations and one meaning. `triage` holds
the live one in router memory; `flush_queue_items` (migration v85) holds
the one that survives the process, because the composer cleared the
moment the user pressed Send and a crash in between would otherwise lose
a message with no trace anywhere. `ItemFromTriage` and `ItemFromStore`
project each onto the same wire `QueuedItem` through one shared body, so
a repeated send answered from the durable row and a fresh one answered
from memory are indistinguishable to the client — which is the whole
point, since those are the two answers one retried frame can get.

Nothing here writes or reads SQLite. Row lifetime is
`app_flush_queue.go`'s (registered → deleted at a durable endpoint) and
the boot sweep is `app_flush_queue_restore.go`'s.

## Responsibility boundary

- What BELONGS here:
  - Wire-shape declarations for the queue projection.
  - Pure decode-projection into the rendered wire `QueuedItem`, from
    `triage.QueuedFlushItem` (which keeps `Payload` opaque to triage)
    and from `store.FlushQueueItem` alike.
  - The queue-item id allocator.
- What does NOT belong here:
  - The App-bound `RegisterQueueItem` / `GetQueueState` sagas. They stay in
    `app_flush_queue.go` where
    attachment / plan / triage coordination already lives.
  - Provider dispatch (`dispatchFlush` in `app_flush_queue.go`).

## Anti-patterns

- Do NOT inflate scope by absorbing the dispatcher. Keep this package
  to wire-shape + projection.
- Do NOT log-and-swallow inside the projector. A corrupt payload
  intentionally returns a partially-populated wire item (message text
  preserved, attachment refs nil) so the frontend can render the row.
  See the doc comment on `ItemFromTriage`. The boot sweep applies the
  same rule to the durable row: a payload that no longer decodes costs
  the attachment and plan references and never the message, which is the
  part a person typed and the reason the text has a column of its own.

## Anti-pattern carve-out: drift guard test

`app_flush_queue_test.go` keeps a compile-only
`guardCompileEnsureSendMessageOptionsCompatible` referencing the
`flushQueuePayload` alias `internal/app` keeps for
`flushqueue.Payload`. The alias is intentional. Removing it would
require either churning that drift guard or moving it across the
package boundary, and the local name is cheap.
