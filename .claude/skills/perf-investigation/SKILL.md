---
name: perf-investigation
description: Investigate application memory, CPU, retention and frame pacing using owned harness workloads, browser probes and platform traces. Attribute measured costs to their owning code and verify fixes with controlled comparisons.
---

# Performance investigation

Read [REFERENCE.md](REFERENCE.md) for measurement interpretation and probe
constraints. Report the measured symptom, confirmed mechanisms, implemented
fixes, verification and remaining uncertainty. Distinguish evidence from a
hypothesis; an inconclusive trace is a valid limit, not permission to invent a
cause.

## Scope and ownership

- Follow the repository's [product decisions](../../../docs/decisions.md) for
  performance and memory tradeoffs. Optimize the cost of supported behavior.
- Reproduce and compare on an owned harness, clone or soak instance. Keep the
  user's running app read-only unless the session authorizes a visible change.
  Do not infer permission to restart it from permission to profile it.
- Online CDP probes require the ownership manifest described in the
  [probe README](../../../scripts/perfprobe/README.md). Do not bypass its guards.
- Use `ao-harness clone` for a copy of application data and mock replay. Clones
  contain private conversation content and remain outside tracked files.
- `make dev-wsl` embeds the frontend. Verify the running build before claiming a
  source edit is active, and report when a rebuild/relaunch is still needed.

## Choose the instrument

- Harness `perf`, `bench`, `ui` and `health` commands correlate page measurements
  with backend and process-tree usage. They also support webviews without CDP.
  Read the [harness architecture](../../../docs/architecture/agent-harness.md).
- `scripts/perfprobe/probe` exposes Windows WebView2 probes for memory dumps,
  allocation profiles, frame traces, layers, DOM retention and owned A/B runs.
  Select the exact instance and page through its manifest.
- Go heap and CPU profiles identify backend allocation and execution costs.
  Use the configured diagnostic endpoint rather than assuming a port is free
  or belongs to the target instance.
- Platform presentation and scheduling traces are needed when page callbacks
  look regular but displayed motion or the whole desktop stalls.

## Investigation sequence

1. Establish process ownership, build/runtime identity and a bounded workload.
   Record focus, visibility, viewport, workload state and competing activity.
   Validate sustained motion before measuring scrolling.
2. Choose the measurement that answers the symptom. Split memory by process,
   allocator and live/committed state. Split frame pacing into callback timing,
   content movement and physical presentation. Measure baseline and candidate
   under the same conditions; repeat small effects with A/B/A.
3. For growth, separate an ordinary footprint curve from forced-GC retention
   measurements. Follow surviving objects to their retainers. For churn, use
   allocation profiles to identify the code producing temporary objects.
4. For frame cost, correlate JS, style, layout, paint and presentation work.
   Trace a specific delayed frame through its owning subsystem. Verify provider
   coverage and clock alignment before assigning a platform cause.
5. Fix a confirmed cause in its owner and check sibling callers. Add a regression
   test for the behavior the change could break. Use an isolated prior build to
   prove the failure when practical; preserve unrelated local changes.
6. Run the applicable checks from the repository and area guides, then repeat
   the controlled measurement. Test cancellation, failure and cleanup for
   processes, observers, timers and other resources introduced by the fix.
7. Stop owned captures and test workloads, release temporary flags and restore
   changed settings. Verify cleanup. Keep only useful development contracts in
   maintained documentation; raw evidence and machine-specific notes stay out
   of Git.

A result can identify separate application and platform contributors. Report
what each fix improves without claiming it explains every symptom. A successful
counter comparison does not override the user's observation of motion.
