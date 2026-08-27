# Perf rigs

Self-driving measurement rigs for the perf campaign: each attaches a
headless Playwright Chromium to a harness instance and drives load
through `bin/ao-harness`, so no human activity is required and the
user's running app is never the capture instrument. They are operator
tools, not tests — nothing here runs in any gate.

| Rig | Question it answers |
|---|---|
| `storm.mjs` | Frame-budget fit under the heaviest normal load: N panes streaming concurrently. Captures cpuprofile + timeline trace (with stacks) + the harness busy meter. |
| `churn.mjs` | Active-use heap dose-response: looping replay rounds for hours, heap/LoAF/DOM samples per round, forced-GC live-vs-garbage split at the end. |
| `heapsoak.mjs` | Passive uptime curve: heap + LoAF every 2 minutes against a `make soak --autopilot` instance. |
| `coldload.mjs` | Thread-switch cold-load cost: sequential opens of heavy threads in one pane, profiled. |

## Standing up an instance

The realistic venue is a **clone root** — a harness data dir built from
a copy of a real app data dir (`ao-harness clone`), so threads carry
real sizes and shapes. The clone contains real conversation content:
keep it under `/tmp`, never commit it, and delete it when done. Boot:

```sh
systemd-run --user --unit=ao-rig --collect --setenv=PATH="$PATH" \
  <binary> --harness --data-dir /tmp/<clone-root> \
  --mock-provider <repo>/bin/ao-mockprovider
journalctl --user -u ao-rig | rg -o '__AO_HARNESS__.*' | tail -1   # url + token
```

`systemd-run --collect` (not a backgrounded shell) so the rig survives
the operator session. Thread titles/ids for `--threads` come from
`ao-harness threads`.

## Facts the hard way

- **Scenario rules are in-memory per boot** and reach only mocks that
  register after `scenario set`. Reinstall after every restart; a live
  mock keeps its old script until the instance restarts.
- **`scenario from-thread` emits `afterTurns: silent` by design.** A rig
  that replays the same session repeatedly must flip the doc to
  `repeatLast` (`riglib.installReplayScenarios` does) or turn 2+ wedges
  at the send timeout.
- **Storm density is not provider density.** Replay streams at 15ms/line;
  real providers flush ~100ms windows. Budget-fit percentages compare
  storm-to-storm on the same box (note cross-load from other rigs), not
  storm-to-live.
- **End measurement windows on the reveal drain** (`awaitRevealDrain`),
  never on wire completion — the reader has not seen the stream when the
  send resolves. The drain read is read-only; nothing in a measurement
  path may skip, rush, or pop it.
- `--enable-precise-memory-info` is required for `performance.memory`
  samples (churn/heapsoak launch with it).
- Analysis recipes (windowed cpuprofile attribution, tall-RunTask
  decomposition, trace event counts) live in
  `.claude/skills/perf-investigation/REFERENCE.md`.
