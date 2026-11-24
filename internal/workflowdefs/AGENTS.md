# Workflow definition watcher

`Watcher` invalidates the workflow definition catalog when definition files
change.

- Watch only shared `workflows/` and project `projects/<slug>/workflows/`
  trees. A root watch may discover new definition trees but must ignore
  unrelated data-root changes.
- The debounce, the emit and `Close` are `internal/fswatch`'s; this package
  decides which events change a definition tree and keeps the watch set in
  step with the trees.
- Keep event projection in `internal/app`; this package accepts a callback and
  owns no wire model.
