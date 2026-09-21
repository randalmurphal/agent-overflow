# Subagent scope windows

## Goal

Load a subagent pane with the same complete, windowed history and activity-run
behavior as a normal thread, including remote access and reconnect recovery.

## Behavior

- Keep the existing pane presentation, tail following, reader escape, restore,
  nested-agent navigation, and read-only composer.
- Load direct children of the canonical transcript root. The pane includes all
  executions; completed timeline entries keep their existing execution bounds.
- The background tray expands tool activity only. It does not render prose or
  thinking and does not share the pane's scroll or expansion state.
- Search results inside an agent open its pane at the exact row with a breadcrumb,
  preserving the main timeline's reading position.
- Every omitted eligible row has a paging or run-member recovery route. Every
  shortened visible body has an expansion route to its actual content source.
- Do not add a pinned prompt, task-summary panel, or new completion presentation.

## Ownership

Use the same store page composition, byte projection, run-member reads, and held
window verification for main and scoped timelines. A validated selection names
its transcript root and whether it selects tool activity only. Completed inline
cards retain their existing execution-bounded selection.
Selection is distinct from page shape. Imported history and local rows obey the
same selection and index contracts.

Each rendered surface owns its reading window, activity-run presentation, scroll
controller, and row-resource leases. Transcript rows do not depend on a launch
being inside the host timeline's current window. Scope context supplies launch
identity and current lifecycle independently of the history range; it is not a
new visible header. Live execution state retains its existing owner.

All item events, removals, reverts, and backend invalidations reach the owning
window through the ordered event path. Reconciliation preserves newer live
changes and revalidates history already loaded by the reader. Backend ownership,
request generations, and replica lineage fence late responses. Scope resources
participate in watches and reconnect recovery.

## Removal

The pane and tray use paged reads instead of wholesale descendant hydration.
They no longer retain child-row islands in the main thread or depend on its
hydration exhaustion state. Inline completion-card digests keep their separate
execution-bounded history contract.

## Validation

- Store/app: complete traversal beyond old caps, byte progress, scoped runs and
  member reads, immutable completion context, imported/local parity, query plans, context
  changes with unchanged held rows, and invalid cross-scope requests.
- Client: independent windows, all event types, replay, snapshot races, deletes,
  release/reacquire, backend ownership and generation changes, and payload leases.
- Harness/browser: cold and live opens, nested navigation and search, tools-only
  tray, simultaneous readers, loaded-history recovery, restart and replica miss,
  immutable completed executions, compact layout, and remote routing.
- Content: long detail, prompt and output expansion through the correct source.
