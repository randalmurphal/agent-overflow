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

Start the app with `DEBUG=1 make dev-wsl` (CDP on 9223), or the soak rig with
`make soak` (CDP on 9224). Then, from the repo root:

```
scripts/perfprobe/probe                     # usage plus the probe list
scripts/perfprobe/probe overview
scripts/perfprobe/probe memdump --renderer --classes
scripts/perfprobe/probe heapsnapshot before
scripts/perfprobe/probe snapshot-detached /mnt/c/Users/<you>/AppData/Local/Temp/ao-perfprobe/heap-before.heapsnapshot
AO_CDP_PORT=9224 scripts/perfprobe/probe sample --every 60 --for 3600
```

Env:

- `AO_CDP_PORT`: CDP port, default `9223`.
- `AO_PERFPROBE_OUT`: Windows directory for staged scripts and saved traces and
  snapshots, default `%LOCALAPPDATA%\Temp\ao-perfprobe`. The wrapper prints its WSL
  path after every online run so you can feed saved files back to the offline probes.

All probes are read-only except `ab`, which injects a CSS override and can drive
synthetic wheel scrolling. It refuses port 9223 without `--allow-user-app` because
that is the port of the app somebody is actually using.

`sample --detached` adds a `Runtime.queryObjects` node census, which runs a full
memory-reducing GC on every tick and so changes the memory it reports. Keep it off for
footprint-over-time curves; use `detached` or a heap snapshot for retention questions.

Only one tracing session can exist on the browser target at a time, so `memdump`,
`sample`, `churn`, `tiles`, `frames` and `ab` must not run concurrently.

## Method

The probes are the instrument, not the investigation. For how to use them (which
process a number belongs to, what a growth curve has to show before it is a leak,
what to rule out first), see
[.claude/skills/perf-investigation/SKILL.md](../../.claude/skills/perf-investigation/SKILL.md).
