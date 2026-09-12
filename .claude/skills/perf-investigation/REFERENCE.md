# Performance investigation reference

Use current measurements from a controlled workload. Results depend on the
build, browser runtime, display configuration and background activity; historical
machine measurements are not acceptance thresholds.

## Interpreting memory

- Identify the launcher, browser, renderer and GPU processes by parentage and
  profile directory. Other applications can use the same WebView2 executable.
- Distinguish private working set, committed allocator memory and live objects.
  GPU memory accounting differs between integrated and discrete adapters.
  Separate raster tiles, shared images, Skia and unattributed driver allocations.
- Compare live memory after collection with committed memory and allocation rate.
  Stable live objects with growing committed pages can indicate fragmentation or
  retained allocator capacity. Increasing live objects need a retainer analysis.
- A forced collection changes the workload. Use it to separate live objects from
  garbage, not as a leg of an ordinary memory-footprint comparison.
- Detailed dumps, heap snapshots and object queries can pause execution or cause
  collection. Record those interventions and exclude them from steady samples.
- Heap snapshot `detachedness === 2` identifies detached nodes. Follow retaining
  paths to determine whether a component, signal or observer still owns them.
- Confirm trace categories and sampled stacks before attributing CPU time.
  A sample at a native binding does not by itself measure that binding's cost.

## Measuring motion and presentation

- Measure animation timestamps, actual callback spacing, scroll increments and
  displayed-frame intervals separately. Regular callbacks do not prove regular
  presentation, and a presented frame need not contain a new content update.
- With mixed refresh rates, callbacks can run on one output while animation
  timestamps follow another cadence. `scroll/spring.ts` uses the shared elapsed
  clock at callback entry. Its clock regression test supplies independent
  callback and animation clocks.
- Validate overflow and sustained scroll movement before recording. A layout
  jump or a completed response that fits in the viewport is not a scrolling
  workload. Record focus, visibility, viewport and source identity per phase.
- Windowed and fullscreen presentation can use different composition paths.
  Compare the recorded presentation mode and output capabilities rather than
  inferring the path from a window's appearance.
- Native presentation controls help distinguish frontend work from compositor
  and driver behavior. Keep settings and competing workloads fixed; a reboot
  can change several variables and is not an isolated setting comparison.
- Repeated console creation can block desktop display queries even when the
  console is hidden. The isolated WSL memory watchdog keeps one pipe-only
  process. Keep recorders continuous and exclude their startup from samples.
- Confirm ETW provider coverage and lost-event counts. Zero events from a
  missing provider cannot rule out that subsystem. Validate decoded fields and
  align clocks before associating a scheduler or GPU event with a missed frame.
- GPU elapsed time can include waits. Separate busy time, synchronization,
  queueing and display latency before blaming rendering cost.

## Finding the owner

| Symptom | Code and measurement to inspect |
|---|---|
| Uneven scrolling with regular callbacks | `utils/scroll/`, elapsed clock and quantized position increments |
| Repeated style/layout work | Mutation sources, featureless global selectors, synchronous geometry reads |
| Large raster memory | Painted layer dimensions and tile bytes, not layer count alone |
| Streaming allocation churn | Markdown tail parsing, reveal queues, projection identity and repeated allocations |
| Detached component retention | Svelte reactions, observer cleanup and heap retaining paths |
| Repeated backend allocation | Provider framing, persistence batches and Go allocation profiles |
| Whole-desktop stalls | Presentation trace, process creation, scheduler waits and graphics-driver events |

Paths in this table are under `frontend/src/lib/` unless they name a backend or
platform subsystem. The owning contracts are in
[frontend scrolling](../../../docs/architecture/frontend-scroll.md),
[frontend guidance](../../../frontend/AGENTS.md), and the relevant package guide.
Product tradeoffs remain in [product decisions](../../../docs/decisions.md).

## Instruments and gotchas

- Follow the [probe ownership contract](../../../scripts/perfprobe/README.md).
  Online CDP probes require a harness ownership manifest and matching page
  marker. An ordinary dev window does not carry that marker. Do not bypass
  the ownership guard to attach a probe to it.
- One browser tracing session can run at a time. Continuous sampling, memory
  dumps and frame traces can conflict; stop the owned session before switching.
- `sample --detached` forces collection on each sample. Use a separate census
  when measuring retention and ordinary footprint over time.
- Use the harness's own perf, bench, UI and health commands for WebKitGTK or
  other targets without a CDP endpoint. See the
  [harness contract](../../../docs/architecture/agent-harness.md).
- Mock scenario rules are in memory and must be restored after backend restart.
  Check provider, workspace and session scope. Reveal backlog can outlive wire
  delivery, so validate visible movement rather than relying on mock status.
- Use a fresh harness launch URL and page ticket. A bare port or stale URL may
  load a page without establishing its authenticated harness connection.
- Closing an isolated native launcher also stops its backend. Stopping a WSL
  build process alone does not necessarily close the Windows window; use the
  supported harness teardown and verify process identity before cleanup.
- Windows loopback may require Windows-hosted tooling when invoked from WSL.
  Stage PowerShell scripts and use `-File` to avoid nested shell quoting.
- Source replacement, trace serialization and forced collection can alter the
  renderer state. Restore the original state or restart the owned rig before
  comparing memory totals.
- Programmatic `scrollTop` writes are not reader intent. Use the supported
  gesture fixture when testing escape from or re-engagement with tail follow.

## Regression verification

Use a deterministic unit test for clock/state behavior and a browser test for
geometry, focus or actual scroll movement. Reproduce failures with the previous
implementation, then run the same checks with the fix.

For dependency patches, use an isolated checkout with the prior patch and lockfile
for the failing case. Apply edits with `pnpm patch` and `pnpm patch-commit`, then
reinstall and run the passing case. Do not edit installed dependencies by hand
or replace unrelated working-tree changes to construct a baseline.

Keep raw traces, conversation clones, host inventories and detailed investigation
notes outside Git. Committed fixtures use synthetic identities and paths while
preserving the protocol behavior they test. Carry forward mechanisms and test
contracts, not machine descriptions, dated measurements or work logs.
