# The testing harness

One harness, many shells. This spec grows the existing agent test harness
(docs/architecture/agent-harness.md) into the project's general testing
rig: the place agents validate their own changes (functional, visual,
and performance) against the real app with mocked providers, on any
platform, from any worktree, without screenshots and without a real LLM.

Status: implemented contract and design rationale. All implementation waves
listed at the bottom have landed. Update the living architecture and command
guides with behavior changes, then update this contract when the design
changes.

## What already exists (do not rebuild)

- **Isolation core**: `prepareHarness` + `newIsolatedProviderApp`
  (main_harness.go): data-dir refusals, HOME redirect, the four provider
  pins, settings seed. Enforced by `TestMockedBootModesShareOneIsolationHelper`.
- **Harness RPC receiver** (app_harness*.go): info, seed, reset,
  scenarios, mock drive, record/replay, thread-row escape hatch.
  Registered only under `--harness`/`--soak`, receiver-level LocalOnly.
- **Mock provider** (cmd/ao-mockprovider): both provider wire protocols,
  scenario engine (internal/harness/scenario), control channel
  (internal/harness/control), embedded scenario library.
- **e2e** (Playwright, e2e/) with the TS client in `e2e/src/harness.ts`.
- **Soak rig** (--soak + Windows launcher `--profile soak`): the
  Windows windowed shell. Stays; this spec adds the native windowed
  shell beside it and folds the docs together.
- **Perf shell** (`make perf-wsl`, launcher `--profile perf`): a third
  Windows harness with its own data root and CDP 9226. Destructive benches
  name it explicitly rather than reusing the harness or soak.

Every addition below wraps these; nothing forks them.

## Design rules

1. **One isolation story.** Every new mode boots through `prepareHarness`
   and `newIsolatedProviderApp`. A new spawn path (textgen one-shot,
   probes) resolves to the mock like every other spawn. No exceptions,
   no three-of-four.
2. **Transport boundary stays clean.** All new control and inspection
   flows are Harness RPCs + event channels on the existing wire. The
   frontend bridge answers over the same WS; no side channel.
3. **The harness never fabricates app state through side doors**,
   unchanged from agent-harness.md. New capabilities wrap production
   paths.
4. **Evidence is text.** Semantic snapshots, metric series, JSONL logs.
   No screenshot tooling.
5. **Worktree-concurrent by construction.** Any two instances from any
   two checkouts run beside each other and beside the developer's real
   app: per-data-root ports, no shared singletons, isolated webview
   storage.

## 1. Windowed harness mode (linux, macOS; Windows keeps the launcher)

New flag `--window`, valid only with `--harness` or `--soak`, only in a
GUI (`!nogui`) build (`nogui` build: fatal with a message naming
`make harness-build`). It boots the harness/soak backend exactly as
today (same `prepareHarness`, same Harness receiver, same bootstrap
line on stdout), then opens the real Wails webview window pointed at
`srv.AppURL()` (plus `&cid=`), the same shell `runDesktop` builds.

Windowed-harness specifics, versus `runDesktop`:

- **No single-instance registration.** Isolated instances are
  collision-free by construction; N worktrees may each hold one. The
  ordinary desktop boot keeps its dev/prod single-instance ID.
- **Window title**: `Agent Overflow (harness · <id>)` /
  `(soak · <id>)`, where `<id>` is the instance id (short hash of the
  data root, see §2). Humans tell windows apart; agents use the registry.
- **Webview storage isolation (linux)**: before `application.New()`, set
  `XDG_DATA_HOME`, `XDG_CACHE_HOME`, `XDG_CONFIG_HOME` to
  `<dataRoot>/home/xdg/{data,cache,config}`. Verified by spike
  2026-08-26: WebKitGTK's default network session then keeps cookies /
  localStorage / IndexedDB / shader caches entirely under the data root.
  Set AFTER `prepareHarness` (its refusals must compare against the real
  config root) and before any GLib call. `AO_HARNESS_KEEP_HOME` does not
  opt out of this. Webview storage is never shared.
- **Webview storage isolation (macOS)**: WKWebView's default data store
  is keyed by bundle identity, not `$HOME`; a raw dev binary would share
  storage across instances. `ao-darwin-harness` creates a nonce-qualified
  bundle id for each run and verifies it before Wails starts. After the
  supervised process exits it removes that exact generated bundle and its
  bundle-id-scoped `~/Library/WebKit`, cache, HTTP storage, saved-state, and
  preference paths. Cleanup validates the harness prefix plus the agreement
  between app filename and Info.plist before touching user-Library state.
- **Window geometry** persists to the instance's own settings.json (the
  data-dir override already routes this correctly).
- Soak's autopilot (`armSoakSteadyState`) runs identically under
  `--soak --window`; `soakDefaultDataRoot()` stays the default only for
  the launcher-driven Windows path. A native `--soak --window` without
  `--data-dir` derives the per-worktree default (§2) instead, so two
  native soaks never collide.

`make harness-window` and `make soak-window` build (harness-build) and
launch with the per-worktree data dir through `ao-harness up --window`.
The make target waits while the supervised instance is live and traps
Ctrl-C or a closed window into `ao-harness down`, so it retains the old
foreground workflow without bypassing containment. On macOS,
`ao-darwin-harness` creates the unique bundle required by WKWebView, then
exec-disclaims the backend from the launching app's macOS responsibility chain
and invokes the same supervised `up --window` path. The disclaimer lets memory
accounting include this run's launchd-parented WebKit helpers without charging
the Agent Overflow/Codex process that started the harness. WSLg renders Linux windows
on the Windows desktop, so the mode is live-testable in the WSL dev
environment.

## 2. Instances, worktrees, the registry

**Instance id** = first 8 hex chars of SHA-256 of the canonical absolute
data root. Stable across restarts, unique per data root.

**Per-worktree default data root** (generalizing the Makefile's
`HARNESS_DATA_DIR` pattern): `os.TempDir()/agent-overflow-harness<checkout-path-with-slashes-as-dashes>`.
The Make targets keep computing it; `ao-harness up` computes the same
value in Go so both agree.

**Registry.** Every `--harness`/`--soak` boot writes
`<registryDir>/<instance-id>.json` after `MarkReady`:

```json
{"id":"…","pid":123,"mode":"harness|soak|perf","window":true,
 "port":1234,"dataRoot":"…","dataDir":"…","worktree":"<cwd at boot>",
 "version":"…","startedAt":"RFC3339"}
```

`registryDir` = `os.UserCacheDir()/agent-overflow/harness-instances`
(created 0700). The token is deliberately NOT in the registry; it lives
in `<dataDir>/harness-instance.json` (0600), the full bootstrap payload
written at the same moment, which is also what lets any tool attach to a
running instance without having parsed its stdout. Both files are
removed on graceful shutdown; readers treat a dead `pid` as stale and
delete the entry. Registry rows are discovery only. Attaching always
reads the token from the data dir, so a foreign registry row can at
worst point at a path the reader must be able to open.

## 3. The `ao-harness` CLI (cmd/ao-harness)

A standalone Go binary (built by `harness-build`, landing in `bin/`)
that gives agents a first-class Bash surface over everything the TS
client does and more. It is a pure WS/HTTP client plus process
supervisor. It links no App code.

Instance selection for every command: `--instance <id|idPrefix|dataRoot>`, else
exactly one running instance, else the current worktree's default data
root. Ambiguity is an error listing candidates, never a guess.

Command sheet (design intent; `-o json` on every read command). The
shipped surface of record is `ao-harness --help` and
`cmd/ao-harness/AGENTS.md`, which carry flags this sheet omits:

| Command | Behavior |
|---|---|
| `up [--window] [--soak] [--data-dir D] [--binary B] [--mock-provider M] [--dev-assets URL] [--keep-home] [--memory-limit-bytes N]` | Spawn detached, capture stderr to `<dataDir>/logs/backend-stderr.log`, wait for bootstrap, print instance summary. The default memory ceiling is 2 GiB. Refuses a second instance on the same data root. |
| `down [--all]` / `list` / `info` | SIGTERM (escalate KILL after 5s) / registry listing with liveness / `HarnessInfo` + paths + URL. |
| `rpc <Method> [json-arg…]` | Generic named RPC (App + Harness methods). Args are raw JSON values. |
| `seed [-f spec.json\|-]` | `HarnessSeed`. |
| `reset` | `HarnessReset`. |
| `scenario set\|list\|clear` | Scenario rules. `set` takes `--name lib-name` or `-f file.json`, `--cwd`, `--session-ref`. |
| `scenario validate <file…>` | Offline: parse + pacing rules + fixture-path existence. No instance needed. Used by tests and authors. |
| `mock list\|advance\|emit\|exit` | `HarnessListMocks` / `HarnessMockCommand`. |
| `threads` / `items --thread T [--turn N]` | Thread rows (harness escape hatch) / items via App RPCs. |
| `send --thread T <text…>` | `SendMessage`. |
| `events tail [--channel C…]` / `events await --channel C [--where path=value] [--timeout 15s]` / `events count` | Live WS subscribe with ring replay; await consumes matches exactly like the TS client so two awaits see two occurrences. Tail defaults to 1,000 records on stdout. `--file` captures without an event-count cap, bounded by `--max-bytes` or `--timeout`, and reports when the capture is incomplete. |
| `record start\|stop`, `bundles`, `replay bundle\|file\|pause\|resume\|step\|stop\|status` | Bundle workflow. |
| `logs backend\|frontend-errors\|ui-trace [-f] [-n N]` | Tails the evidence files `HarnessInfo` names (backend = the stderr capture from `up`). |
| `db <SELECT…>` | Read-only SQL against the instance DB (`mode=ro` open of the DB file; statement must be a single SELECT/PRAGMA, and anything else is refused). The ad-hoc assertion escape hatch. |
| `ui snapshot [--pane P]` / `ui diff` / `ui query <selector>` / `ui state <name>` | Frontend bridge (§4). `diff` compares against the previous snapshot taken by this CLI for the instance. `--json` is an alias for `-o json`; query output is bounded unless `--full` or `--file` is supplied. |
| `perf start\|stop\|status [--json]` / `perf watch [--json]` | Perf meters (§5). `--json` is an alias for `-o json`; watch emits NDJSON. |
| `bench <workload> [--repeat N] [--duration D] [--baseline file] [--json]` | Attach to the selected borrowed instance's open frontend, reset it, seed + run a bench workload, collect a perf report, print/compare (§5). `--duration` applies to sustained workloads. A headless instance is refused before reset. Use `run --plan` for fresh ownership. |
| `monitor list|start|heartbeat|overlap|status|collect|stop|cleanup|last` | List or operate the typed app-feel monitor catalog exposed by one exact attached frontend page. `status` collects a live snapshot without stopping it. `overlap` records concurrent runs. `cleanup` safely stops one named run and retains its result. |
| `run --plan <file\|->` | Execute a strict managed workload plan with ownership, safety ceilings, and structured partial reports. Fresh plans require an absent or empty root. Copyable plans are in `cmd/ao-harness/AGENTS.md`. |
| `compare prepare\|run` | Build or execute a portable offline A/B comparison capsule. |
| `postmortem --root <root>` | Inspect stopped-run evidence offline without attaching to a live instance. |
| `profile --thread T --scenario N [--cdp E]` | One scripted turn under the V8 sampling profiler; writes a `.cpuprofile` and splits sampled time into Svelte flush execution / write-side marking / other. Chromium-only (see below). |
| `artifacts list\|pin\|unpin\|clean [--dry-run]` | Host-global failed-run quarantine retention. Cleanup verifies ownership and manifest identity, and never removes active, leased, pinned, borrowed, or real-app roots. |
| `health [--watch]` | One-shot or continuous rollup (§6). `-o json --watch` emits one compact NDJSON record per check, including a machine-readable error record when the instance is unavailable. |
| `open` | Print the URL (and `xdg-open` it with `--browser`). |

### Memory containment

The public E2E gate runs through the fixed-purpose `ao-harness-e2e` launcher.
It starts `pnpm exec playwright test` inside one boundary, so the test runner,
browser processes, harness backends, mock providers, and their descendants
share the same limit. `pnpm test` invokes this launcher through `go run` when
the standalone binary is not present. The two-worker gate reserves 6 GiB by
default and accepts `--memory-limit-bytes` for machine-specific runs. Direct
Windows use of `launchHarness` refuses unless this launcher marker is present.

Every `up`, managed `run`, and compare leg installs the platform memory policy
before starting the backend. `up` defaults to 2 GiB and managed plans may
choose a lower or higher limit within host capacity. The ceiling was raised
from 600 MiB when the in-app browser made a full managed Chrome a legitimate
app-owned child; 1 GiB and 1.5 GiB trials also killed the browser-companion
E2E during that Chrome's measured 1.69 GB aggregate-RSS macOS startup peak.
Managed Chrome is gone (the harness now boots the fake browser engine and
spawns no browser at all), but the bound still covers the backend, mock
providers, webview children, and profilers that remain in the backend's
process tree. Worktree reservations use the same 2 GiB claim, so
parallel harnesses cannot overcommit the host's available-memory floor.

- Linux uses a private cgroup v2 with `memory.max=2 GiB`,
  `memory.swap.max=0`, and `memory.oom.group=1`. The child enters it through
  `SysProcAttr.UseCgroupFD` before exec. If the host exposes a read-only or
  non-delegated hierarchy, the launcher falls back to inherited
  `RLIMIT_DATA`, writes `harness-containment.json`, and keeps the watchdog
  active. It never silently runs without a limit.
- Windows uses a Job Object with `JOB_OBJECT_LIMIT_JOB_MEMORY` for each
  supervised native launcher tree. The launcher itself is assigned before
  WebView2 is created, so the browser, GPU, and Windows-side bridge share the
  boundary. WSL is a separate kernel namespace, so the Linux backend also
  starts behind an inherited `RLIMIT_DATA` and an exact `/proc` identity
  watchdog that sums its descendants. The watchdog fails closed on an
  identity change or an unreadable sample. The WSL path writes the same
  `logs/harness-containment.json` evidence record as other supervised
  launches. Detached `up` keeps its boundary alive through the
  launcher/watchdog state rather than relying on a caller's process lifetime.
- macOS current kernels reject lowering `RLIMIT_DATA`, `RLIMIT_RSS`, and
  `RLIMIT_AS`. The 100ms native application-responsibility ceiling and host
  available-memory watchdog are therefore the enforceable boundary. The
  responsibility join includes WebKit renderer/network/GPU XPC services even after
  launchd reparents them. macOS cannot promise a pre-allocation kernel cap, so
  reports retain that limitation and containment evidence says
  `watchdog-only-darwin`.

The governor remains the cross-platform diagnostic and host-floor backstop.
Kernel containment is the primary OOM protection where the OS exposes it;
macOS necessarily uses the reactive native responsibility sampler.

Detached `up` starts a separate watchdog that owns no application state. It
samples the authenticated backend's exact birth identity and process tree
every 100ms, protects a 2 GiB host available-memory floor, writes
`harness-watchdog.json` on a trip, calls `HarnessShutdown`, and verifies the
backend exited before using the identity-checked destructive fallback. The
watchdog is not a PID-only killer.

`profile` and `bench --trace` are the only two verbs here that do not go
through the bridge: a CPU profile and a timeline trace are Chromium
instruments, spoken over the DevTools protocol (`internal/cdpclient`)
against an endpoint named by `--cdp` / `$AO_CDP_URL` / `$AO_CDP_PORT`. A
WebKitGTK window serves none, and both refuse with that stated rather
than timing out.

Before profiling, `ao-harness` attests the active boundary. Detached local
instances must still have the verified `harness-watchdog-state.json`. A
launcher-hosted WSL instance must have a schema-valid
`harness-containment.json` whose Linux PID, memory limit, mode, data root,
and complete Windows launcher identity match the authenticated bootstrap and
registry row. Stale or cross-instance evidence is refused.

The authenticated WebSocket is authoritative for an attach. This lets a native
Windows CLI drive a launcher-hosted WSL backend through a `\\wsl.localhost`
data-root path even though Windows cannot validate the Linux PID. Lifecycle
commands that signal a process keep same-namespace PID validation.

The CLI's WS client implements the full frame contract
(rpc/event/batch/replay/subscribe/ping) per internal/transport/AGENTS.md.
It is the Go twin of `e2e/src/harness.ts`, kept in `internal/harnessclient`
so future Go tests can reuse it.

## 4. Frontend bridge: seeing without screenshots

`/bootstrap.json` gains `"harness": true` in harness/soak modes. When
set, the SPA dynamically imports `lib/harness/bridge.ts` (zero cost in
ordinary boots; ships in every bundle so a production-embedded harness
build needs no frontend rebuild).

Protocol (request/reply over the existing wire):

1. Backend `HarnessUIQuery(spec)` (Harness receiver, LocalOnly) assigns
   an id, emits `harness:ui-query {id, spec}`, and waits (10s timeout,
   with the error naming "no frontend attached or bridge inactive").
2. The bridge computes the answer and calls `HarnessUIQueryReply(id,
   result)`, also a Harness method, so the reply path exists only where
   the query path does.
3. Multiple attached frontends: first reply wins; the backend drops
   late replies by id.

Query kinds (the spec is a tagged union, versioned `v:1`):

- `viewport` is the **semantic snapshot**: for each visible pane, the
  ordered visible timeline rows with `{itemId, kind, role, streaming,
  rect, textHead (first ~120 chars), spinner/badge state}`, plus scroll
  position, sticky state, open overlays/popups (name + rect), active
  thread id, and a `settled` bool (no rAF-observed mutation in the last
  300ms). Stable field order, text-first, built to be diffed and read
  in a terminal, not parsed from pixels.
- `element`: CSS selector → `{count, first: {rect, visible, clipped,
  textContent (capped), aria}}`.
- `globals`: whitelisted read of the existing diagnostic globals
  (`__aoMemoryReport`, `__paneGeometry`, `__agentOverflowTimelineMemoryStats`,
  `__stickState`, ui-trace `recent(n)`). The whitelist lives in the
  bridge; unknown names error.
- `perf`: meter control (§5).

The bridge is also the place future inspection kinds land (a11y tree
walk, computed-style probes); one union, one version field.

## 5. Performance and FPS as first-class assertions

**In-page meters** (bridge-owned, WebKit + Chromium both):

- Frame cadence: rAF-delta collector → fps series + frame-time
  histogram (p50/p95/p99/max, long-frame count over threshold).
- Main-thread busy time: rAF-callback entry → a task posted on one reused
  MessageChannel → busy histogram (p50/p95/max/mean) plus the share of
  ticks fitting each of `budgetsMs` (default 6/8/16). The frame cadence
  above cannot answer a budget question (a vsync-locked compositor reads
  ~16.7ms for a 3ms tick and a 9ms one alike), and LoAF only reports past
  50ms.
- `PerformanceObserver`: `longtask` + `long-animation-frame` (where
  supported: Chromium; WebKit degrades to longtask/rAF), `layout-shift`,
  `event` (input latency).
- Memory/DOM: JS heap (`performance.memory` where present), DOM node
  count, listener-ish proxies (row count per pane, detached-pane stats
  via the existing timeline memory diagnostics).

`HarnessPerfStart({sampleMs, meters, budgetsMs})` arms them; samples stream on
`harness:perf`; `HarnessPerfStop()` returns the summary document.
**Backend-side samples** ride the same report: Go runtime heap/goroutine
counts, plus backend and webview RSS on Linux and macOS. Linux walks `/proc`;
macOS reads libproc and joins the app's responsible-process set so
launchd-parented WebKit helpers remain attributable. Renderer RSS is the
number Task-Manager-style questions are actually about.

**Bench workloads** are ordinary scenario-library entries plus seed
specs, named `bench-*`: `bench-burst-stream` (delta flood at
chunk-stress pacing), `bench-giant-turn` (one turn, thousands of items),
`bench-active-stream` (paced rich Markdown plus a normal tool pause),
`bench-many-threads` (wide sidebar + switch storm driven via RPC),
`bench-subagent-fanout` (the soak shape, bounded). `ao-harness bench`
boots-or-attaches, seeds, arms meters, runs the workload to its
completion signal, and writes `<dataDir>/bench/<workload>-<ts>.json` +
a terminal summary. `--baseline` compares against a checked-in or local
JSON with per-metric tolerances. It reports drift, never a hard gate by
default (machine variance is real; a gate is a per-repo choice later).

**CDP stays the deep path** where Chromium exists (headless e2e,
WebView2): scripts/perfprobe is untouched and the harness's CDP port
story on Windows is unchanged. The bridge meters are the portable
baseline; perfprobe is the allocator-level microscope.

## 6. Health: the generalized soak-check

`ao-harness health` rolls up, per instance: process liveness + uptime,
frontend-errors.jsonl count (new-since-last-check), oracle triggers in
the ui trace, backend-stderr error/warn scan, RSS of the process tree,
DB size, mock liveness (`HarnessListMocks` vs control reports), replay
state, and, when meters are armed, the live fps/long-frame counters.
`--watch` re-renders on an interval; exit code reflects "anything red".
On Windows, launcher-log concerns (watchdog episodes) remain
`make soak-check`'s job; `health` covers everything visible from the
backend side. `kill -USR1` (goroutine dump) and the pprof listener
(`AGENT_OVERFLOW_PPROF=1`) are documented knobs `up` can arm.

## 7. Mock/provider surface growth

- **One-shot textgen**: ao-mockprovider learns the `claude -p
  --output-format json --json-schema …` and `codex exec --ephemeral`
  argv shapes and answers schema-valid canned output (control-channel
  scriptable later if a test needs specific titles). Closes the "harness
  titles come from an unmocked spawn shape" gap; `resolveTextGeneration`
  needs no change since the binary already points at the mock.
- **Usage-limit / 429 injection**: library scenarios that emit each
  provider's real usage-limit wire shape mid-turn
  (`usage-limit-claude`, `usage-limit-codex`), written against the
  parsers like every other library entry, so `usagebackoff`/status
  surfaces are drivable end-to-end. Wire-first; a Harness RPC wrapping a
  production path is the fallback only if the wire alone cannot reach
  the state.
- **Per-session scenario scoping**: `HarnessSetScenario` gains an
  optional `sessionRef` scope matched against the mock registration's
  `ResumeRef`. Two threads in one workspace can run different scripts.
  Specificity order: sessionRef > cwd > provider-wide.
- **Claude probe account models**: the mock probe's account payload
  gains a `models` array so the catalog-merge path runs against real
  merge logic under harness.

## 8. e2e and gates

- New specs: bridge queries (snapshot/element/globals), perf
  start/stop + summary shape, registry/instance files, CLI smoke
  (spawn `ao-harness` against the worker's backend), textgen one-shot,
  usage-limit scenarios, per-session scoping.
- Windowed boots are NOT in `make e2e` (CI/headless machines have no
  display); a `windowed.manual.spec` + `make harness-window` cover them
  where a display exists.
- `make e2e` stays required when touching harness, transport, mock, or
  provider parsing. `ao-harness` gets Go unit tests (client frames,
  registry, db guard, scenario validate) under `make go-test`.

## Deliberate non-goals

- **App-wide fake clock.** Scenario pacing + scaled reliability
  profiles cover the need; injecting a clock through every subsystem
  contradicts the triage-and-pipe principle and would diverge from
  production timing behavior. Revisit only with a concrete test that
  cannot be written otherwise.
- **Screenshot/pixel tooling.** Semantic snapshots are the contract.
- **CI wiring.** There is no test CI today; adding it is its own
  conversation.

## Implementation waves

- **W1 (windowed mode + instances)**: `--window`, XDG isolation, title,
  no-single-instance, native soak default data root, registry +
  `harness-instance.json`, `/bootstrap.json` harness flag, Make targets.
- **W2 (CLI)**: `internal/harnessclient` + `cmd/ao-harness` (everything
  in §3 except ui/perf/bench/health verbs), Go tests.
- **W3 (bridge + perf)**: `lib/harness/bridge.ts`, `HarnessUIQuery`/
  `Reply`, `HarnessPerf*`, `harness:ui-query`/`harness:perf` channels,
  CLI ui/perf verbs, e2e specs.
- **W4 (mock growth)**: textgen one-shot, usage-limit scenarios,
  per-session scoping, probe models, e2e specs.
- **W5 (bench + health)**: bench workloads + runner + reports, health
  rollup, CLI verbs, e2e/unit coverage.
- **W6 (docs)**: agent-harness.md absorbs this surface, soak-rig.md
  narrows to the Windows launcher shell, AGENTS.md pointers, glossary.

All waves landed 2026-08-26 (W1+W4 `da4f3002`, W2+W3 `bfb6f1f1`,
W5 `e33e346b`, W6 docs). `docs/architecture/agent-harness.md` is the
living description of the built surface; this spec stays as the design
rationale and contract reference.
