# The soak rig

A **soak** is a second, fully isolated copy of the real desktop app
(real Windows launcher, real WebView2 window, real SPA, real Go backend
inside WSL) left visible and untouched for hours while background
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
make soak-contract # verify scrollTop quantization + compositor ownership in WebView2
```

A soak is a **preset**, not a mode. `make soak` is `make harness-wsl`
(the Windows launcher shell of the [agent test
harness](agent-harness.md)) plus `--autopilot`, the flag that arms the
scenario below. This document describes that preset on the Windows
shell, which is the one that reproduces the WebView2 renderer hang. The
same preset also runs behind a native Wails window (`make soak-window`,
i.e. `--soak --autopilot --window`) on linux/macOS, with its own
per-worktree data root and instance registry row. See
[agent-harness.md § Windowed mode](agent-harness.md#windowed-mode---window)
and `docs/specs/testing-harness.md`. Everything below about provider
isolation applies to both; the profile/launcher table is
Windows-specific.

## What makes it safe to run beside your own app

Two independent isolations, one axis each.

Two measurement caveats before drawing renderer conclusions from a
Windows-shell run. First, `make soak` / `make harness-wsl` build the SPA
with `--minify false` (the `launch-wsl` recipe is the dev-wsl one), so
the renderer executes the unminified bundle (identifier names retained,
larger script text); `import.meta.env.DEV` gates stay off, so behavior is
production, but byte-for-byte memory numbers are not. Second, if
`FRONTEND_DEVSERVER_URL` is set in the launching shell, the backend
proxies the Vite dev server (HMR WebSocket included) instead of serving
the embedded bundle, a different renderer workload entirely; the boot
log announces it loudly. Unset it for any run whose numbers matter.


### Provider isolation: it is the harness's, not a copy of it

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

### Instance isolation: one profile axis, not three flags

Everything the launcher owns per-instance derives from ONE value:
`--profile soak` (or `AGENT_OVERFLOW_PROFILE=soak`), parsed in
`cmd/agent-overflow-windows/flags.go:56` and folded with the build stamp
by `appidentity.LauncherMode` (`internal/appidentity/profile.go:50`).
A soak launched from the dev build is `soak`, never `dev`.

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
these reaches into the developer's live instance. A shared
single-instance id means the soak URL opens in **their** window; a
shared WebView2 dir means shared localStorage and the same IndexedDB
thread replica. The `harness` profile (`make harness-wsl`) is a third
point on the same axis, isolated from dev AND soak alike.
`TestIsolatedProfilesFoldEveryPerInstanceName`
(`cmd/agent-overflow-windows/main_test.go`) asserts every row above
differs across all three, and an unknown profile string is a hard error
rather than a silent fall back to the default instance
(`appidentity.NormalizeProfile`).

Debug is the soak's Wails log level because half the watchdog narrative
(*"render watchdog armed"*, *"standing down"*, *"render recovery
re-navigating"*) is logged at debug by the pinned wails fork. At the
default level an episode has a start line and no story.

## How the two halves meet

`make soak` and `make dev-wsl` are the same recipe (`launch-wsl`,
parameterised by `LAUNCH_PROFILE`): cross-compile the Linux backend and
the Windows `.exe`, stage the exe to a versioned `%LOCALAPPDATA%` path,
launch it through Windows. The soak leg adds two things:

1. `make mockprovider`, then `cp bin/ao-mockprovider ~/.local/bin/`.
   The WSL payload installs the backend at `~/.local/bin/agent-overflow`
   and `resolveMockProvider` looks *beside the running executable*
   (`main_harness.go:318`), so that is where the mock has to be.
2. `--profile soak` on the launcher's argv, which becomes
   `--soak --autopilot --launcher-pid <pid>` on the WSL backend's argv
   (`profileBackendArgs`, `cmd/agent-overflow-windows/main.go`). These
   ride argv rather than env vars deliberately: WSLENV passthrough is
   for diagnostics, and anything load-bearing across the WSL boundary
   belongs in explicit launch args.

`--data-dir` is deliberately *not* spelled by the launcher: it runs on
the Windows side and has no Linux path to offer, so the backend resolves
its own default.

### Why `--soak` and not `--harness`

Same isolation, different shell. `--harness` prints an `__AO_HARNESS__`
line and expects a browser to be pointed at it; the launcher can only
parse the ordinary `__AO_BOOTSTRAP__` `{port, token, pageUrl, clientId}`
contract
(`internal/wsllauncher`). `--soak` is that contract plus harness
isolation: the launcher-owned wire name for the launcher-shell
instance, historical and never typed by a user. What makes it a SOAK is
`--autopilot`, which arms the steady state below; without it the same
boot is the Windows harness, waiting to be driven. Nothing is
duplicated: `runSoak` calls the same `prepareHarness`,
`newIsolatedProviderApp`, `newHarness`, and registers the same `Harness`
RPC receiver, so a running soak is inspectable with the tools an agent
already has (`HarnessInfo` for evidence paths, replay capture).

The window opens at 800x600 (`soakWindowWidth`) because a soak sits on a
real monitor beside real work for hours; it only has to be big enough to
keep the renderer painting a live thread.

## The scenario: three subagents, forever

At boot + 3s (`soakArmDelay`, enough for the frontend to attach so the
steady state includes streaming from the first frames rather than
replayed history), `armSoakSteadyState` seeds and arms:

- **Thread A, "Soak: idle thread"**: one completed short turn, then
  nothing. This is the idle half of the incident's window.
- **Thread B, "Soak: background agents"**: a live turn that launches
  three async `local_agent` subagents and never completes.

The script is the embedded library scenario
`internal/harness/scenario/library/soak-background-agents.json`:

1. The parent assistant message, then per agent *n* ∈ {1,2,3}: an
   `Agent` `tool_use`, a `system/task_started{task_type:"local_agent"}`,
   and the async `tool_result` ack (`isAsync`, `status:"async_launched"`,
   `agentId`), the real Claude background-subagent shape.
2. An **unbounded `repeat`** whose body walks the three agents,
   `delayMs: 5000` apart, emitting a small burst each: subagent
   `message_start` → text deltas → a subagent tool_use/tool_result →
   `message_stop`, every line carrying top-level `parent_tool_use_id`,
   plus a `system/task_progress` sample. Ids are uniquified with
   `${ITER}`.

So the cadence is **one burst per subagent per ~15s**, ~5s apart from
each other, inside the brief's "a burst every 5–20s per subagent, small
payloads". The turn never emits `result`, so it stays live indefinitely
and the working indicator keeps animating; that is the closest match to
"activity streaming indefinitely", and it is the deliberate choice over
letting each turn end and restart.

**Tuning without a rebuild**: drop an edited copy of the scenario at
`~/.agent-overflow-soak/soak-scenario.json` and restart. It replaces the
embedded one and is validated at boot (`installSoakScenario`,
`main_soak.go:175`). A bad edit fails in `launcher-soak.log`, not as
frames that never arrive.

**Tool ids must be unique across boots.** The soak data dir persists,
and triage upserts tool rows by provider tool id, so a scenario whose
`tool_use` ids are deterministic (`tu-burn-edit-${TURN}-${ITER}`
restarting from 1) lands every "new" tool call on the PREVIOUS boot's
completed row in an old turn. The symptom is a timeline streaming prose
with no tool rows at all while `launcher-soak.log` fills with
`triage: dropping late EventToolComplete … already terminal`
(2026-08-25). Bake a per-deploy nonce into the ids (or delete
`~/.agent-overflow-soak` to reseed) before every scenario swap that
reuses an id scheme.

### The `repeat` step

`repeat` (`internal/harness/scenario/scenario.go`) is a general scenario
step, not a soak special case: `{"repeat": {"count": 0, "steps": [...]}}`,
where `count <= 0` means forever. Two rules keep it from being a foot-gun:

- an unbounded repeat must contain a **pacing step** among its direct
  children (`delayMs > 0`, `stall`, `waitSignal`, `approval`, or an
  `emit` with `delayBetweenMs > 0`), since validation rejects a hot loop;
- the body runs **unreported**, so an infinite loop does not flood the
  mock control channel or the event bus with `step_started` reports.

An inbound interrupt aborts it at the next step boundary like any other
step. `${ITER}` (1-based) is available inside the body.

## Reading the results

`ao-harness health --watch` works against any soak (it reads the
instance's own evidence files, not the Windows launcher log), and is the
only checker for a `make soak-window` instance. The Windows shell has
its launcher-side view too:

Teardown on this shell: `ao-harness down` stops the WSL backend, then
closes the launcher window too. The backend publishes the launcher's
Windows pid (`--launcher-pid`) in its discovery files, and `down`
taskkills it over WSL interop after confirming the pid's image name is
an agent-overflow launcher (`cmd/ao-harness/launcher_kill.go`). The
launcher still deliberately does not exit when its child **crashes**
(a launcher that vanished on backend death would take the window, and
its evidence, with it), so a crashed run's window stays up for autopsy
until you close it or run `down`.

`make soak-check` (`scripts/soak-check.sh`) is read-only. It resolves
`%APPDATA%\agent-overflow\launcher-soak.log` through cmd.exe interop,
scopes everything to the **current run** (the log is append-only across
launches; the `launcher: profile=soak` boot marker is the separator) and
reports:

- whether an `--autopilot` backend is alive in this distro (checked
  WSL-side via argv, since `--soak` alone would also match a harness-wsl
  instance, which is not a soak);
- start time and uptime;
- counts of `renderer ran no script`, `render recovery episode N
  started` / `closed`, `rebuilding controller`, plus an explicit warning
  when an episode is open with no close, i.e. the renderer has not come
  back;
- the last dozen watchdog lines, plus the full-history grep one-liner
  (`make soak` prints the same one-liner up front).

Chromium's own log (`webview2-soak\EBWebView\chrome_debug.log`) is
opt-in as everywhere else: `AGENT_OVERFLOW_WEBVIEW_LOG=1 make soak`.
Note the WebView2 console-window caveat in
`cmd/agent-overflow-windows/AGENTS.md`. Closing that console kills the
app, which would end the soak.

`make soak-contract` attaches to the soak WebView2 over its isolated CDP
port. It first mounts an invisible offscreen scroller, verifies whole-CSS-pixel
`scrollTop` readback, and removes the element. It then inspects the real mounted
timeline, virtual rows, activity-run content, and layer tree. Authored transform
state, authored `will-change`, row-owned drawing layers, and content-sized
timeline-scroller layers fail the probe. It changes no app state.

## Restarting a soak

`armSoakSteadyState` is idempotent. Fixtures are seeded only when the
store is empty and the live turn is re-armed on the *same* thread every
boot, so ten restarts still leave two threads. If the data dir holds
threads that the rig did not seed (you drove it by hand), it refuses to
arm rather than sending a prompt into your work. Delete
`~/.agent-overflow-soak` to reseed.

## Related

- [agent-harness.md](agent-harness.md): the mocking machinery the soak
  reuses wholesale, and the `Harness` RPC surface a running soak exposes.
- [`cmd/agent-overflow-windows/AGENTS.md`](../../cmd/agent-overflow-windows/AGENTS.md):
  launcher internals, log locations, WebView2 profile pinning.
