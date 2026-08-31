# internal/workflowhost/

The app-side workflow **runner**: the `engine.Runner` implementation that
turns one workflow element (a phase, a fan-out unit, or a join) into a
live provider turn or a supervised command, and everything a single
attempt needs while it runs.

The engine (`internal/workflow/engine`) owns the FSM and decides WHAT
runs next. This package owns HOW one attempt happens and reports exactly
one fate back. Spec: `docs/specs/workflows-system.md`.

## The host seam

The runner used to be `workflowAppRunner` in package `main` with an
`app *App` field, which made all 19 App members it happened to reach part
of its contract by accident. It now holds `host Host` (`host.go`): nine
capability-named consumer-side interfaces composed into one field.
`SessionHost`, `TurnHost`, `ThreadHost`, `WorktreeHost`, `PromptHost`,
`EventEmitter`, `EngineSource`, `ProcessLifetime`, and
`ProviderHomeSource`, the last of which exists because an isolated boot
(`--harness` / `--soak`) and a test fixture both pin a provider home
that is not `$HOME`.

Rules that keep the seam a seam:

- **`internal/app` satisfies it through exactly one adapter**,
  `workflowHostAdapter` in `internal/app/app_workflow_host.go`. Every method is a
  forward to the App's own unexported one and nothing else. Behavior
  belongs on the App method; the adapter must stay pure glue. It exists
  because an interface declared outside `internal/app` cannot name an
  unexported method, and exporting the App methods would ripple through that package
  further than the forwards do.
- **Adding a capability means adding it to a seam**, not reaching around
  it. There is no `*App` here and none may come back.
- **The store is NOT a host capability.** It is a dependency of the
  runner, held as `*store.Store` directly, the way every other workflow
  collaborator in `internal/app` (`workflowProfileSource`,
  `workflowDefinitionSource`, `workflowSpendSource`) holds it.
- **Nothing here registers on the wire.** Bound methods stay in `internal/app`;
  `App.workflowRunner` is the only reference into this package.

## Layout

| File | Owns |
|---|---|
| `runner.go` | `Runner`, its registries (`runs`, `schemas`, `workItems`, `takeovers`, `tools`, `startProgress`), `New`, `Start`, `Stop`, `installAttempt`, `finish`, the schema-restart, and the exported reads `main` uses (`SessionSchemaForThread`, `WorkItemForThread`, `DataRoot`, `WorkspaceLockRefs`). |
| `host.go` | The seams above plus `DispatchIdentity`. |
| `agent_turn.go` | Preparing the thread and provider session one agent turn runs on, including the continuation preflight that proves provider context still exists. |
| `thread.go` | `ThreadSpec`, `ThreadTitle` / `UnitThreadTitle`, and the prior-thread validation a reused session must pass. |
| `workspace.go` | Item-worktree provisioning and adoption, fan-out sub-worktrees, `PreparedWorkspace`, and the branch-layout contract (`ItemBranchPrefix`, `UnitBranch`, `UnitWorkspaceRef`). |
| `units.go` | Fan-out unit and join planning, the per-unit start, and unit-worktree retirement. |
| `reliability.go` | `Timer`, watchdog/backoff resolution and arming, the transient-failure allowlist, and the send-wait bound (`StopSendWait`). |
| `send.go` | The one send chokepoint (`sendIfActive`), its epoch ladder, and the drop reasons. Every drop is logged by the door itself, never by caller discipline. |
| `observe.go` | The provider-event observer: which events may move the turn machine and what is left armed afterwards. |
| `quota.go` | The typed usage-limit park (never a retry) and the bounded failure-detail rendering every park cause passes through. |
| `start_watchdog.go` | The bound on `Start`: the deadline that cancels a wedged start and the grace fallback for a wait the context cannot reach. |
| `takeover.go` | Human takeover: detach without losing the reliability deadline, the yield wait, and the Claude schema swap. |
| `tool.go` | `driver: tool` phases: process supervision, envelope synthesis, and the reaper. |
| `narrative.go` | Settling an accepted attempt's narrative file from its three ordered sources. |
| `artifacts.go` | Capture plus safe listing of per-run artifacts: `CaptureArtifact`, `ListArtifacts`, `OpenArtifactRoot`, and `ArtifactDir`. `internal/app` retains the Wails DTO conversion. |

## Testing

`fixture_test.go` holds `fakeHost` (every capability a settable func
with an inert default) plus `newTestRunner` and `newTestStore`. A test
states the two or three capabilities its subject actually reaches and
leaves the rest alone; that is the whole point of the seams.

Deliberate choices in the fixture:

- The **git half is a real `gitops.Core`**. The provisioning rules under
  test are decided by what git answers, and the App's own
  implementations of those four seams are thin wrappers over the same
  Core (`app_worktree.go`).
- `SubscribeThreadTurnObserver` defaults to a **real observer bus**, so a
  test can tell a fresh resubscription from a dangling reference by
  dispatching through it.
- `newTestRunner` installs `kerneltest.IsolateSpawns`. The runner never
  spawns (every process it would start is behind a host seam), but the
  continuation preflight reads a provider home directly
  (`claude.ScanSessionLeaf`), and that read must never reach the
  developer's real `~/.claude`. See the root guide's permanent
  invariants.
- Store-backed tests clone the package template via
  `storetest.Clone` (`main_test.go` runs `storetest.Run`).

Tests that exercise App-level workflow behavior through bound methods
(engine-driven end-to-end runs, `workflowSchemaForSession`, the
access→runtime-mode mapping, project deletion against a live run) stay
in `internal/app`.

## References

- `docs/specs/workflows-system.md` describes the system this implements.
- `docs/architecture/root-decomposition.md` § Stage 3+ covers why the
  move happened and what stayed behind.
- `internal/workflow/engine/` is the FSM that calls `Start` / `Stop`.
