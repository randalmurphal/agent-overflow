# Application composition

The executable root contains process entry points and a small compatibility
wrapper. The importable application shell lives in `internal/app`, and focused
services beneath `internal/` own their state and behavior.

```text
main.App
  embeds internal/app.App
    composes responsibility-owned services
      depend on domain, store, provider, and platform packages
```

## Stable service boundary

Root `service.go` declares a named `main.App` that embeds
`*internal/app.App`. Go reflection includes the promoted method set, while
Wails and the custom transport identify each method as `main.App.<Method>`.
The custom dispatcher pins the same `Package: "main"` and
`TypeName: "App"` labels.

Method IDs are the FNV-1a-32 hash of the fully qualified service method name.
Moving a method body into `internal/app` preserves its ID while the named
wrapper, registration labels, and method name remain unchanged. Changing one
is a wire migration. `//wails:id` can pin an ID for an approved rename;
`//wails:ignore` keeps a method off the wire.

Generated App DTOs live in
`frontend/bindings/agent-overflow/internal/app/models.ts`. Update generated
files through the Wails generator.

## Ownership

`internal/app.App` owns:

- stable Wails method facades and wire DTOs;
- construction, startup, shutdown, and executable-to-service bootstrap inputs;
- typed adapters between responsibility packages;
- transactions whose correctness depends on order across multiple owners;
- harness integration and application-level integration tests.

Responsibility packages own their complete process-local state, including
locks, caches, timers, goroutines, stop gates, and focused tests. They receive
capability-named dependencies instead of `*App`. A broad `Host` interface
that recreates App's surface moves files without moving authority.

| Area | Owner |
|---|---|
| live provider sessions | `internal/sessionruntime` |
| managed provider accounts | `internal/provideraccountapp` |
| provider discovery and quota lifecycle | `internal/providerdiscoveryapp`, `internal/providerlifecycleapp` |
| thread persistence and action locks | `internal/threadapp` |
| git status/fetch and worktree queries | `internal/gitapp`, `internal/worktreeapp` |
| chat-worktree setup runs | `internal/worktreesetupapp` |
| workflow application runtime | `internal/workflowapp` |
| workflow provider execution | `internal/workflowhost` |
| MCP coordination | `internal/mcpapp` |

Consult the nearest package guide before changing an owner boundary.

## Residual application transactions

Some operations remain in `internal/app` because their visible order spans
several owners:

- provider-event processing across triage, live configuration, observers,
  accounting, queue recovery, workflows, and session registration;
- send, durable flush, revert, and edit/resend across drafts, attachments,
  triage, provider sessions, and thread locks;
- cross-provider fork across provider-native history, attachments, anchors,
  persistence, and rollback;
- project/worktree deletion and workspace switching across workflow/session
  cancellation, setup runs, git/filesystem mutation, persistence, events, and
  restart;
- conversation transfer and remote command placement across ownership,
  filesystem handoff, provider state, and execution admission;
- platform lifecycle operations whose build tags or shutdown order are part of
  the boundary.

Keep these sequences explicit. Extract one only when a narrower owner can take
the whole transaction, including its locks, rollback, and failure handling.

## Adding or extracting behavior

Place a new bound method on `internal/app.App`, annotate its transport scope
and route, regenerate Wails bindings, and run `make methodgen`. Prefer an
unexported adapter type for transport bootstrap hooks so they cannot become
RPCs through method promotion.

When behavior has one clear owner, add it to that service and expose only the
capabilities needed by the App facade. Move all mutable state and lifecycle
with it. When behavior coordinates multiple owners, keep the transaction in
`internal/app` and call narrow service APIs.

Provider-capable tests use isolated homes and fake binaries through
`internal/kerneltest`. `internal/app` tests cover wire compatibility,
lifecycle, and cross-service composition; focused behavior tests stay with the
owning package.

See also:

- [refactoring-principles.md](refactoring-principles.md)
- [transport.md](transport.md)
- [turn-lifecycle.md](turn-lifecycle.md)
- [`internal/app/AGENTS.md`](../../internal/app/AGENTS.md)
