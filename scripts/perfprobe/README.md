# perfprobe

Chrome DevTools Protocol probes for the WebView2 window of a running Agent Overflow
dev build. They answer where renderer, GPU and browser-process memory goes, what the
main thread spends frame time on, and what is still retained after a pane closes.

The dev build runs the Go backend inside WSL and the WebView2 window on Windows, and
WebView2 exposes CDP on **Windows loopback**, which WSL cannot reach. So the online
probes are staged into a Windows directory and executed there under `node.exe`
(Node 22+, no npm dependencies). Offline probes read a saved snapshot or trace and
run under WSL `node` with a raised heap limit.

## Running

Start the app with `DEBUG=1 make dev-wsl` (CDP on 9223), the soak rig with
`make soak` (CDP on 9224), or the destructive benchmark target with
`make perf-wsl` (CDP on 9226). Then, from the repo root:

```
scripts/perfprobe/probe                     # usage plus the probe list
scripts/perfprobe/probe overview
AO_CDP_PORT=9224 scripts/perfprobe/probe scroll-contract
AO_CDP_PORT=9224 scripts/perfprobe/probe compositor-contract
scripts/perfprobe/probe memdump --renderer --classes
scripts/perfprobe/probe heapsnapshot before
scripts/perfprobe/probe snapshot-detached /mnt/c/Users/<you>/AppData/Local/Temp/ao-perfprobe/heap-before.heapsnapshot
AO_CDP_PORT=9224 scripts/perfprobe/probe sample --every 60 --for 3600
AO_CDP_PORT=9226 scripts/perfprobe/probe webviewmem --for 600 --every-ms 1000
```

Env:

- `AO_CDP_PORT`: CDP port, default `9223`.
- `AO_PERFPROBE_OUT`: Windows directory for staged scripts and saved traces and
  snapshots, default `%LOCALAPPDATA%\Temp\ao-perfprobe`. The wrapper prints its WSL
  path after every online run so you can feed saved files back to the offline probes.

No probe changes persisted app state. `scroll-contract` mounts an invisible
offscreen scroller for one synchronous readback check and removes it before
returning. `compositor-contract` reads computed styles and the layer tree of the
mounted timeline. `ab` injects a CSS override and can drive synthetic wheel
scrolling. It refuses port 9223 without `--allow-user-app` because that is the port
of the app somebody is actually using.

Port 9223 also hard-refuses probes that install page observers/state, alter
rendering, reload, click, type, or scroll. Run those against perf on 9226. The
guard is central in `probe`, including for older scripts with no local refusal;
read-only traces, profiles, process/DOM censuses, and screenshots remain allowed.

`resizewatch [seconds]` attributes ResizeObserver loop errors to observer
registration stacks and recent targets. It reloads once to install the wrapper
and once to remove every instrumented observer, so it is hard-disabled on port
9223 and belongs on the isolated soak rig. The target is clean again before the
probe exits, including on failure.

`driveburn [thread-title] [prompt] [--mount-only]` targets an exact visible pane
header. It mounts the named thread beside the current pane when needed, then
sends through that pane's composer. `--mount-only` stops after the pane
transition, which pairs with `resizewatch` for deterministic layout tests.

Use `bin/ao-harness --instance "$HOME/.agent-overflow-perf" bench
active-multi-pane` for the normal active-use case. It mounts six panes before
measurement, streams paced rich Markdown into four for 30 seconds by default,
and records low-frequency DOM text and scroll-height growth before interrupting
the mock turns. Endurance runs set `--duration` explicitly and use the WebView
memory ceiling below. Durations below 30 seconds are refused.
`multi-pane-stream` remains the separate three-pane 1ms flood test. The
background-agent soak is a hang reproduction and does not prove that visible
Markdown tails are changing.

`sample --detached` adds a `Runtime.queryObjects` node census, which runs a full
memory-reducing GC on every tick and so changes the memory it reports. Keep it off for
footprint-over-time curves; use `detached` or a heap snapshot for retention questions.

`webviewmem` samples the exact WebView2 profile identified by the selected CDP
endpoint. It re-censuses that profile on every sample, so renderer/utility
replacements and late children cannot fall out of the total. It records private
bytes, total working set, and private working set for the browser, GPU, renderer,
utility, and crashpad processes without tracing or forcing GC. It can run beside
`bench active-multi-pane`; the CSV and p50/p95/max summary provide the Windows-side
peak that the in-page perf meter cannot see. Do not treat a run carrying `frames`,
`cpu`, `alloc`, a heap snapshot, or a memory dump as an app-footprint run. Those
probes allocate profiler buffers inside Chromium.
For an intentionally destructive perf-profile run, add
`--kill-at-private-working-set-mb <MB>`. It is hard-disabled outside CDP 9226,
verifies the exact perf-profile WebView data directory and launcher parent, then
terminates both launcher and browser if the ceiling is crossed. Killing the
launcher prevents its normal browser restart from resuming the same workload.
Renderer-policy A/Bs can add `--require-browser-arg <exact-switch>` so the
sampler refuses to record a mislabeled run when WSL environment forwarding or
launcher argument assembly dropped the intended switch.

Only one tracing session can exist on the browser target at a time, so `memdump`,
`sample`, `churn`, `tiles`, `frames`, `checkerboard` and `ab` must not run
concurrently. `checkerboard` records Chromium's own missing-tile and
checkerboarded-content counters, so it distinguishes compositor loss from a DOM
that actually lost text. `markdownwatch` is the other half: on a harness build it
checks every painted streaming-Markdown mutation for a 128-character text drop
and compares every completed top-level fence with its rendered `<code>` text.
It takes no screenshots and retains only bounded failure snippets. Run the two
together with `markdownwatch` in the background and `checkerboard` as the sole
trace owner; do not use `markdownwatch` for allocation or memory numbers because
its source and `textContent` reads intentionally add forensic work.
`markdownstate` is a read-only point sample of each visible assistant row's
canonical source, parser source, committed root, and volatile root. Use it
immediately before a long-turn settle to distinguish retained volatile DOM from
completion-time reparsing without installing an observer.
`checkerboard` is capped at 15 seconds. Chromium retains hundreds of megabytes
of trace buffers in its tracing utility process, and repeated or long captures
can make the profiler itself cross a safety ceiling. Sample one 15-second
segment inside a longer workload, then restart the isolated window before a
clean footprint run.

`mutations` counts attribute writes; `attrflap <attr>` says whether they change
anything. A writer that rewrites the value already there costs a style
invalidation per write and moves nothing, and a value alternating between two
answers is usually visible flicker on top of the cost. Neither reads as
different from real work in the `mutations` census. Both use only
`Runtime.evaluate`, so they run alongside a `sample` curve.

`evalq '<expr>'` runs one ad-hoc expression in the page and prints the JSON
result — for one-off censuses that don't earn a probe of their own (count
distinct mask URIs, list running animations by target). `Runtime.evaluate`
only, so it also runs alongside a sampler. An arbitrary expression cannot be
classified at the wrapper boundary, so `evalq` is isolated-rig-only even when
the intended expression is read-only.

## Method

The probes are the instrument, not the investigation. For how to use them (which
process a number belongs to, what a growth curve has to show before it is a leak,
what to rule out first), see
[.claude/skills/perf-investigation/SKILL.md](../../.claude/skills/perf-investigation/SKILL.md).
