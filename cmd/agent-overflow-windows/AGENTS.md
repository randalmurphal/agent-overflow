# cmd/agent-overflow-windows/

Windows entry point for the WSL-backed build. The desktop `.exe` in the
Start menu picks a WSL distro, drops the Linux backend into it, spawns it,
and points a Wails WebView2 window at the resulting
`http://localhost:<port>` URL.

Launcher orchestration and the plain-HTML picker / loading / error pages
(`picker.html` ships before the backend exists) are here. Everything about
WSL itself stays in `internal/wsllauncher` so it is testable off-Windows:
discovery, spawn, Job Object lifetime, bootstrap-line parsing, and the
reconnecting backend WS client this binary's notification handlers hang
off. The backend is not a separate program; it is the same root `main.go`
binary running headless inside the distro (`--listen` / `--print-url-fd`),
and the chat UI is the embedded SPA under `frontend/`.

The build cross-compiles the Linux ELF backend first, embeds it as a payload,
then builds this `main` package against `windows/amd64` (`Taskfile.yml`). Job
Object teardown means killing the `.exe` always tears down the WSL-side child
too.

## CLI flags

The launcher is GUI-only in production; the user-facing flags are for the dev
path. `parseLauncherFlags` (`flags.go`) is the single source of truth for the
CLI shape and `resolveChosenDistro` (`main.go`) owns the override-vs-saved
precedence, both unit-tested.

- `--distro <name>` skips the picker and launches in that WSL distro, used by
  `make dev-wsl`. The override is TRANSIENT: a successful launch does not
  write `wsl.json`, so a dev invocation cannot overwrite the user's saved
  pick. An invalid value warns to `launcher.log` and falls through to the
  picker rather than to saved config, so the mismatch surfaces.
- `--profile harness|soak|perf` (or `AGENT_OVERFLOW_PROFILE=...`) runs an
  isolated instance beside the developer's own. This one flag is THE axis
  behind every piece of per-instance state: single-instance id, window title,
  WebView2 user-data dir, CDP port, `launcher-<profile>.log`,
  `window-<profile>.json`, debug-level Wails logging, a refusal to persist
  `wsl.json`, the backend's argv (`profileBackendArgs`), and the containment
  below. It folds through `launcherRuntimeMode()` into
  `internal/appidentity`, which owns the naming rules and the "unknown
  profile is an error, never a fallback" invariant.
  - harness waits to be driven by `bin/ao-harness`
    ([agent-harness.md](../../docs/architecture/agent-harness.md)). soak is
    that instance with `--autopilot` armed in an 800x600 window built to sit
    on a monitor for hours
    ([soak-rig.md](../../docs/architecture/soak-rig.md)). perf is a third
    driveable harness for renderer A/B runs owning `~/.agent-overflow-perf`,
    so a destructive reset or interrupt command must name that root.
  - All three pass `--launcher-pid <own pid>` so `ao-harness down` can close
    the launcher window. The launcher still outlives a crashed child on
    purpose, to preserve the evidence.

The parser also accepts Windows' internal `-Embedding` COM-server switch, so
a toast click can cold-start the launcher and register the notification
callback. It is not user-facing and does not alter distro selection.

## Connectivity probe and the one fresh-port retry

`launchAndProbe` probes `/bootstrap.json` over Windows localhost, and on the
UNREACHABLE class only (`errBackendUnreachable`: no HTTP response at all
inside `bootstrapProbeDeadline`) stops that backend and relaunches it ONCE
with `--reset-transport-port` (`wsllauncher.ResetTransportPortFlag`). Stop
the old backend first: it is healthy inside the distro and holds the SQLite
store.

The retry exists because the backend pins its listen port per install
(`internal/transport/AGENTS.md` § the listen port is pinned) from the
ephemeral range, the same range Hyper-V/WSL2 excluded port ranges cover and
Windows re-seeds every reboot. The WSL side sees a successful bind, so
nothing there can clear the pin, and the user would get
`/connectivity-error` identically on every launch.

Anything the backend ANSWERED (500 to `/startup-error`, 404, a never-ready
503) is not retried. The port is demonstrably reachable, and a fresh one
would churn the webview origin, wiping localStorage and the IndexedDB thread
replica, for nothing.

The probe gap starts at `bootstrapProbeInitialPollInterval` (25 ms) and
doubles up to `bootstrapProbePollInterval` (250 ms). A miss is an instant
503 or RST, so early retries are free; the flat 250 ms gap it replaced
cost every boot ~250 ms of sleep after the backend was already ready.

## Payload path: recorded, not re-resolved

For the normal dev/prod installation, `ensurePayloadInstalled` returns the path wsl.json recorded
(`InstalledBinPath`, written with `InstalledVer` after a successful boot)
whenever version and distro match, and spawns no wsl.exe at all on that
path. Resolving `$HOME` through wsl.exe costs ~440 ms per boot and only
matters when something has to be installed. The record is the one thing a
warm boot trusts without asking WSL, so `launchAndShow` treats
`errLaunchFailed` on a recorded path as "maybe stale": it re-resolves once,
reinstalls at the fresh path if it differs, and retries. A path that
resolves the same is a real launch failure.

Isolated profiles never read that install record or replace its binary.
`appidentity.WSLBinaryDir` puts their backend and mock provider together at
`~/.local/share/agent-overflow/<profile>/bin/`; `launch-wsl` stages the mock
there, and launcher cleanup matches only that profile's exe names. Test
profile transitions with an otherwise matching dev install record: data,
browser profiles, and CDP isolation do not imply executable isolation.

## Presenting a bridged notification

`notifications.go` is the host-side presenter for everything the backend
sends on `notification:send`. The wire shape, its limits and its admission
check are `internal/notify`'s, re-run here because a cross-process boundary
validates what it is handed rather than trusting the sender.

- **A send carries a STABLE id and may be a RETRACTION.** `present` branches
  on `Send.Retract`: withdraw by id, or `UpdateNotification` with the id the
  mapping chose. Never allocate an id here — replace-in-place is exactly the
  platform recognising a second send about the same moment as the same
  notification.
- **Retraction degrades to nothing on Windows, silently, on purpose.**
  wintoast exposes no call that pulls a delivered toast back out of the Action
  Center, so Wails' `RemoveDeliveredNotification` answers nil without acting.
  Refusing the retraction, or logging it as a failure, would turn a platform
  limit into an error the user sees — for an operation whose whole purpose is
  to make things quieter. Linux (D-Bus `CloseNotification` + `replaces_id`)
  and macOS (remove the delivered notification) do act — through
  `RemoveDeliveredNotification`, never the similarly-shaped
  `RemoveNotification`, which is a nil stub everywhere but Linux.

## Self-update: acting on an install directive

The WSL backend downloads and digest-verifies the new launcher `.exe`, stages
it into `%APPDATA%\agent-overflow\update` through `/mnt/c`, then emits an
`InstallDirective` on `updater:install` because it cannot replace a running
Windows executable. `update.go` is the half that swaps.

- `handleUpdateInstall` is wired in as `HandleUpdateInstall` by
  `startNotificationBridge`. One install runs at a time; a directive
  arriving during one is logged and dropped, not reported as a failure of
  the install already proceeding.
- The staged path is `<staging dir>\<directive.Filename>`.
  `selfupdate.InstallDirective.Validate` guarantees a bare file name, which
  is what makes "the wire can never name a path" structural.
- The swap runs a FRESH `updater.New` per directive (`Init` is one-shot and
  directives repeat after a failure) over a `selfupdate.StagedFileProvider`
  with `updater.WindowNone`. `CheckAndInstall`'s streaming hash re-verifies
  the staged bytes, so there is no separate pre-hash.
- `ReportUpdateInstallStatus` acknowledges (`proceeding`) first, and its
  result decides whether the swap happens at all via
  `wsllauncher.ClassifyInstallAck`: REFUSED aborts without a `failed`
  report, because the backend already unwound the install and showed the
  user an error. UNDELIVERED (timeout or disconnect) proceeds, because an
  unanswered report may have landed with only its response lost. Any error
  before `Restart` succeeds reports `failed` with a reason.
- `armUpdateExitWatchdog` force-exits 25s after `Restart`, under the swap
  helper's 30s parent-exit abort, so a wedged graceful shutdown cannot
  silently cancel the swap. Disarmed only when the helper spawn fails.

`updater.HandleHelperMode()` is therefore the FIRST statement of `main()`,
before flags, config, and logging. The helper child is this same binary, and
Wails' own call inside `application.New` would run distro detection, the
picker, the payload install, and the single-instance machinery against the
app it is trying to replace.

## Keep-awake: acting on a power directive

The backend owns the keep-awake SETTING but runs inside the distro and cannot
make the Win32 call, so it emits a mode (`off` / `system` / `display`) on
`eventchan.PowerKeepAwake`. `applyKeepAwakeDirective` (`keepawake.go`) is the
`NotificationClientConfig.HandleKeepAwake` callback that asserts it through
`internal/power`, the same holder the native Windows build uses: do not
reimplement it here, because `SetThreadExecutionState` needs a goroutine
parked on a locked OS thread for the process lifetime. An unrecognized mode
is dropped, never defaulted, since guessing `display` pins the machine awake
on a garbled frame and guessing `off` drops an inhibit the user asked for.

## Embedded browser pane: hosting the second WebView2

The backend decides what the pane shows and where it sits; the controllers
that draw it must be child windows of THIS process's HWND, driven from its
UI thread. So the backend emits directives on `eventchan.BrowserHost` and
`browserhost.go` executes them through `internal/webview2host`, which owns
the COM, the z-order rule, and the CDP relay (its guide has the reasoning
for all three).

- **Lazy, not bootstrap-gated.** `handleBrowserHostDirective` builds the
  host and its tunnel on the FIRST directive. The feature costs a browser
  process and a profile directory, most sessions never open a pane, and a
  backend without the feature simply never emits. A bootstrap flag would
  have to be kept in sync to say what the first directive already proves.
  A construction failure is not cached: its inputs (AppData, the profile
  directory, a free port) can come back. The two ops the backend BLOCKS on
  are answered even when the host could not be built at all — `create`
  with `create-failed`, `clear-data` with `clear-failed` — rather than
  becoming a pane that never appears or a Settings button that spins until
  its own timeout. The rest address a page that, by definition, was never
  created. Being lazy also makes it profile-agnostic for free:
  `startNotificationBridge` wires `HandleBrowserHost` on every launch, so
  an isolated profile's backend is served exactly like the dev instance
  the moment it emits — which is what makes
  `AO_HARNESS_REAL_BROWSER=1 make harness-wsl` the Windows leg of the
  real-engine gate (`docs/specs/embedded-browser.md` §10) with no
  launcher-side wiring of its own.
- **Profile storage.** `prepareBrowserProfileStorage` creates
  `appidentity.BrowserProfilesDir(mode)` beside the SPA's own webview2
  directory, through the same `validateWindowsStoragePath` that refuses
  symlinked and reparse-point components. Per mode like the others, and
  for a harder reason: a WebView2 user-data folder belongs to one browser
  process, so a shared folder would leave whichever launcher started
  second unable to create its environment at all. It is also the folder
  Settings → Clear site data DELETES (and recreates empty): the backend's
  own `browser-profiles/` tree is empty on this deployment, so this folder
  is the whole of the user's pane site data.
- **Env scrub at boot.** `main` calls `webview2host.ScrubEnvOverrides`
  before `prepareWebviewStorage`, ahead of the SPA environment Wails
  builds. An inherited `WEBVIEW2_USER_DATA_FOLDER`, including a SET BUT
  EMPTY one, silently collapses every environment in the process onto one
  profile with no error anywhere.
- **Reports go through a serial queue.** `reportBrowserHost` submits to
  `launcherApp.browserReports` rather than calling the RPC inline. The
  host reports `created` from a WebView2 completion handler running on the
  UI thread, where a blocking RPC would freeze the window; a bare `go`
  would let the backend see `closed` before the `created` carrying the
  page's CDP target id.
- **Teardown before the windows.** `OnShutdown` calls `closeBrowserHost`
  ahead of `stopLaunchedBackend`: a pane controller outliving its parent
  HWND faults inside WebView2. Calling it from that hook is safe even
  though the hook already runs on the main thread, because Wails' dispatch
  runs the closure inline when it is already there instead of posting to a
  pump that is blocked waiting on the hook.

## Isolated-profile containment

Three layers, all gated on `activeProfile != ""` so a production launch keeps
its existing lifetime and memory behaviour, sharing one ceiling
(`governor.DefaultCeilingBytes`).

- **Windows Job Object.** `installHarnessBoundary` puts the launcher in a
  job carrying `JOB_OBJECT_LIMIT_JOB_MEMORY`, `KILL_ON_JOB_CLOSE`, and
  `SILENT_BREAKAWAY_OK`, before Wails creates WebView2. A harness profile
  that cannot install it fails closed, and the handle is deliberately never
  closed during shutdown: `KILL_ON_JOB_CLOSE` is the final descendant
  backstop.
- **WSL memory watchdog.** A Windows job cannot account for guest memory,
  so `startWSLMemoryWatchdog` polls the Linux process tree through
  `wsl.exe` every `wslMemoryWatchInterval` (100ms), summing
  `/proc/<pid>/stat` RSS. Every sample rechecks pid, `/proc` start time,
  and `/proc/<pid>/exe` before accepting the number, so a recycled pid can
  never be read as the backend. A failed probe, a changed identity, or an
  over-limit sample stops the backend and quits the launcher.
- **Host-global reservation.** `acquireHarnessReservation` claims the
  COMBINED launcher plus WSL budget once in `internal/harness/governor`, so
  concurrent worktrees cannot each assume they own the whole budget. The
  lease renews on a TTL/3 ticker, and a governor event (host
  available-memory floor or safety ceiling) quits the instance. Release
  happens only after both sides are confirmed stopped: an uncertain teardown
  leaves the lease visible for dead-owner pruning rather than freeing
  capacity early. `writeWSLContainmentEvidence` records what was enforced as
  `harness-containment.json` under the profile's WSL data root.

## Minimised-window memory trim

`webviewtrim.go` suspends the WebView2 (the pinned wails fork's
`SuspendWebview` / `ResumeWebview`) after `suspendAfterMinimiseDelay` (30s)
minimised and resumes on un-minimise, releasing the ~500MB of renderer and
GPU working set a parked 4-pane session holds. Nothing user-observable runs
while minimised, and the transport's replay ring plus seq-gap refetch
reconstruct anything missed. The suspend side re-checks the minimised state
on the main thread, so a timer racing an un-minimise cannot hide the webview
under a visible window.

## Diagnostics: where the logs are

Nothing from the Windows side reaches the dev terminal; the launcher is a
GUI-subsystem exe. Everything below is under `%APPDATA%\agent-overflow\`.

- **`launcher.log`** is the primary log: the launcher's own `log` output,
  Wails' internal slog (wired via `application.Options.Logger`, info-level
  in dev and warn+ in prod; without that wiring Wails logs go to a
  discarded GUI stderr), and the ENTIRE WSL backend's stderr, piped in line
  by line.
- **`webview2-dev\EBWebView\chrome_debug.log`** (prod: `webview2\`) is
  Chromium's own log: GPU and compositor errors, process deaths, and
  renderer `CONSOLE(n)` lines. OPT-IN via
  `AGENT_OVERFLOW_WEBVIEW_LOG=1 make dev-wsl`, which whitelists the var across
  the WSL to Windows hop through WSLENV (the gate works in prod builds too).
  Off by default because enabling Chromium logging opens a visible console
  window even for file-only destinations (WebView2Feedback #3192, no
  workaround), and closing that console CTRL_CLOSE-kills the whole app.
  Chromium truncates at every browser start, so `rotateChromeDebugLog` keeps
  the prior session as `chrome_debug.previous.log`: after a webview crash the
  autopsy is there, not in the live file.
- **DevTools.** Dev builds bind F12 to the WebView2 devtools window
  (`uikeys.WithDevTools`, gated on `launcherMode == "dev"` because dev and
  prod ship the same .exe) and expose CDP on `127.0.0.1:9223`. WebView2's own
  F12 accelerator is dead in all builds: Wails sets
  `PutAreBrowserAcceleratorKeysEnabled(false)`.

WebView2 storage paths are pinned via `WebviewUserDataPath`
(`webviewDataDir`): the default derives from the exe name, and dev exes are
timestamp-named, so every run would mint a throwaway profile.
`prepareWebviewStorage` creates the profile and diagnostics directories before
Wails boots, refusing symlinked or reparse-point components (a junction is a
reparse point even when `os.Lstat` does not call it one, so both checks run).

The `main.go` package doc has the step-by-step launcher flow.
