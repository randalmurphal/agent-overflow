# The Android shell

A Capacitor 8 project whose entire job is to put the SPA on a phone.
There is no mobile app here: `webDir` is `../frontend/dist`, the same
bundle the desktop ships, and the compact layout it renders is a layout
mode of that one app chosen from the viewport
(`frontend/AGENTS.md` § Compact). Everything phone-shaped in the
TypeScript lives in `frontend/src/lib/native/`, not in this directory.

What is here is the native container, its build, and the one fact the Go
side has to agree with.

## The fixed origin

`capacitor.config.ts` sets `androidScheme: 'https'` and
`server.hostname: 'shell.agent-overflow.invalid'`, which fixes the
WebView document's origin at `https://shell.agent-overflow.invalid`.
`internal/transport/shellorigin.go` admits that exact string as the
constant `ShellOrigin`, for the WS upgrade and for the CORS answer on the
routes the shell fetches. **The two name each other**: change the
hostname or the scheme here and the constant stops matching, and the app
gets a manifest fetch the browser refuses rather than an error anybody
can read.

One exact origin rather than a pattern, because `.invalid` is reserved
(RFC 6761 §6.4) and can never resolve. No page on any network can hold
it; the WebView has it only because Capacitor assigns its own document
that authority locally. A pattern would be a wider door for no gain.

The consequence for the frontend is that the page's origin and the home
backend's are two different things for the first time. That seam is
`frontend/src/lib/transport/homeEndpoint.ts` and it is documented in
`frontend/src/lib/transport/AGENTS.md`; nothing in this directory should
grow a second way to address a backend.

`CapacitorHttp` is disabled on purpose. It intercepts `fetch` and routes
it through the native HTTP stack, which is the opposite of what this
transport needs: the WebView opens its own WebSocket, and the fetches
beside it carry a session header and a device proof. The WebView's own
`fetch` and `WebSocket` are the whole transport.

## Building

```
make apk
```

which is `mobile/scripts/build-apk.sh`: build the SPA, `cap sync
android`, `./gradlew testDebugUnitTest`, `./gradlew assembleDebug`. The
order is strict — `cap sync` copies whatever is in `frontend/dist` into
the Android assets, so a run that skipped the SPA step would package a
stale bundle and say nothing about it, and the JVM tests run before the
assemble so a broken bundle store cannot be packaged.

The toolchain is not on PATH on the development box. Both are discovered
with the defaults below and can be overridden by exporting them:

| Variable | Default | Needs |
|---|---|---|
| `JAVA_HOME` | `~/.jdks/temurin-21` | JDK 21 |
| `ANDROID_HOME` | `~/Android/Sdk` | `platform-tools`, `platforms;android-36`, `build-tools;36.0.0` |

**`AO_SHELL=1` is what makes a build a shell build.** The three Capacitor
plugins are dependencies of THIS package, never of `frontend/`, so the
desktop bundle cannot carry them; `frontend/vite.config.ts` aliases their
specifiers at `mobile/node_modules` when `AO_SHELL=1` and at a
null-exporting stub (`frontend/src/lib/native/capacitorAbsent.ts`)
otherwise. `build-apk.sh` is the only thing that sets it. The same
mapping is repeated in `frontend/vitest.config.ts` and
`frontend/tsconfig.json` so the type check and the unit tests resolve the
same three names; adding a plugin means adding it in all four places.

A side effect worth knowing: `make apk` leaves `frontend/dist` holding a
SHELL bundle. Anything that serves `dist` afterwards (`make e2e`,
`make build`) rebuilds it, but a hand-run of a Go binary that embeds it
will not.

`minSdkVersion` is 26, not the Capacitor template's 24, because the
barcode scanner's native library declares 26 and the manifest merge
across that gap fails the build outright. `android/variables.gradle`
argues it in place.

## Cleartext, and why only the debug build has any

The release APK declares no network security config, so it keeps the
platform default: at `targetSdkVersion` 36, cleartext is refused for
every host. That is the correct posture rather than an inconvenience.
The phone's path to a backend is tailnet TLS
(`docs/specs/remote-access.md`, "The phone client"), so a pairing link
naming an `http://` endpoint fails at the fetch on a real device, which
is the answer that should be given.

`android/app/src/debug/` is a debug-only manifest overlay pointing at
`res/xml/network_security_config.xml`, and that file permits cleartext to
`127.0.0.1` and to nothing else — no `base-config`, so every other host
keeps the refusal even in a debug build. It exists for `make e2e-android`
alone: the harness backend runs on the developer's machine and is exposed
to the device with `adb reverse`, which makes the DEVICE's own loopback
the address.

Loopback rather than the emulator's `10.0.2.2` host alias, for a reason
that would bite anything else addressing a backend from here: the page's
origin is `https://shell.agent-overflow.invalid`, and Capacitor leaves
the WebView at `MIXED_CONTENT_NEVER_ALLOW`
(`CapConfig.allowMixedContent` defaults false and `Bridge` only calls
`setMixedContentMode` when it is true), so an `http://10.0.2.2:<port>`
request is refused by the renderer before any network policy is
consulted. Chromium treats loopback as potentially trustworthy, so
`http://127.0.0.1` is not mixed content at all. Turning mixed content on
in `capacitor.config.ts` would apply to the release build too, which is
the door the tailnet-TLS ruling closes — so it stays off, and the reverse
forward is what makes the test address a trustworthy one.

## The bundle plugin

The APK ships with the SPA it was built with and runs it until the
backend it paired with says it has a newer one. `BundlePlugin` (local to
this app, registered in `MainActivity`, the only native code wave 6g-a
adds) is what puts that bundle on disk; `frontend/src/lib/native/
bundleSync.ts` decides when, and `internal/bundle` +
`internal/transport/bundleroutes.go` are the other end. Design
authority: `docs/specs/remote-access.md` §9, "Bundle sync".

**Everything that decides anything lives in `BundleStore`, which takes a
directory and no Android type.** That is deliberate: the state
transitions, the unzip, the verification and the rollback are the part
of this feature that decides whether a phone boots, and none of it
should need an emulator to be proved. `BundleStoreTest` is a plain JVM
JUnit test — no Robolectric — and `make apk` runs it before it
assembles. What is left for the emulator is the part only a device has:
that the WebView really serves from the staged directory, and that the
plugin registers at all.

### The state file

One small JSON document at `filesDir/bundles/state.json`, beside one
directory per bundle named by its content id:

```
{current, next, pendingHealth, lastKnownGood, rolledBack: []}
```

Each of the four strings is a bundle id or `""`. **An empty `current`
means the APK's own assets** — the bundle this build shipped with. It is
the resting state of a phone that has never updated, the state a
rollback returns to when there is no last known good, and what a damaged
state file reads as. There is deliberately no separate "using assets"
flag: a second spelling of one fact is a second thing to keep true.

The file is written whole through a temporary name and a rename, because
it is read on the next cold start and a process killed mid-rewrite would
leave the one document that decides which bundle boots half-written.
Every read-modify-write holds one process-wide lock: Capacitor runs
plugin methods on its own task thread while the boot transition and the
watchdog run on the main one.

### The boot order, and why it is before `super.onCreate`

`MainActivity.onCreate` reads the state, applies this launch's
transition, and calls `bridgeBuilder.setServerPath(BASE_PATH, dir)` —
all **before** `super.onCreate`. It has to:
`BridgeActivity.onCreate` builds the Bridge from that builder and
immediately loads the WebView from whatever path it holds, so a path
installed afterwards would mean one launch of the wrong bundle every
time. `registerPlugin` is before it for the same reason.

The transition, in this order:

1. **`pendingHealth` is still set** → the previous launch swapped onto a
   bundle and never reported healthy. `current` becomes `lastKnownGood`,
   the bad id joins `rolledBack`, its directory is deleted.
2. **`next` is set** → adopt it, and arm the health check by setting
   `pendingHealth` to it. From here until the app reports healthy, case
   1 is what happens on any launch.
3. Otherwise nothing moves.

Then the directory is checked for real: a `current` whose directory is
missing or has no `index.html` falls back to the assets and is cleared,
so a bundle lost to a wipe or an interrupted install cannot become a
white screen.

### The 30-second watchdog

`ready()` is the health check, and the shell calls it once per launch
after the app has mounted and its home transport has reached hello — a
bundle that boots and talks to its backend is a bundle that works.

A bundle that hangs before then would otherwise sit on a dead screen
until the person killed the app, and killing it is what ARMS the
rollback, so they would have to work out that killing it is the fix. So
`MainActivity` arms a 30 s handler when the health flag is set: if it is
still set when the timer fires, the rollback runs in place and
`setServerBasePath` / `setServerAssetPath` points the WebView back at the
last known good bundle or at the APK's own assets. Thirty seconds is far
past a healthy boot on a cold WebView and far short of a person's
patience with a blank screen.

### `rolledBack`, and when it is cleared

`rolledBack` is what stops the shell downloading a bundle it has already
watched fail. It is cleared **only when a DIFFERENT id succeeds** —
which is exactly the launch that had `pendingHealth` set. A relaunch on
the existing bundle keeps the list, or the phone would fetch the same
failure again on the next hello.

### Staging

`stage({id, manifest, archiveBase64})` unzips into `<id>.staging`,
verifying as it goes: every entry must be a path the manifest names,
must resolve inside the staging directory (checked against the CANONICAL
path, not by looking for `..`), must match its digest and its size, and
every manifest path must have been delivered. Only then is the directory
renamed to `<id>` and `next` set. Any failure deletes the staging
directory and rejects with a message naming the file — "the update
failed" is not something anybody can act on.

The archive arrives base64-encoded because that is what a Capacitor call
can carry. It is a few MB once per update, on a background thread.

### Two dependencies worth knowing

`state()` reads the APK's own `versionCode` itself rather than through
`@capacitor/app`, so this seam has exactly one native dependency: the
shell compares that number against the bundle's `minShellBuild` before
downloading, and a seam that needed a second plugin to answer would be a
second plugin an update could not ship. And `android.jar`'s `org.json`
is a stub whose methods throw, so `app/build.gradle` puts the real
implementation on the unit-test classpath (`orgJsonVersion`); nothing
ships it to a device.

## What is committed

The generated `android/` tree is committed, minus build outputs,
`local.properties`, and the `assets/public/` copy of the SPA bundle
(`.gitignore` says which and why). A tree regenerated on every clone
cannot hold the edits native configuration needs, and `minSdkVersion` and
the debug source set above are already two of them.

## The seams

Each one has a web fallback that is inert, and every seam checks
`isNativeShell()` before it issues its dynamic import, so a browser build
never resolves a Capacitor module at runtime:

| File | Does |
|---|---|
| `native/platform.ts` | `isNativeShell()`, off `window.Capacitor` |
| `native/plugins.ts` | the guarded dynamic imports, and nothing else |
| `native/lifecycle.ts` | pause/resume to `setClientLease`, hardware back to the compact list |
| `native/lock.ts` | biometric gate on cold start and on resume past a window |
| `native/qr.ts` | `scanPairingQr()` |
| `native/pickers.ts` | a documented stub |
| `native/boot.ts` | what runs before anything mounts; `adoptPairingEndpoint` is the one place both pairing doors (scanned code, `#pair=` hash) point the shell at a backend |
| `native/bundleSync.ts` | the one door for downloading a newer bundle from an attached backend, and for reporting this launch healthy (§ The bundle plugin) |

Two things `main.ts` keeps true for the shell. The `#pair=` hash is
checked BEFORE the first-run screen for every client, so a shell can be
handed a pairing link (an app link, or the emulator smoke navigating to
one) and takes it. And the lock screen is a FIXED, full-bleed element
mounted after the app's root with the root marked `inert` while locked:
the app underneath stays mounted and warm, and nothing under the paint
can take focus or a tap.

Read `frontend/src/lib/native/lifecycle.ts`'s header before touching the
lifecycle: the pause signal is the ONLY visibility signal this client
sends and it is not `document.visibilityState`, and the back button
closes an overlay by dispatching Escape through the keybinding path
rather than by holding a registry of overlays.

## Verifying without a device

`make e2e` covers the cross-origin transport for real, in a real browser,
against the real Go server: `e2e/tests/compact-shell-origin.spec.ts`
serves `frontend/dist` from a second HTTP server so the page and the
backend are genuinely different origins. That is the half of this shell
that can be tested on a laptop.

The other half — that the bundle boots under the fixed origin, that the
plugins register, that the app lock gates the app, that the back button
arrives — needs an emulator: `make e2e-android`, which exits clean when
there is none. It drives the shell's own WebView through Playwright's
Android API rather than a Chromium of its own; `e2e/AGENTS.md` §
The emulator smoke owns the details, including the fact that it was
written against those docs and has NOT been run on a device yet.

## Deferred

Named here so nobody reads their absence as an oversight:

- **A keystore-bound signing key.** Debug signing only. The paired
  session over TLS is the trust root for this wave
  (`docs/specs/remote-access.md`, "The phone client"), and release
  signing is a distribution question that starts when there is a
  distribution.
- **`pickers.ts` answers `null`.** No native file or photo picker. The
  composer's own file input works in the WebView; the seam exists so the
  day a picker is wanted there is one place for it.
- **iOS.** Only `npx cap add android` was run. The seams are written
  against Capacitor rather than against Android, so an iOS target is
  another platform folder and a signing story, not a second frontend.
