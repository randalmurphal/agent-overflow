# `internal/flushqueue`

Shared wire shapes and pure projectors for queued sends. A queued message has a
live `triage.QueuedFlushItem` representation and a durable
`store.FlushQueueItem`; both project to the same `QueuedItem`.

- Keep `Payload` aligned with the queued subset of send options. Runtime mode is
  intentionally absent because dispatch uses the active round's mode.
- Preserve queue id, send id, message, attachment ids, plan and review sources,
  and enqueue timestamp across both projectors.
- A malformed opaque payload logs the decode error and returns the typed message
  row without optional references. Do not drop the person's message.
- `NewItemID` returns the stable `queue:<uuid>` format.

Queue execution and persistence live in `internal/app` and `internal/store`. See [turn lifecycle](../../docs/architecture/turn-lifecycle.md) for promotion and turn boundaries.
