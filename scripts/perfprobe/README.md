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
AO_PERFPROBE_MANIFEST=<supervisor instance manifest> \
scripts/perfprobe/probe realuse --for 86400 --every-ms 10000
```

The `AO_PERFPROBE_MANIFEST` setting applies to every online command in the
examples above. It is a probe-ownership manifest, not the harness lifecycle
`run-manifest.json`. Create one after the page is open as described below.

Env:

- `AO_CDP_PORT`: CDP port, default `9223`.
- `AO_PERFPROBE_OUT`: Windows directory for staged scripts and saved traces and
  snapshots, default `%LOCALAPPDATA%\Temp\ao-perfprobe`. The wrapper prints its WSL
  path after every online run so you can feed saved files back to the offline probes.
- `AO_WINDOWS_NODE_EXE`: exact WSL path to Windows `node.exe`. Set this for a
  systemd user service whose restricted `PATH` cannot discover Windows programs.
- `AO_PERFPROBE_MANIFEST`: supervisor-created JSON manifest for the exact
  instance, loopback origin, page target ID, page marker, and shared probe lease.
  Every online probe requires it. Direct script execution uses the same check.
- `AO_PERFPROBE_LEASE`: optional lease directory override. The manifest's
  `leasePath` wins when present.

### Prepare a probe manifest

Online probes refuse a bare CDP port. Before running one, bind the exact
instance and page to a private manifest. This sequence is safe for the WSL
perf shell, whose CDP port is 9226. Use 9224 for the soak shell.

```sh
INSTANCE="$HOME/.agent-overflow-perf"
CDP_PORT=9226
INFO_JSON="$(bin/ao-harness --instance "$INSTANCE" -o json info)"
INSTANCE_ID="$(printf '%s' "$INFO_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
PAGE_ORIGIN="$(printf '%s' "$INFO_JSON" | python3 -c 'import json,sys; from urllib.parse import urlsplit; print(urlsplit(json.load(sys.stdin)["url"]).scheme + "://" + urlsplit(json.load(sys.stdin)["url"]).netloc)')"
PAGE_MARKER="$(printf '%s' "$INFO_JSON" | python3 -c 'import json,sys; pages=json.load(sys.stdin)["info"].get("frontendPages", []); print(pages[0]["marker"] if len(pages)==1 else "")')"
test -n "$PAGE_MARKER" || { echo "expected exactly one registered frontend page; inspect `bin/ao-harness --instance \"$INSTANCE\" -o json info`" >&2; exit 2; }

# Query Windows loopback through node.exe and require exactly one page on the
# selected backend origin with the selected harness page marker.
NODE_EXE="${AO_WINDOWS_NODE_EXE:-node.exe}"
TARGET_ID="$("$NODE_EXE" -e '
const [origin, marker, port] = process.argv.slice(1);
fetch(`http://127.0.0.1:${port}/json/list`).then((response) => response.json()).then((pages) => {
  const matches = pages.filter((p) => {
    if (p.type !== "page" || !p.webSocketDebuggerUrl) return false;
    const url = new URL(p.url);
    return url.origin === origin && url.searchParams.get("page") === marker;
  });
  if (matches.length !== 1) throw new Error(`expected one owned page, found ${matches.length}`);
  process.stdout.write(matches[0].id);
}).catch((error) => { console.error(error.message); process.exitCode = 1; });
' "$PAGE_ORIGIN" "$PAGE_MARKER" "$CDP_PORT")" || exit 1
test -n "$TARGET_ID" || { echo "could not resolve the owned CDP page" >&2; exit 1; }

MANIFEST_WSL="$(mktemp -p "$INSTANCE" ao-perfprobe-manifest.XXXXXX.json)"
LEASE_DIR="$INSTANCE/perfprobe-leases"
MANIFEST_WIN="$(wslpath -w "$MANIFEST_WSL")"
LEASE_WIN="$(wslpath -w "$LEASE_DIR")"
python3 - "$MANIFEST_WSL" "$INSTANCE_ID" "$PAGE_ORIGIN" "$PAGE_MARKER" "$TARGET_ID" "$LEASE_WIN" <<'PY'
import json, pathlib, sys
out, instance_id, origin, marker, target_id, lease = sys.argv[1:]
pathlib.Path(out).write_text(json.dumps({
    "instanceId": instance_id,
    "origin": origin,
    "target": {"targetId": target_id, "pageMarker": marker},
    "leasePath": lease,
}) + "\n")
PY
chmod 600 "$MANIFEST_WSL"
export AO_CDP_PORT="$CDP_PORT" AO_PERFPROBE_MANIFEST="$MANIFEST_WIN"
scripts/perfprobe/probe webviewmem --for 60 --every-ms 1000
rm -f "$MANIFEST_WSL"
```

The manifest records no token. Keep it under the selected isolated root, do
not reuse it after the page or backend restarts, and remove it when the probe
ends. A new page target requires a new manifest. The probe lease directory is
also instance-local, so separate worktrees cannot block one another.
- `AO_PROBE_MAX_EVENT_BYTES`: maximum in-memory CDP event history per
  connection, default 16 MiB. The connection fails loudly when exceeded.
- `AO_PROBE_MAX_STREAM_BYTES`: maximum one streamed CDP result, default 512
  MiB. This bounds traces and heap snapshots before they can exhaust the
  probe process.

No probe changes persisted app state. `scroll-contract` mounts an invisible
offscreen scroller for one synchronous readback check and removes it before
returning. `compositor-contract` reads computed styles and the layer tree of the
mounted timeline. `ab` injects a CSS override and can drive synthetic wheel
scrolling. Mutating probes require the supervisor manifest and have no user-app escape hatch.
The supervisor manifest must name the exact instance even on a developer-selected
port.

The declarative policy in `lib/policy.mjs` classifies every online probe and
refuses unlisted scripts or incompatible CDP instruments. The shared lease
allows compatible counter/page-observer pairs and refuses competing trace,
profiler, or mutating instruments. Ownership is revalidated before every CDP
command, including after a target replacement.

`realuse` is the deliberate consent-gated exception. It records focused and
unfocused rAF gaps in fixed-size histograms, samples main-thread busy time once
per 16 focused frames, observes long tasks/animation frames/input events, and
polls cumulative Chromium task, heap, DOM, and per-process CPU counters. It does
not record page text, URLs beyond the loopback origin, screenshots, input, or
stack traces. Hidden intervals reset the frame clock, and gaps above two seconds
are labeled as suspend/resume instead of dropped frames. The page state has a
two-minute watchdog and is removed on a clean collector exit. `realuse-state`
reports whether that page-side monitor remains armed.

The JSONL appends one labeled session per collector restart. Summarize it,
optionally joined with the exact-profile memory CSV, without attaching to the
app:

```
scripts/perfprobe/probe realuse-report \
  --telemetry /path/to/realuse.jsonl \
  --memory /path/to/webview-memory.csv
```

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
`--append` keeps one CSV across collector restarts without repeating its header;
it refuses to append after an incomplete final row rather than corrupting the
next sample.
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
