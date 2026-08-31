# internal/flushqueue/

Pure projectors backing the per-thread flush queue: the wire-side
`QueuedItem` shape the frontend mirrors, its inner `Payload` JSON, the
`triage.QueuedFlushItem → QueuedItem` projection, and the
`queue:<uuid>` id allocator.

## Responsibility boundary

- What BELONGS here:
  - Wire-shape declarations for the queue projection.
  - Pure decode-projection from `triage.QueuedFlushItem` (which keeps
    `Payload` opaque to triage) into the rendered wire `QueuedItem`.
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
  See the doc comment on `ItemFromTriage`.

## Anti-pattern carve-out: drift guard test

`app_flush_queue_test.go` keeps a compile-only
`guardCompileEnsureSendMessageOptionsCompatible` referencing the
`flushQueuePayload` alias `internal/app` keeps for
`flushqueue.Payload`. The alias is intentional. Removing it would
require either churning that drift guard or moving it across the
package boundary, and the local name is cheap.
