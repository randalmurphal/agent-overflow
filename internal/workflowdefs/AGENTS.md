# Workflow definition watcher

`Watcher` invalidates the workflow definition catalog when definition files
change.

- Watch only shared `workflows/` and project `projects/<slug>/workflows/`
  trees. A root watch may discover new definition trees but must ignore
  unrelated data-root changes.
- Use trailing-edge debounce so a logical edit produces one invalidation.
- Keep event projection in `internal/app`; this package accepts a callback and
  owns no wire model.
- `Close` remains idempotent and waits for the watcher goroutine to exit.
