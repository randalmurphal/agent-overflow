# e2e/

Playwright suite for the agent test harness: real backend, real SPA,
headless, isolated data dir, mocked providers. Full harness guide:
[docs/architecture/agent-harness.md](../docs/architecture/agent-harness.md).

`tests/*.spec.ts` is the index. Each file names its subject, and every
spec's own header comment says what it proves. Do not keep a per-spec
catalogue here: the last one had drifted 11 of 44 files behind the
directory.

## Where the shared pieces live

- `src/harness.ts` is the TS client. `launchHarness()` spawns
  `bin/agent-overflow --harness` on a temp data dir, parses the
  `__AO_HARNESS__` bootstrap line, and returns a `HarnessApp` speaking the
  transport wire (RPC by method name, event push) over one WebSocket. It
  is also the reference for driving a harness from anything else, such as
  a Playwright MCP session or an ad-hoc script.
- `tests/fixtures.ts` owns the worker-scoped backend and the per-test
  `harness.reset()`. Before the reset it waits for the ui bridge to hold
  no page (`harness.awaitNoPages()`): the previous test's page leaves the
  registry when its WebSocket is torn down, which Playwright does not
  order before the next test's fixtures, and a ui query naming no page
  refuses two. The wait returns the instant the count reaches zero
  (~1.5-2s in the common case: Chromium context teardown, not a backend
  cost), so its ceiling is generous — 15s, because a heavy test that
  navigated one page several times was measured clearing at ~5.6s on
  macOS. A page still there at the ceiling fails the test as a leaked
  context.
- **A spec that asserts a MAPPED notification must not have a page open.**
  Since wave R5 the SPA states a screen presence on its socket, and the
  backend's default `notifyQuietWhen: "focused"` holds back a notification
  about a screen that is being looked at (`internal/app/app_notifications.go`) —
  a Playwright page HAS focus, so a mapped turn-complete would simply not be
  raised. `notifications.spec.ts` and `push.spec.ts` are page-free today and
  that is what makes them deterministic, not luck. A spec that genuinely
  needs both writes `UpdateSettings({ notifyQuietWhen: "never" })` first,
  on the same connection the sender reads (a harness connection names no device, so the write lands on
  the backend machine's own screen).

  The attended gate is deliberately LIVE under the harness rather than
  pinned off at boot: turning it off there would make it the one piece of
  notification logic `make e2e` never runs, which is the mistake this rig
  already made once with the refusal stub
  (`docs/architecture/agent-harness.md`). `HarnessNotify` is the single
  exception and it says so — it sends through `notifyOSUngated`, because a
  send that exercises the pipe must not depend on preferences a spec never
  set.
- `harness.rpc('MethodName', ...)` calls bound methods by NAME STRING, so
  no compiler connects these call sites to the Go signature. Changing a
  bound method's parameters must sweep `e2e/tests` and `cmd/ao-harness`
  for that name (the dispatcher rejects a wrong arity with `bad_params`,
  which is 26 red specs, not a build error — 2026-08-31, the `ListItems`
  `inlinePreviews` param). `make e2e` is the gate that catches it; run it
  before merging any bound-signature change.
- `tests/*-helpers.ts` and `tests/probe-wire.ts` hold the wire builders
  and seeds their spec families share. Put a new provider wire shape
  there, not inline in one spec. `offhost-helpers.ts` also owns the
  PAIRING CEREMONY every off-host spec starts with — mint the link,
  redeem it on the real screen, compare the number the device shows
  against the one the host holds, confirm — because one flow with two
  implementations is one that drifts, and it owns
  `answered(outcome, why)`: a wire-level spec that wants the PAYLOAD of a
  call needs the outcome union narrowed, and `expect(outcome.ok).toBe(true)`
  narrows nothing, so reading `.result` after it fails the launcher's
  typecheck rather than the assertion.
- `rigs/` holds self-driving perf measurement rigs (storm, churn,
  heapsoak, coldload). They are operator tools outside every gate, and
  [rigs/README.md](rigs/README.md) has the clone-root venue, the scenario
  reinstall rules, and the storm-density caveat.

## Running

`make e2e` builds `bin/agent-overflow`, `bin/ao-mockprovider`, and the
fixed-purpose `bin/ao-harness-e2e` launcher. The launcher typechecks the
suite first (`tsc --noEmit` over `src/`, `tests/`, `android/`,
`scripts/`, and all three configs — `tsconfig.json` names them), because
Playwright and the flow runner only STRIP types: a
typo'd property in a helper's predicate would otherwise pass an emptiness
assertion vacuously. It then runs `pnpm exec playwright test` under one
process-tree memory boundary and host-floor watchdog. The complete
two-worker gate reserves 6 GiB. `pnpm test` here uses the same launcher
through `go run`. Override the backend binary with `AO_HARNESS_BIN`.
Chromium comes from the Playwright cache
(`pnpm exec playwright install chromium` on a fresh machine).

The gate is two Playwright PROJECTS over the same harness and the same
bundle: `desktop` (Desktop Chrome) runs every ordinary spec, and
`compact` (Pixel 7: touch, coarse pointer, a 412px layout viewport) runs
the `compact-*.spec.ts` files. Compact is a layout mode of the one app
(frontend/AGENTS.md § Compact), so a surface is done only when both
projects pass, and a fix found on one is checked on the other. Run one
file with `bin/ao-harness-e2e tests/<spec>`; the file's name picks the
project. Changes to the native seams (`frontend/src/lib/native/`) also
rerun the emulator smoke, `make e2e-android`.

Not everything in `tests/` runs in the gate, on purpose. A
`*.manual.spec.ts` is `testIgnore`d by `playwright.config.ts` and needs
`playwright.manual.config.ts` plus a locally generated fixture. The
`*-probe.spec.ts` instruments skip themselves unless `BOUNDARY_PROBE` is
set: they dump per-frame samples for offline analysis rather than
asserting, so they are evidence, not a gate.

Freeze reproductions are manual on purpose. `scripts/generate-freeze-repro.mjs`
reads the live DB read-only and writes a fixture for a thread and turn range
(it refuses a non-gitignored `--out`: fixtures carry real conversation content
and are never committed); `freeze-repro.manual.spec.ts` replays it with the
probe armed. Run with `pnpm test:freeze-repro`. Saturation shows as a longest
gap far above the probe floor with per-task profiles; a single-loop wedge
shows as a pause stack. The probe arms `Debugger` up front because
`Profiler.stop` never answers on a wedged thread.

## The emulator smoke

`make e2e-android` is a THIRD suite, not a third project: its own config
(`playwright.android.config.ts`), its own directory (`android/`), and one
spec, `android/shell-boot.spec.ts`. It has to be separate because its
`page` fixture does not come from a browser Playwright launched — it is
the shell's own WebView, reached through Playwright's Android API
(`_android.devices()` → `device.webView({pkg})` → `webView.page()`), so a
spec written for it is nonsense under `desktop` or `compact` and vice
versa. Everything after that fixture is the ordinary Page API.

**First run 2026-09-03**, on a Mac against an arm64 android-36 emulator
(no biometric, a device PIN). It was written from the Playwright Android
docs and this app's own contracts on a box with no emulator, and the
first run found five shell defects the unit suites could not reach (the
spec's header lists them) plus two stale premises of its own. Do not
read a green `make e2e-android` on a laptop as evidence: it exits 0
when no device is attached, on purpose.

`make e2e-android` enables UI trace in its backend build so its bundle
differs from the APK even on the same checkout. The update case trims
`bundle-id.txt` before checking that prerequisite; its trailing newline
must never make identical bundles look different.

`scripts/android-smoke.sh` owns what is per RUN: it installs the APK
`make apk` built, sets a device PIN, and clears the PIN on every exit
path. The SPEC owns everything per case and everything downstream of
the port: its `page` fixture `pm clear`s the app, re-grants the
notification permission and relaunches the activity before EVERY case
(the shell persists its endpoint and session in the WebView's
localStorage, each run's harness is on a fresh port, and a case that
failed with the credential prompt up would otherwise leave the WebView
paused, timers and all, for the next one), and the cases own
`launchHarness`, the `adb reverse` forward that lets the device reach it,
and the pairing. It runs through
`bin/ao-harness-e2e --config=playwright.android.config.ts`, which is what
typechecks the tree and what lets `launchHarness` spawn at all.

**A real phone** runs the same suite with `AO_ANDROID_HUMAN_LOCK=1`
(wireless adb included: pair and connect in developer options, then name
its serial with `AO_ANDROID_SERIAL`). The smoke clears the app data; use it
only on a test installation. Without a serial, the runner selects only a
single emulator and refuses an ambiguous device list. A real phone without
`AO_ANDROID_HUMAN_LOCK=1` is refused before installation or PIN changes.
`TestAndroidSmokeSelectsOnlyAnExplicitPhone` checks selection with a fake adb. The script skips PIN
provisioning — the owner's credential is already on the device, and
typing `1234` at their real prompt would be wrong-PIN attempts Android
escalates into a lockout — and the spec instead waits up to two minutes
for the owner to answer each credential prompt by hand. That hand is the
point: it is the only way the biometric fallback
(`allowDeviceCredential: true` with a real finger enrolled) ever gets
exercised, since an emulator has none.

The spec's last case is the push last hop — a real message through the
owner's Firebase project, Google, and the phone's tray. It skips itself
unless `AO_ANDROID_PUSH_CREDENTIAL` names a service-account key file and
the APK was built with `google-services.json` in place (mobile/AGENTS.md
§ google-services.json), making it a manual gate in the same sense as
`make provider-smoke`: run it when the Firebase project or the push path
changes. First real delivery 2026-09-04, Pixel 9a over wireless adb.

Two platform facts the spec has to answer for, both learned on that
run: the platform's credential prompt is an activity of its own, so it
is answered through the focused native PIN field, then Enter,
not at the page; and a hardware back press with the soft keyboard up
closes the keyboard and reaches nothing else, so `pressBack` closes the
keyboard first, by the same key.

The first case also opens and cancels the composer's Photos and Files
choosers, then presses Back during a gated mock turn and verifies that the
list opens while the provider keeps running. Keep this native check beside
the browser regressions (`compact-composer-polish.spec.ts` and
`compact-reconnect-turn-completion.spec.ts`): a browser cannot prove that
the platform picker or Android Back reaches the right app path.

**The backend is reached at `127.0.0.1` over `adb reverse`, not at
`10.0.2.2`.** Two independent walls make the emulator's host alias
unusable and the spec's header argues both: the page's origin is
`https://`, and Capacitor leaves the WebView at
`MIXED_CONTENT_NEVER_ALLOW`, so an `http://10.0.2.2:<port>` fetch is
refused by the renderer; and `transport.Server.loopbackHostGuard` answers
404 to a non-loopback `Host` while the listener is on loopback. A reverse
forward makes the device's own loopback the address, which Chromium
treats as potentially trustworthy and the guard admits, so the pairing
payload is redeemed exactly as `MintDevicePairing` wrote it. The debug
APK carries a network security config permitting cleartext to that one
host and nothing else (`mobile/AGENTS.md`).

It is deliberately NOT a blocking gate: with nothing attached it prints
how to create and start an AVD and exits 0, because the seams' web
fallbacks are already covered by `pnpm test` and a check that cannot run
on a laptop is a check people learn to skip. What it answers that nothing
else can is whether the bundle boots under the shell's fixed origin,
whether the Capacitor plugins register, whether the app lock gates the
app, whether the hardware back button reaches `showCompactList`, whether a
STAGED bundle is what the WebView serves after a cold start — including
that the shell clears the health flag before the 30-second watchdog rolls
it back (`mobile/AGENTS.md` § The bundle plugin) — and whether a
NOTIFICATION TAP that cold-launched the app lands on its thread once the
lock is answered. That last one is delivered as `am start` with the
extras `AndroidTray` writes rather than by clicking a real tray entry:
the extras ARE the contract, and driving the system tray would add a
surface no assertion is about.

What CAN be answered without a device is the transport half, and
`compact-shell-origin.spec.ts` answers it: it serves `frontend/dist` from
a throwaway `http.createServer` on its own port, so the page and the
backend are genuinely different origins and the browser enforces CORS
itself. Setting `window.__aoHomeEndpoint` on a page the backend served
would exercise the URL rewriting and prove nothing, because every request
would still be same-origin. That spec also covers the update channel's
transport half end to end: a paired page on the other origin reads
`/bundle/manifest.json` and `/bundle/archive.zip`, unzips the archive in
the browser (`fflate`) and checks every file's SHA-256 against the
manifest, whose id must equal the `bundle-id.txt` in the very tree the
page was served from. That last equality is the Go rule
(`internal/bundle`) and the build rule (`frontend/scripts/bundleId.ts`)
agreeing over the whole shipped bundle, which is why neither this suite
nor that spec implements the hash a third time.

## Owning processes

`src/harness-process.ts` is the only place that reads the process table,
and everything it produces is evidence for a kill. It builds
`ProcessIdentity` / `ProcessRow` three ways (Linux `/proc`, darwin `ps`,
Windows CIM); a consumer that needs a field the platform branch forgot
does not fail — it reads `undefined` and degrades, so when adding a
field, add it in every branch, and prefer an assertion that fails on the
missing value over one that skips. Two rules keep the evidence real:

- **An identity carries its process group on Unix.** Escalation after
  the group leader exits authenticates through a surviving member proof,
  and `captureProcessGroupMemberProof` declines any identity without a
  `groupId` — so a platform branch that omits the field silently disarms
  teardown instead of failing loudly (the Linux branch did exactly that,
  fixed 2026-08-31). A row only becomes a proof once its executable
  resolves; on Linux that link is read per candidate, never per row,
  because the memory watchdog sweeps every row on a cadence.
- **Sweep `/proc` by name.** `readdir` with `withFileTypes` lstats the
  entries procfs leaves untyped, so a process exiting mid-scan raises
  ENOENT out of the whole scan; the watchdog reads that as a backend
  fault and takes the run down with it. Numeric names plus per-process
  reads already guarded against disappearance are enough.

## Writing specs

- **Never sleep.** Await `harness.waitForEvent('harness:mock', ...)`,
  `'harness:replay'`, `'provider:turn_completed'`, or
  `'workflow:item-state'` for backend progress, and Playwright's
  auto-waiting locators for the DOM.
- **Backend setup goes through RPCs** (`HarnessSeed`,
  `HarnessSetScenario`, `SendMessage`, ...), not the UI, unless the UI
  interaction is the thing under test.
- **Assert the precondition your assertion depends on.** A surface that
  is supposed to overflow, a fixture that is supposed to have two rows: a
  drifted fixture should fail rather than quietly stop testing anything.
- **An assertion that nothing happened waits for the thing that would
  have.** Emptiness is true before the work starts, so a spec that checks
  it without first waiting on a SETTLED rendered state is racing what it
  is about, and wins often enough to look green — two runs in three, for
  "a view-only device spends no refusal", which was passing over four
  real refusals (2026-08-31). Wait on the state the guarded path
  produces, and assert the capture itself saw traffic, so a broken probe
  reads as a failure rather than as a clean bill.
- **A listener this process opens is one the backend genuinely
  discovers.** The harness runs on the same machine as the spec, so a
  `node:http` server the spec binds on `127.0.0.1:0` is found by
  `internal/devscan`'s /proc walk with nothing faked and no scanner
  injected — and because it belongs to the Playwright process rather than
  to anything the backend spawned, it is attributed to no thread and
  arrives as a `seen` candidate. That is what lets the preview-gateway
  pair drive the real allow-then-open flow, and it is also the only way
  to assert what CROSSED the proxy: the fake server records the `Host`,
  `Origin`, raw request target and cookies of every request, so a
  rewrite that would have made a real dev server answer 403 fails on the
  record rather than passing on a green screen. Bind port 0 and read the
  port back; never pin one.
- **Ask the harness RPC, not the production reader, for a negative.**
  `App.ListThreads` hides the item-less draft row several bugs create, so
  "no row exists" goes through `HarnessListThreadRows`. Turn liveness
  comes from `ListItems` statuses, never `Thread.hasIncompleteTurn`, which
  is derived against `last_read_at` and flips when the UI opens the
  thread.
- **Open the page before the session when live progress matters.** Ticks
  are in-memory UI state that no reload recovers, so gate each one behind
  a mock `waitSignal` rather than racing it.
- **A scenario reaches only the mocks that register after it is set.**
  That ordering is how one spec stages one behaviour for a run and a
  different one for the session a recovery action starts.
- Draft threads (no items yet) are hidden from the sidebar. Seed at least
  one turn, or send the first message before navigating, when a spec needs
  the thread visible.
- **A seeded-and-opened thread takes the MOUNT path; the in-app draft does
  not.** "+ New" holds a placeholder and adopts the created row in place,
  which is a different code path from `openThreadInPane` for everything
  keyed on the pane's thread identity (the watched-thread set above all).
  `draft-first-turn-render.spec.ts` drives that path through the real
  composer and asserts the first turn RENDERS; a change to how a pane
  acquires its thread is not covered by the RPC-seeded specs.
- **Every page this backend hands out shares ONE ui_state bucket.** Pane
  layout persists under the `client:<id>` scope and `/pageurl` answers with
  the same client id every time, so a second page BOOTS INTO the panes the
  first one opened — and then watches their threads. A spec that needs a
  client with no panes, or with a different set, opens it BEFORE any other
  page opens one; `HarnessReset` clears ui_state, so the first page of each
  test is the only one that can boot bare.
  `transport-watch-badge-carriers.spec.ts` turns on that ordering.
- **A spec boots its OWN backend only for state `harness.reset()` cannot
  undo**, and then owns everything downstream of it. The LAN bind and the
  canonical domain both PERSIST to the settings file and REBIND the
  listener, so borrowing the worker fixture's instance hands the next
  spec a rebound backend. Such a spec is `test.describe.serial` with its
  own `beforeAll`/`afterAll`, restores the settings it wrote, and — when
  its legs need different browser LAUNCH arguments, since
  `--host-resolver-rules` is process-wide — owns its browsers too.
  `harness-remote-device-lifecycle.spec.ts`,
  `harness-passkey-lifecycle.spec.ts`,
  `harness-provider-signin.spec.ts`, `compact-shell-origin.spec.ts` and
  the preview-gateway pair (`preview-gateway.spec.ts` /
  `compact-preview-gateway.spec.ts`, whose backend also holds a LAN
  preview LISTENER open on somebody else's port for the length of the
  file) are the five, and each header argues its own constraints where
  they bite. The cross-origin one owns its backend for a different reason than
  persistence: the page origin it has to admit is an ephemeral port that
  does not exist until a listener has one, so the backend has to be
  LAUNCHED with that origin in its environment. Read the passkey one before
  writing any WebAuthn case: the three requirements a page has to satisfy
  at once (secure context, a DOMAIN relying party, a non-loopback peer)
  admit exactly one shape, and Chromium's virtual authenticator has a
  ceiling the header names rather than stages around. The sign-in spec is
  the other kind of unresettable state: it ADOPTS provider accounts,
  which live in the account store rather than in anything
  `HarnessReset` clears.
- Otherwise each worker owns one backend. Tests share it and must leave
  it reset (the fixture does this) rather than booting their own. Production
  project deletion drops the workflow rows (D25), but `HarnessReset` still
  deletes them itself first (`DeleteProjectWorkflowRecords`): reset removes
  the generated workspace tree wholesale rather than spending a git
  worktree removal per checkout on fixtures that are about to go anyway. A
  spec that asserts on a global count (the overlay's attention badge, the
  sweep total) depends on that explicit delete.
- Transport notification replay survives `HarnessReset`. Any spec whose
  backend state can produce a notification therefore declares a distinct
  no-op worker fixture identity, and each cold-activation case declares
  its own, so an activation for deleted test state cannot redirect or
  satisfy a later spec. That population is now EVERY spec that runs a
  turn: the event mapping (`internal/app/app_notification_mapping.go`)
  raises a `notification:send` when a top-level turn comes to rest, fails,
  or opens an approval, and withdraws it when the thread resumes. A spec
  asserting on notification traffic must therefore filter by thread id or
  kind rather than by "the next send".
- **Push is real up to the last hop, and that hop is a recorder.** A
  harness boot installs one in the `push.Sender` seam
  (`InstallHarnessPushSender`, only where no credential is configured),
  so the mapping, the fan-out, the per-device preference gate and
  `push.MessageFor` are all production and `HarnessPushSent` reads back
  exactly what would have gone to Google. `HarnessReset` clears that
  ledger with the other per-test state, but NOT the device rows or their
  registrations — those are access state, which is why `push.spec.ts` is
  `test.describe.serial` and pairs once. The wire's `notification:send`
  is the BARRIER for a push assertion rather than the assertion: the
  fan-out runs on its own queue behind the notification queue, so the
  ledger is polled after the event, never read on it.
- **Navigate with `harness.open(page)`, never `page.goto(harness.url)`.**
  A page URL carries a one-time ticket the first load exchanges for an
  HttpOnly session cookie, and each Playwright context is a fresh cookie
  jar, so every navigation needs a ticket of its own. `open` asks the
  running instance for one (`GET /pageurl`, session token in an
  `Authorization` header). `harness.url` is the boot URL's identity —
  origin, page marker, client id — not something to navigate to twice.
- Provider homes are seeded by writing files under
  `harness.bootstrap.homeDir`. The harness pins both `$HOME` and
  `App.credentialHomeOverride` at `<dataRoot>/home`, so a spec cannot
  reach the developer's real `~/.claude` or `~/.codex` even by accident.
- The mock provider cannot shell out, so a spec that must exercise a real
  subprocess reads a live session's `AO_*` environment through
  `HarnessSessionEnv` (a READ of the token registry, never a mint) and
  spawns the binary with exactly that env. Everything past the process
  boundary is then production code.
