# End-to-end tests

This Playwright suite exercises the real Go backend and compiled SPA with
isolated data roots and mock providers. The harness architecture is documented
in [agent-harness.md](../docs/architecture/agent-harness.md). Each spec's header
states its own coverage; do not maintain a duplicate spec catalog here.

## Shared infrastructure

- `src/harness.ts` owns backend launch and the TypeScript harness wire client.
- `tests/fixtures.ts` owns the worker backend and per-test reset. Before reset,
  wait until the previous context's page registration is gone; a leaked page is
  a test failure.
- `frontend-client-helpers.ts` owns production frontend-controller fixtures.
  Execution hosts remain separate harness processes, and cold-start cases reuse
  only the intended disposable device profile.
- Shared provider frames, pairing flows, and result narrowing belong in the
  relevant `*-helpers.ts` or `probe-wire.ts`, not inline copies.
- `forge-helpers.ts` seeds the fake `gh`/`glab` (`HarnessForgeSeed`), reads
  the recorded calls and opens a PR's review pane through the UI. A forge
  call the fake does not answer is added to the fake first:
  [forgefake/AGENTS.md](../internal/harness/forgefake/AGENTS.md).
- `rigs/` contains manual performance tools outside release gates. Follow
  [rigs/README.md](rigs/README.md) for their data and scenario rules.

Calls such as `harness.rpc('MethodName', ...)` are not linked to Go signatures.
Any bound-method signature change must sweep `e2e/tests` and `cmd/ao-harness`
and run the end-to-end gate.

Notification specs must account for screen presence. An open focused page can
suppress mapped notifications under normal preferences. Keep page-free tests
page-free, or explicitly set `notifyQuietWhen: "never"` on the connection whose
state the sender reads. A remote page presents for itself and gates on its OWN
presence, so a spec about what that page shows sets the preference on that
page's connection, not on the backend screen's.

## Running and evidence

`make e2e` builds the backend, mock provider, mock forge, and fixed-purpose
launcher, typechecks all suite sources, then runs Playwright under the harness
memory boundary. The `desktop` project runs ordinary specs; `compact` runs
`compact-*.spec.ts` with touch and compact viewport settings. Run one file with
`bin/ao-harness-e2e tests/<spec>`; it runs the `bin/agent-overflow` from the last
`make harness-build`, so rebuild after changing Go or frontend code.

`make e2e-mobile-browser` runs browser-lock checks in mobile Chromium and
WebKit without the Android emulator. See
[mobile browser validation](../docs/references/mobile-browser-testing.md) for
coverage boundaries, real visibility events and the real-phone check.

Manual specs and boundary probes are evidence tools, not automatic gates. Keep
their opt-in environment checks and report exactly which mode ran. Freeze
reproduction fixtures may contain real conversations: generate them from a
read-only database connection into a gitignored path and never commit them.

The optional saved-release recovery leg uses
`AO_E2E_RECOVERY_BASELINE=/absolute/path`. It proves only the exercised graceful
restart path unless the test explicitly causes abrupt loss. A version label
alone is not release-compatibility evidence.

Real SSH mode requires `AO_E2E_SSH_CONFIG` pointing to an isolated config for an
owned loopback sshd, temporary keys, and a pinned known-hosts file. Never use the
developer's normal SSH configuration or install a service from a test.

## Android emulator smoke

`make e2e-android` drives the installed shell's own WebView through
Playwright's Android API. It is separate from desktop/compact browser projects.
A zero exit with no attached device is a skip and provides no emulator evidence.

Start the emulator with `e2e/scripts/android-emulator.sh start`, never bare.
Emulator 37.1 leaks gigabytes of host memory from its adb proxy on every
Playwright session and has OOM-killed a whole WSL VM; the script contains it
under a memory cap. Restart the emulator between test batches. The diagnosis,
the cap rationale and WebView-driving pitfalls are in
[android-emulator.md](../docs/references/android-emulator.md).

The ordinary smoke may run on a real phone only when the operator explicitly
sets both `AO_ANDROID_SERIAL` to that device and `AO_ANDROID_HUMAN_LOCK=1`.
Every case clears Agent Overflow's app data. Human-lock mode must not provision,
change, or guess the phone credential; it waits for the owner to answer system
prompts. Never select a personal phone implicitly. Signed-release lifecycle
cases remain emulator-only because they replace installations and change
network radios.

The runner and spec divide ownership deliberately:

- `scripts/android-smoke.sh` selects the device, installs the APK, configures the
  temporary device PIN, and clears the PIN on every exit path.
- The page fixture clears only the target package, re-grants required test
  permissions, launches its activity, and attaches to that package's WebView.
- Each case owns its harness process, data root, `adb reverse` mapping, pairing,
  app state, and teardown. Never select the first WebView, process, port, or
  device without matching the expected package and run identity.

Ordinary smoke builds and installs the debug APK. Signed-release mode is enabled
only by an explicit absolute `AO_ANDROID_RELEASE_APK`; it requires an emulator
and fails if unavailable. Preserve this distinction in assertions and reports.

Bundle-adoption tests build a second harness with a strictly newer fixture
release and temporarily stamp generated dist metadata inside a `finally`
restore. Do not run that fixture builder concurrently with another frontend or
mobile build. Compare trimmed bundle IDs.

LAN renewal cases must prove the old route is gone. Stop the app before removing
`adb reverse`, cold-launch it, wait for session renewal rather than cached UI,
and verify the same pairing can resume after the advertised endpoint changes.
The native HTTP bridge bypasses Playwright route interception, so browser mocks
cannot prove this path.

Only explicit manual cases may use external services or real credentials. The
real Firebase delivery case self-skips unless
`AO_ANDROID_PUSH_CREDENTIAL` names an isolated service-account key. Ordinary
tests use mock providers and disposable homes and must not launch real provider
binaries.

## Writing specs

- Assert the user-visible result and the production state or wire boundary that
  caused it. Do not rely on arbitrary sleeps.
- Use unique IDs and await specific events so parallel workers cannot satisfy
  each other's assertions.
- Register event waits before triggering operations that may complete inside an
  RPC round trip.
- Close pages, browser contexts, clients, child processes, forwards, and temp
  roots through fixture-owned teardown, including failure paths.
- A UI geometry or animation claim requires real Chromium or the shell WebView;
  DOM-only tests cannot establish pixel geometry or compositor behavior.
