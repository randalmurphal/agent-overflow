# internal/worktreesetupapp/

Application service for asynchronous chat-worktree setup runs.

## Ownership

- `Service` owns the complete run registry, its mutex, the stopped gate, and
  the WaitGroup that joins execution before SQLite closes.
- `internal/worktreesetup` remains the blocking, store-free execution engine.
  Workflow provisioning calls that lower-level engine directly.
- `internal/app` retains the exact Wails methods, comments, and JSON DTOs. It
  supplies narrow store, event, lifetime-context, and shutdown-error ports.
- Every run belongs to an already-persisted thread. There is no draft-workspace
  RPC, pre-thread run, or adoption path: thread creation launches setup only
  after it has cut and persisted a new worktree.

## Ordering

- `Config.BeginWork` acquires the host restart lease before reserving a run.
  Transfer it to the execution goroutine and release after command cleanup and
  terminal publication. Failed starts and recipes with no steps release too.
  Canceling/observing an existing run needs no new admission.

- The stopped check, registry reservation, and WaitGroup `Add` share one
  critical section. `Stop` sets stopped under that mutex before `Wait`.
- Settlement publishes terminal state under the same mutex used by snapshot,
  retry, release, and cancellation reads. Durable stamps also occur under that
  ward.
- Shutdown cancels and joins all runs before the store closes. Interrupted
  bound rows stay `running`; the next boot sweep marks them failed.
- Output sequence is read before the tail snapshot, so a racing chunk is
  ignored as already folded rather than dropped.

## Guardrails

- Do not add provider, session, or workflow-runtime behavior here.
- Do not split any run field or lifecycle mutex back onto `App`.
- Snapshot and retry RPCs resolve the persisted thread and its current
  worktree; a run is never selected by a caller-supplied workspace path.
- `CancelPath` exists for destructive workspace/project cleanup and must cancel
  every run whose canonical worktree matches the path before removal begins.
- A setup failure never rolls back a chat worktree. Retain it for retry.
