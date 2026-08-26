# The testing harness

One harness, many shells. This spec grows the existing agent test harness
(docs/architecture/agent-harness.md) into the project's general testing
rig: the place agents validate their own changes — functional, visual,
and performance — against the real app with mocked providers, on any
platform, from any worktree, without screenshots and without a real LLM.

Status: spec + build plan. Sections marked **[built]** are done;
everything else is contract for the implementation waves at the bottom.

## What already exists (do not rebuild)

- **Isolation core**: `prepareHarness` + `newIsolatedProviderApp`
  (main_harness.go) — data-dir refusals, HOME redirect, the four provider
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

Every addition below wraps these; nothing forks them.

## Design rules

1. **One isolation story.** Every new mode boots through `prepareHarness`
   and `newIsolatedProviderApp`. A new spawn path (textgen one-shot,
   probes) resolves to the mock like every other spawn. No exceptions,
   no three-of-four.
2. **Transport boundary stays clean.** All new control and inspection
   flows are Harness RPCs + event channels on the existing wire. The
   frontend bridge answers over the same WS; no side channel.
3. **The harness never fabricates app state through side doors** —
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
today — same `prepareHarness`, same Harness receiver, same bootstrap
line on stdout — then opens the real Wails webview window pointed at
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
  opt out of this — webview storage is never shared.
- **Webview storage isolation (macOS)**: WKWebView's default data store
  is keyed by bundle identity, not `$HOME`; a dev binary shares
  storage across instances. Needs verification on a mac; until then the
  windowed mac harness carries a boot log line naming the limitation.
  Candidate fix if verification fails: fork patch adding
  `WKWebsiteDataStore dataStoreForIdentifier:` wiring (the fork already
  carries the Windows `WebviewUserDataPath` analog). Compile-correct
  code now, behavioral verification deferred to a mac.
- **Window geometry** persists to the instance's own settings.json (the
  data-dir override already routes this correctly).
- Soak's autopilot (`armSoakSteadyState`) runs identically under
  `--soak --window`; `soakDefaultDataRoot()` stays the default only for
  the launcher-driven Windows path — a native `--soak --window` without
  `--data-dir` derives the per-worktree default (§2) instead, so two
  native soaks never collide.

`make harness-window` and `make soak-window` build (harness-build) and
launch with the per-worktree data dir. WSLg renders these windows on the
Windows desktop, so the mode is live-testable in the WSL dev
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
{"id":"…","pid":123,"mode":"harness|soak","window":true,
 "port":1234,"dataRoot":"…","dataDir":"…","worktree":"<cwd at boot>",
 "version":"…","startedAt":"RFC3339"}
```

`registryDir` = `os.UserCacheDir()/agent-overflow/harness-instances`
(created 0700). The token is deliberately NOT in the registry; it lives
in `<dataDir>/harness-instance.json` (0600) — the full bootstrap payload
written at the same moment, which is also what lets any tool attach to a
running instance without having parsed its stdout. Both files are
removed on graceful shutdown; readers treat a dead `pid` as stale and
delete the entry. Registry rows are discovery only — attaching always
reads the token from the data dir, so a foreign registry row can at
worst point at a path the reader must be able to open.

## 3. The `ao-harness` CLI (cmd/ao-harness)

A standalone Go binary (built by `harness-build`, landing in `bin/`)
that gives agents a first-class Bash surface over everything the TS
client does and more. It is a pure WS/HTTP client plus process
supervisor — it links no App code.

Instance selection for every command: `--instance <id|dataRoot>`, else
exactly one running instance, else the current worktree's default data
root. Ambiguity is an error listing candidates, never a guess.

Command sheet (design intent; `-o json` on every read command — the
shipped surface of record is `ao-harness --help` and
`cmd/ao-harness/AGENTS.md`, which carry flags this sheet omits):

| Command | Behavior |
|---|---|
| `up [--window] [--soak] [--data-dir D] [--binary B] [--mock-provider M] [--dev-assets URL] [--keep-home]` | Spawn detached, capture stderr to `<dataDir>/logs/backend-stderr.log`, wait for bootstrap, print instance summary. Refuses a second instance on the same data root. |
| `down [--all]` / `list` / `info` | SIGTERM (escalate KILL after 5s) / registry listing with liveness / `HarnessInfo` + paths + URL. |
| `rpc <Method> [json-arg…]` | Generic named RPC (App + Harness methods). Args are raw JSON values. |
| `seed [-f spec.json\|-]` | `HarnessSeed`. |
| `reset` | `HarnessReset`. |
| `scenario set\|list\|clear` | Scenario rules. `set` takes `--name lib-name` or `-f file.json`, `--cwd`, `--session-ref`. |
| `scenario validate <file…>` | Offline: parse + pacing rules + fixture-path existence. No instance needed. Used by tests and authors. |
| `mock list\|advance\|emit\|exit` | `HarnessListMocks` / `HarnessMockCommand`. |
| `threads` / `items --thread T [--turn N]` | Thread rows (harness escape hatch) / items via App RPCs. |
| `send --thread T <text…>` | `SendMessage`. |
| `events tail [--channel C…]` / `events await --channel C [--where path=value] [--timeout 15s]` / `events count` | Live WS subscribe with ring replay; await consumes matches exactly like the TS client so two awaits see two occurrences. |
| `record start\|stop`, `bundles`, `replay bundle\|file\|pause\|resume\|step\|stop\|status` | Bundle workflow. |
| `logs backend\|frontend-errors\|ui-trace [-f] [-n N]` | Tails the evidence files `HarnessInfo` names (backend = the stderr capture from `up`). |
| `db <SELECT…>` | Read-only SQL against the instance DB (`mode=ro` open of the DB file; statement must be a single SELECT/PRAGMA — anything else refused). The ad-hoc assertion escape hatch. |
| `ui snapshot [--pane P]` / `ui diff` / `ui query <selector>` / `ui state <name>` | Frontend bridge (§4). `diff` compares against the previous snapshot taken by this CLI for the instance. |
| `perf start\|stop\|status [--json]` / `perf watch` | Perf meters (§5). |
| `bench <workload> [--repeat N] [--baseline file]` | Seed + run a bench workload, collect a perf report, print/compare (§5). |
| `profile --thread T --scenario N [--cdp E]` | One scripted turn under the V8 sampling profiler; writes a `.cpuprofile` and splits sampled time into Svelte flush execution / write-side marking / other. Chromium-only (see below). |
| `health [--watch]` | One-shot or continuous rollup (§6). |
| `open` | Print the URL (and `xdg-open` it with `--browser`). |

`profile` and `bench --trace` are the only two verbs here that do not go
through the bridge: a CPU profile and a timeline trace are Chromium
instruments, spoken over the DevTools protocol (`internal/cdpclient`)
against an endpoint named by `--cdp` / `$AO_CDP_URL` / `$AO_CDP_PORT`. A
WebKitGTK window serves none, and both refuse with that stated rather
than timing out.

The CLI's WS client implements the full frame contract
(rpc/event/batch/replay/subscribe/ping) per internal/transport/AGENTS.md
— the Go twin of `e2e/src/harness.ts`, kept in `internal/harnessclient`
so future Go tests can reuse it.

## 4. Frontend bridge: seeing without screenshots

`/bootstrap.json` gains `"harness": true` in harness/soak modes. When
set, the SPA dynamically imports `lib/harness/bridge.ts` (zero cost in
ordinary boots; ships in every bundle so a production-embedded harness
build needs no frontend rebuild).

Protocol — request/reply over the existing wire:

1. Backend `HarnessUIQuery(spec)` (Harness receiver, LocalOnly) assigns
   an id, emits `harness:ui-query {id, spec}`, and waits (10s timeout —
   the error names "no frontend attached or bridge inactive").
2. The bridge computes the answer and calls `HarnessUIQueryReply(id,
   result)` — also a Harness method, so the reply path exists only where
   the query path does.
3. Multiple attached frontends: first reply wins; the backend drops
   late replies by id.

Query kinds (the spec is a tagged union, versioned `v:1`):

- `viewport` — the **semantic snapshot**: for each visible pane, the
  ordered visible timeline rows with `{itemId, kind, role, streaming,
  rect, textHead (first ~120 chars), spinner/badge state}`, plus scroll
  position, sticky state, open overlays/popups (name + rect), active
  thread id, and a `settled` bool (no rAF-observed mutation in the last
  300ms). Stable field order, text-first — built to be diffed and read
  in a terminal, not parsed from pixels.
- `element` — CSS selector → `{count, first: {rect, visible, clipped,
  textContent (capped), aria}}`.
- `globals` — whitelisted read of the existing diagnostic globals
  (`__aoMemoryReport`, `__paneGeometry`, `__agentOverflowTimelineMemoryStats`,
  `__stickState`, ui-trace `recent(n)`). The whitelist lives in the
  bridge; unknown names error.
- `perf` — meter control (§5).

The bridge is also the place future inspection kinds land (a11y tree
walk, computed-style probes); one union, one version field.

## 5. Performance and FPS as first-class assertions

**In-page meters** (bridge-owned, WebKit + Chromium both):

- Frame cadence: rAF-delta collector → fps series + frame-time
  histogram (p50/p95/p99/max, long-frame count over threshold).
- Main-thread busy time: rAF-callback entry → a task posted on one reused
  MessageChannel → busy histogram (p50/p95/max/mean) plus the share of
  ticks fitting each of `budgetsMs` (default 6/8/16). The frame cadence
  above cannot answer a budget question — a vsync-locked compositor reads
  ~16.7ms for a 3ms tick and a 9ms one alike — and LoAF only reports past
  50ms.
- `PerformanceObserver`: `longtask` + `long-animation-frame` (where
  supported — Chromium; WebKit degrades to longtask/rAF), `layout-shift`,
  `event` (input latency).
- Memory/DOM: JS heap (`performance.memory` where present), DOM node
  count, listener-ish proxies (row count per pane, detached-pane stats
  via the existing timeline memory diagnostics).

`HarnessPerfStart({sampleMs, meters, budgetsMs})` arms them; samples stream on
`harness:perf`; `HarnessPerfStop()` returns the summary document.
**Backend-side samples** ride the same report: Go runtime heap/goroutine
counts, and on linux the RSS of the backend + webview child processes
(`/proc` walk from the window process tree) — the WebKitWebProcess RSS
is the number Task-Manager-style questions are actually about.

**Bench workloads** are ordinary scenario-library entries plus seed
specs, named `bench-*`: `bench-burst-stream` (delta flood at
chunk-stress pacing), `bench-giant-turn` (one turn, thousands of items),
`bench-many-threads` (wide sidebar + switch storm driven via RPC),
`bench-subagent-fanout` (the soak shape, bounded). `ao-harness bench`
boots-or-attaches, seeds, arms meters, runs the workload to its
completion signal, and writes `<dataDir>/bench/<workload>-<ts>.json` +
a terminal summary. `--baseline` compares against a checked-in or local
JSON with per-metric tolerances — reports drift, never a hard gate by
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
state, and — when meters are armed — the live fps/long-frame counters.
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
  `ResumeRef` — two threads in one workspace can run different scripts.
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

- **W1 — windowed mode + instances**: `--window`, XDG isolation, title,
  no-single-instance, native soak default data root, registry +
  `harness-instance.json`, `/bootstrap.json` harness flag, Make targets.
- **W2 — CLI**: `internal/harnessclient` + `cmd/ao-harness` (everything
  in §3 except ui/perf/bench/health verbs), Go tests.
- **W3 — bridge + perf**: `lib/harness/bridge.ts`, `HarnessUIQuery`/
  `Reply`, `HarnessPerf*`, `harness:ui-query`/`harness:perf` channels,
  CLI ui/perf verbs, e2e specs.
- **W4 — mock growth**: textgen one-shot, usage-limit scenarios,
  per-session scoping, probe models, e2e specs.
- **W5 — bench + health**: bench workloads + runner + reports, health
  rollup, CLI verbs, e2e/unit coverage.
- **W6 — docs**: agent-harness.md absorbs this surface, soak-rig.md
  narrows to the Windows launcher shell, AGENTS.md pointers, glossary.

All waves landed 2026-08-26 (W1+W4 `da4f3002`, W2+W3 `bfb6f1f1`,
W5 `e33e346b`, W6 docs). `docs/architecture/agent-harness.md` is the
living description of the built surface; this spec stays as the design
rationale and contract reference.
