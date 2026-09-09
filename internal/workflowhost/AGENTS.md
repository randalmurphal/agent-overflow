# Workflow execution host

This package implements `engine.Runner`: it turns one phase, fan-out unit, or
join into a provider turn or supervised tool command and reports one outcome to
the engine. The engine decides what runs next.

## Boundary

- Reach application capabilities only through the consumer-side interfaces in
  `host.go`. `workflowHostAdapter` in `internal/app` is the sole adapter and
  remains forwarding glue.
- Add a required application operation to the narrow owning interface. Do not
  import or retain `*app.App` here.
- The store is an explicit runner dependency, not a host capability.
- Keep bound methods, scope annotations, routes, DTO conversion, and wire events
  in `internal/app`.

## Attempt lifecycle

- `Runner.Start` installs exactly one tracked attempt and eventually reports
  exactly one terminal or parked outcome. `Stop` and stale callbacks must not
  produce a second outcome.
- Route every provider send through `sendIfActive`; its identity and epoch checks
  are the protection against late sends after stop, restart, or takeover.
- Bound provider start, send shutdown, inactivity, and retry waits. Cancellation
  must reach provider and tool subprocesses and their process groups.
- Treat typed provider usage limits as parks rather than transient retries.
- A continuation may reuse a provider session only after proving the expected
  provider context still exists and belongs to the phase thread.
- Human takeover detaches automation without discarding the reliability
  deadline and restores the correct schema when control returns.

## Workspaces, units, and artifacts

- Provision and adopt worktrees through the shared workspace helpers. Preserve
  branch naming and ownership across item, unit, call, and recovery paths.
- Fan-out units receive isolated sub-worktrees when required. Retire them only
  after their outcome is durable.
- Tool processes use the same supervision and envelope-validation path as other
  workflow tool execution.
- Settle an accepted attempt's narrative from the documented ordered sources;
  do not replace missing narrative with envelope text.
- Capture artifacts beneath the run-owned artifact root. Listing and opening
  must reject traversal and paths outside that root.

## Tests

- Use `fakeHost` capabilities to state only the dependencies a test exercises.
- Keep git behavior real where the contract depends on git answers.
- Preserve the real observer bus default for subscription-lifetime tests.
- Isolate provider homes and process spawning through `kerneltest`; tests must
  never inspect the developer's provider state or launch a real provider.
- App-level bound-method and transport behavior stays in `internal/app` tests.

See `docs/specs/workflows-system.md` and `internal/workflow/engine`.
