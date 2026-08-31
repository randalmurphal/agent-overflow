# internal/workflowapp/

The application layer for workflow reads, disposition, and follow-up coordination.

- Wails-bound methods and wire DTOs live on `internal/app.App`; the named root
  wrapper promotes them as `main.App` without changing method IDs.
- `Service` may depend directly on `store` and pure workflow packages. Runtime
  concerns that have not moved yet enter through capability-named `Deps`
  callbacks; it must never import or retain `*app.App`.
- It owns inspect, status/list/output, narrative, memory, long-poll watch,
  disposition/discard policy, the shared run-tree checkout model, PR follow-up,
  triage-thread store coordination, the complete workflow event/wake path, and
  the live engine/scheduler/autoresume application runtime.
- Engine event ordering is prepare durable digest → transport/replay emit →
  classify and enqueue reactions. The owned serial queue preserves transition
  order and is drained before the store closes. Queued wake claims are promoted
  only by the host send path's durable callback.
- workflowapp owns the engine and scheduler pointers, ordered reaction queues,
  transition ring, definitions watcher, autoresume timers, and digest-upgrade
  slots. SQLite remains the source of truth for durable engine state, schedules,
  current state, and causes.
- Provider-aware Runner construction remains in `internal/app` and the Runner itself
  remains `internal/workflowhost`. Provider-session sends, thread construction,
  transport emission, OS notification, text generation, git/forge operations,
  workspace cache invalidation, and settings persistence enter through narrow
  ports; workflowapp must not grow a catch-all Host interface.
