# internal/closer/

Close-orchestration helpers reused by the App's shutdown and
fork-cleanup paths. Two shapes:

- A LIFO `Stack` of cleanup callbacks for "did some work; on failure
  undo it in reverse order" flows (used by the fork-and-revert saga in
  `app_thread_fork.go`).
- A bounded `RunParallel` runner for "close N things with one
  wall-clock timeout, collect every failure" flows (used by the
  shutdown path that closes every provider session in parallel).

Depends on `internal/errorsx` for the nil-filtering append and the
action-prefixed wrapping.

## Surface

| Symbol | Purpose |
|---|---|
| `Task{Label, Close}` | Single labelled close operation. `Label` is the human-readable identifier propagated into the error messages. |
| `RunParallel(tasks, timeout) []error` | Fire every Task in its own goroutine, collect non-nil errors, abandon stragglers past `timeout` with a labelled deadline error. |
| `Stack []func() error` | LIFO cleanup list. `Add` ignores nil cleanups. `Run` invokes in reverse order and joins errors via `errors.Join`. |

## Responsibility boundary

- What BELONGS here: small Close-orchestration primitives that don't
  depend on App state or wire shapes.
- What does NOT belong here: the App-specific glue that builds
  `Task`s out of a `session` or a `store.Thread`. That stays in the
  `internal/app` package (e.g. `sessionThreadCloser` in `app_emit.go`,
  `cleanups.Add(...)` in `app_thread_fork.go`).

## Anti-patterns

- Do NOT import non-stdlib packages other than `internal/errorsx`.
  The no-cycle guarantee is the reason this helper lives separately
  from `internal/app`.
- Do NOT change the timeout semantics to fire mid-loop. Callers depend
  on "everything in or one global deadline". Partial early returns
  would surface tasks the caller hasn't seen yet.
