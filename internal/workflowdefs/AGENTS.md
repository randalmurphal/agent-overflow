# internal/workflowdefs/

Live invalidation for the workflow definition catalog.

- `Watcher` recursively watches only shared `workflows/` and
  `projects/<slug>/workflows/` trees beneath its configured root.
- The root watch discovers newly created definition trees; unrelated database
  and settings churn at that root must never emit an invalidation.
- Definition changes are trailing-edge debounced. `internal/app` owns the typed frontend
  event and passes its emitter callback into this package.
- `Close` is idempotent and waits for the watcher goroutine to stop.
