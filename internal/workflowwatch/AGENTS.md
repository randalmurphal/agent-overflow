# internal/workflowwatch/

The bounded, process-local transition ring behind workflow run long polls.

- `Hub` has a usable zero value and performs no I/O in `Record`, because the
  workflow engine calls it on its command-loop event path.
- Cursor zero always means “no cursor”; the first real sequence must never be
  zero. Cursors outside either end report a gap.
- The ring is a jitter buffer, not history. SQLite remains the source of truth
  for the current run state and persisted park cause.
- This package owns no wire model and emits no event. `internal/app` projects
  `Transition` into the Wails response.
