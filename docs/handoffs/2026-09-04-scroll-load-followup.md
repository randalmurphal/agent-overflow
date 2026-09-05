# Spring scrolling: active-load follow-up

This extends [the spring/send-glide validation](2026-09-03-mac-validation.md).
Product change: `0df28edac`, following `ff153e3c4`, `eb3a144a9`, and
`bee1fb21f`. Measurements below are from September 4, 2026, on this Windows/WSL
machine. They establish the remaining costs; they are not a build-to-build
memory comparison or a claim of zero dropped frames.

## Fixed work and retained memory

Committed Markdown appends previously validated and sliced the growing full
prefix. The splitter now passes its exact append proof through ChatMarkdown;
the block parser owns an exact source window around its live blocks. Rare
backward block reclassification can still read canonical source. Rewrites and
resets replace both source and proof state, and normalized token text is never
used to reconstruct source offsets. Stable paragraph/list/table/fence fast paths
do not repeatedly maintain an unchanged window.

`parseBlocksSourceWork.test.ts` feeds 800 paragraphs and a growing 50 KB open
fence. Large-source `startsWith`/`slice` work is zero. Removing the prefix proof
inspects 47,667,080 source code units; disabling window retention inspects
23,833,540. Both mutations fail the test. Differential Markdown tests cover
backward HTML/definition reclassification, and the retention negative control
exceeds 20 MiB when window trimming is disabled; the bounded paths stay below
3 MiB for the 73 KB/800-block case. These are controlled parser checks, not
whole-WebView memory measurements.

## Native harness and probe repairs

- Isolated WSL profiles install backend and mock provider together under
  `~/.local/share/agent-overflow/<profile>/bin/`. They do not reuse the normal
  launcher's cached payload path. Launcher cleanup matches its own profile's
  executable names. The installed developer binary's checksum stayed unchanged.
- Windows governor tree sampling now obtains parent PID, birth time and working
  set in one reusable system snapshot. A reused helper PID had incorrectly
  admitted an older protected system process, whose denied handle query caused
  a false safety shutdown. Birth ordering rejects that stale subtree without
  opening protected processes. Native Windows tests cover this case, actual
  snapshot layout/creation times, buffer reuse and malformed offsets. API
  reference: [NtQuerySystemInformation](https://learn.microsoft.com/en-us/windows/win32/api/winternl/nf-winternl-ntquerysysteminformation).
- `mixed-turn` adds thinking, Read output, ANSI Bash output, a real two-hunk
  inline diff and rich text, with 20/120/240 ms chunk pacing and tool pauses.
  It completes naturally. The real Claude parser/Router test verifies the diff
  payload; a catalog test catches CLI/backend workload-list drift.
- The memory sampler consumes one normalized owner identity, accepting the
  documented nested target manifest. It classifies live processes rather than
  retaining types across PID reuse. A final native six-sample capture succeeded.
- Allocation sampling defaults to 1 MiB and records duration, interval and
  success/failure beside the profile. A previous three-minute 64 KiB capture
  lost its oversized response; the new 30-second capture completed.
- Frame reports count Chromium's instant frame markers. Detailed invalidation
  tracking is opt-in; thread duration totals are explicitly inclusive and must
  not be read as CPU utilization.

## Clean native measurements

Owned `perf` profile, native WebView2 152.0.4191.62, maximized 2560x1369 viewport,
DPR 1, physical 165 Hz display. Idle rAF median 6.1 ms, p95 6.2 ms. GPU
compositing and rasterization were enabled on Intel graphics through ANGLE/D3D11.
No trace or allocation profiler ran during these frame measurements.

| Workload | Measured duration | Frames/s | p95 main-thread busy | Maximum frame interval |
| --- | ---: | ---: | ---: | ---: |
| Mixed thinking/tools/diff/text | 12.75 s | 161.7 | 1.0 ms | 18.3 ms |
| Giant turn, 225 items | 35.11 s | 147.4 | 1.0 ms | 24.2 ms |
| Three subagent cards | 5.27 s | 164.8 | 0.8 ms | 12.1 ms |
| Six open panes, four rich-text streams | 92.48 s | 149.2 | 6.2 ms | 66.6 ms |

All completed without frontend errors or engine notices and passed reveal-drain
checks. The four-pane source workload ran for 90 seconds with visible progress
checks at 22.5/45/67.5/90 seconds. Its one >50 ms frame and worst 63.5 ms busy
sample occurred during the final drain, outside the source-active interval.
The full window met 4.17/4.55/6.06 ms busy budgets in 82.9/85.7/93.8 percent of
samples respectively. These budget calculations do not emulate physical
240/220/165 Hz presentation.

A separate fresh-boot, 60-second active-memory leg ran without frontend meters,
tracing, allocation profiling or forced GC during the active interval. All 48
active samples had complete process censuses:

| Private bytes | Active minimum | Active maximum | Last active sample |
| --- | ---: | ---: | ---: |
| WebView2 group | 463.3 MiB | 560.8 MiB | 545.9 MiB |
| GPU process | 271.9 MiB | 299.3 MiB | 282.7 MiB |
| Renderer | 118.4 MiB | 202.0 MiB | 199.9 MiB |

This is native private memory, not JS heap size or summed working set. The full
76-sample capture included one incomplete census outside the active interval,
excluded from the table. A separate detailed 75-second trace hit the harness's
2 GiB ceiling; that diagnostic run is invalid as evidence of normal app memory.
The shorter, lean trace completed within the unchanged safety boundary.

## Remaining costs and stopping boundary

A 25-second trace in the middle of active streaming measured 9.40 seconds of
main-thread RunTask time. Inclusive costs included Paint 3.60 s, FunctionCall
2.39 s, PrePaint 1.24 s, Layout 0.93 s and style 0.69 s; these overlap and must
not be added. No forced-JS layout was reported. The scrollTop read accounted
for about 0.50 s of style work across 3,468 passes, approximately 20 affected
elements per pass. Reading the actual position is needed to distinguish user
input from programmatic scrolling; substituting the last requested position
would break user escape from a spring.

The 30-second allocation sample estimated 138.1 MiB of allocations, primarily
Svelte updates, dependencies and component lifetime work. Growing-prefix scans
no longer appeared as a hotspot. This is sampled allocation throughput, not
retained memory. Natural GC remained present; no forced GC was introduced.

The giant-turn trace also showed frame-delivery gaps with little main-thread
work: 12 of 16 >9 ms task-start gaps in its active window had less than 4 ms
occupied time. Native scheduling/presentation cadence contributes, but this
does not prove a GPU bottleneck. Existing incremental block reuse and virtual
windowing were inspected. The remaining evidence does not identify another
significant application fix with established behavior parity. Suppressing
visible content, dropping motion, or adding containment/layer tricks would
need separate correctness evidence and is not part of this change.

Physical macOS, mobile, and 220/240 Hz hardware were not validated here. Earlier
deterministic spring tests cover 30–240 Hz timing and display quantization;
they do not prove native presentation performance on every display.

## Validation and local evidence

- `make go-build`, `make go-test`: passed.
- Frontend check: zero errors/warnings; production build passed.
- Frontend unit suite: 728 files, 11,759 passed, 5 skipped.
- Focused browser streaming/reuse/outcome suite: 3 files, 6 passed.
- Native Windows governor and launcher isolation tests: passed.
- Perfprobe Node tests: 13 passed; changed JS scripts pass syntax checks.

Reproduce the mixed workload with `bin/ao-harness bench mixed-turn --instance
<owned-root> --page-id <owned-page>`. Use `active-multi-pane --duration 90s` for
load, with `--leg clean-memory` for separate process-memory sampling. Follow
the owner-manifest and separate-instrument rules in
[perfprobe README](../../scripts/perfprobe/README.md).

Local JSON reports: `/tmp/ao-native-{mixed,giant-turn,subagent-fanout,active-multi-pane}.json`,
`/tmp/ao-native-memory-bench.json`, `/tmp/ao-native-clean-memory-summary.json`.
Gate logs: `/tmp/ao-final4-{go-build,go-test,check,build,frontend-tests,browser}.log`.
Windows artifacts under `%LOCALAPPDATA%\Temp\ao-spring-native-probes`:
`native-active-memory.csv`, `trace-native-active-lean.json`,
`trace-native-giant-turn.json`, and `alloc-1788567255492.heapprofile` with its
metadata sidecar. These temporary artifacts are not checked into the repository.
