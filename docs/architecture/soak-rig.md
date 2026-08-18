# The soak rig

A **soak** is a second, fully isolated copy of the real desktop app —
real Windows launcher, real WebView2 window, real SPA, real Go backend
inside WSL — left visible and untouched for hours while background
activity streams over the WebSocket. It exists to reproduce the WebView2
renderer hang: the host-side watchdog reported *"renderer ran no script
for 20s"* on a window that was **visible on the main monitor, not
minimised, with no user input for ~9 minutes**, showing one idle thread
plus one thread with three Claude background subagents streaming.

The rig recreates exactly that steady state, and only that. It is not a
test: nothing asserts, nothing exits. You start it, you leave it, and
later you ask `make soak-check` what happened.

```
make soak         # build + launch the isolated instance (blocks; leave it running)
make soak-check   # read-only: uptime, stalls, recovery episodes, controller rebuilds
```

## What makes it safe to run beside your own app

Two independent isolations, one axis each.

### Provider isolation — it is the harness's, not a copy of it

The soak backend boots through **`prepareHarness`** and
**`newIsolatedProviderApp`** (`main_harness.go:118`), the same
constructor `--harness` uses. That function is the single place the four
pins are applied:

| pin | effect |
| --- | --- |
| `providerBinaryOverride` | every spawn resolves to `ao-mockprovider`, even after an `UpdateSettings` |
| `credentialHomeOverride` | account slots / prune / canonical credential stay under `<dataRoot>/home` |
| `fileKeychainOverride` | no OS keychain |
| `backgroundFetchDisabled` | no network on a run that lasts hours |

`TestMockedBootModesShareOneIsolationHelper` (`main_soak_test.go:84`)
scans the repo-root Go sources and fails if any of those four is
assigned outside `main_harness.go`. That is the enforcement the
CLAUDE.md invariant asks for: a future mocked boot mode gets all four or
it does not compile past the test. Three-of-four is the shape that
burned a real login (2026-07-29, 2026-08-03).

`--data-dir` defaults to `~/.agent-overflow-soak` (`main_soak.go:69`)
and is refused if it resolves to the OS config root or the real app data
dir, or if any of its directories is a symlink (`main_harness.go:215`,
`:230`). HOME and `%USERPROFILE%` point at `<dataRoot>/home`, so
`~/.claude` and `~/.codex` scans are soak-local.

**No real `claude`/`codex` binary is reachable from a soak instance, and
no real provider home is.**

### Instance isolation — one profile axis, not three flags

Everything the launcher owns per-instance derives from ONE value:
`--profile soak` (or `AGENT_OVERFLOW_PROFILE=soak`), parsed in
`cmd/agent-overflow-windows/flags.go:56` and folded with the build stamp
by `appidentity.LauncherMode` (`internal/appidentity/profile.go:50`) —
a soak launched from the dev build is `soak`, never `dev`.

| collision point | normal instance | soak instance | anchor |
| --- | --- | --- | --- |
| single-instance id | `com.agentoverflow.wsl[.dev]` | `com.agentoverflow.wsl.soak` | `internal/appidentity/singleinstance.go:26` |
| window title | `Agent Overflow[ (dev)]` | `Agent Overflow (soak)` | `internal/appidentity/singleinstance.go:10` |
| WebView2 user data | `webview2[-dev]` | `webview2-soak` | `internal/appidentity/profile.go:83` |
| launcher log | `launcher.log` | `launcher-soak.log` | `internal/appidentity/profile.go:70` |
| window placement | `window.json` | `window-soak.json` | `cmd/agent-overflow-windows/windowstate.go:31` |
| CDP port | dev `9223` | `9224` | `internal/appidentity/profile.go:100` |
| Wails log level | dev info / prod warn | **debug** | `cmd/agent-overflow-windows/main.go:1527` |
| backend data dir | real config root | `~/.agent-overflow-soak` | `main_soak.go:69` |
| `wsl.json` writes | persisted | **refused** | `cmd/agent-overflow-windows/main.go:1010` |

Why one axis and not three flags: a soak that shared *any single one* of
these reaches into the developer's live instance — a shared
single-instance id means the soak URL opens in **their** window; a
shared WebView2 dir means shared localStorage and the same IndexedDB
thread replica. `TestSoakProfileFoldsEveryPerInstanceName`
(`cmd/agent-overflow-windows/main_test.go`) asserts every row above
differs, and an unknown profile string is a hard error rather than a
silent fall back to the default instance
(`appidentity.NormalizeProfile`).

Debug is the soak's Wails log level because half the watchdog narrative
— *"render watchdog armed"*, *"standing down"*, *"render recovery
re-navigating"* — is logged at debug by the pinned wails fork. At the
default level an episode has a start line and no story.

## How the two halves meet

`make soak` and `make dev-wsl` are the same recipe (`launch-wsl`,
parameterised by `LAUNCH_PROFILE`): cross-compile the Linux backend and
the Windows `.exe`, stage the exe to a versioned `%LOCALAPPDATA%` path,
launch it through Windows. The soak leg adds two things:

1. `make mockprovider`, then `cp bin/ao-mockprovider ~/.local/bin/` —
   the WSL payload installs the backend at `~/.local/bin/agent-overflow`
   and `resolveMockProvider` looks *beside the running executable*
   (`main_harness.go:318`), so that is where the mock has to be.
2. `--profile soak` on the launcher's argv, which becomes `--soak` on
   the WSL backend's argv (`profileBackendArgs`,
   `cmd/agent-overflow-windows/main.go:728`). It rides argv rather than
   an env var deliberately — WSLENV passthrough is for diagnostics, and
   anything load-bearing across the WSL boundary belongs in explicit
   launch args.

`--data-dir` is deliberately *not* spelled by the launcher: it runs on
the Windows side and has no Linux path to offer, so the backend resolves
its own default.

### Why `--soak` and not `--harness`

Same isolation, different shell. `--harness` prints an `__AO_HARNESS__`
line and expects a browser to be pointed at it; the launcher can only
parse the ordinary `__AO_BOOTSTRAP__` `{port, token, clientId}` contract
(`internal/wsllauncher`). `--soak` is that contract plus harness
isolation plus an autopilot — nothing is duplicated: `runSoak`
(`main_soak.go:80`) calls the same `prepareHarness`,
`newIsolatedProviderApp`, `newHarness`, and registers the same `Harness`
RPC receiver, so a running soak is inspectable with the tools an agent
already has (`HarnessInfo` for evidence paths, replay capture).

The window opens at 800x600 (`soakWindowWidth`) because a soak sits on a
real monitor beside real work for hours; it only has to be big enough to
keep the renderer compositing a live thread.

## The scenario: three subagents, forever

At boot + 3s (`soakArmDelay`, enough for the frontend to attach so the
steady state includes streaming from the first frames rather than
replayed history), `armSoakSteadyState` seeds and arms:

- **Thread A — "Soak: idle thread"**: one completed short turn, then
  nothing. This is the idle half of the incident's window.
- **Thread B — "Soak: background agents"**: a live turn that launches
  three async `local_agent` subagents and never completes.

The script is the embedded library scenario
`internal/harness/scenario/library/soak-background-agents.json`:

1. The parent assistant message, then per agent *n* ∈ {1,2,3}: an
   `Agent` `tool_use`, a `system/task_started{task_type:"local_agent"}`,
   and the async `tool_result` ack (`isAsync`, `status:"async_launched"`,
   `agentId`) — the real Claude background-subagent shape.
2. An **unbounded `repeat`** whose body walks the three agents,
   `delayMs: 5000` apart, emitting a small burst each: subagent
   `message_start` → text deltas → a subagent tool_use/tool_result →
   `message_stop`, every line carrying top-level `parent_tool_use_id`,
   plus a `system/task_progress` sample. Ids are uniquified with
   `${ITER}`.

So the cadence is **one burst per subagent per ~15s**, ~5s apart from
each other — inside the brief's "a burst every 5–20s per subagent, small
payloads". The turn never emits `result`, so it stays live indefinitely
and the working indicator keeps animating; that is the closest match to
"activity streaming indefinitely", and it is the deliberate choice over
letting each turn end and restart.

**Tuning without a rebuild**: drop an edited copy of the scenario at
`~/.agent-overflow-soak/soak-scenario.json` and restart. It replaces the
embedded one and is validated at boot (`installSoakScenario`,
`main_soak.go:175`) — a bad edit fails in `launcher-soak.log`, not as
frames that never arrive.

### The `repeat` step

`repeat` (`internal/harness/scenario/scenario.go`) is a general scenario
step, not a soak special case: `{"repeat": {"count": 0, "steps": [...]}}`,
where `count <= 0` means forever. Two rules keep it from being a foot-gun:

- an unbounded repeat must contain a **pacing step** among its direct
  children (`delayMs > 0`, `stall`, `waitSignal`, `approval`, or an
  `emit` with `delayBetweenMs > 0`) — validation rejects a hot loop;
- the body runs **unreported**, so an infinite loop does not flood the
  mock control channel or the event bus with `step_started` reports.

An inbound interrupt aborts it at the next step boundary like any other
step. `${ITER}` (1-based) is available inside the body.

## Reading the results

`make soak-check` (`scripts/soak-check.sh`) is read-only. It resolves
`%APPDATA%\agent-overflow\launcher-soak.log` through cmd.exe interop,
scopes everything to the **current run** (the log is append-only across
launches; the `launcher: profile=soak` boot marker is the separator) and
reports:

- whether a `--soak` backend is alive in this distro (checked WSL-side
  via argv, so it can never be confused with your dev backend);
- start time and uptime;
- counts of `renderer ran no script`, `render recovery episode N
  started` / `closed`, `rebuilding controller` — and an explicit warning
  when an episode is open with no close, i.e. the renderer has not come
  back;
- the last dozen watchdog lines, plus the full-history grep one-liner
  (`make soak` prints the same one-liner up front).

Chromium's own log (`webview2-soak\EBWebView\chrome_debug.log`) is
opt-in as everywhere else: `AGENT_OVERFLOW_WEBVIEW_LOG=1 make soak`.
Note the WebView2 console-window caveat in
`cmd/agent-overflow-windows/AGENTS.md` — closing that console kills the
app, which would end the soak.

## Restarting a soak

`armSoakSteadyState` is idempotent. Fixtures are seeded only when the
store is empty and the live turn is re-armed on the *same* thread every
boot, so ten restarts still leave two threads. If the data dir holds
threads that the rig did not seed (you drove it by hand), it refuses to
arm rather than sending a prompt into your work — delete
`~/.agent-overflow-soak` to reseed.

## Related

- [agent-harness.md](agent-harness.md) — the mocking machinery the soak
  reuses wholesale, and the `Harness` RPC surface a running soak exposes.
- [`cmd/agent-overflow-windows/AGENTS.md`](../../cmd/agent-overflow-windows/AGENTS.md)
  — launcher internals, log locations, WebView2 profile pinning.
