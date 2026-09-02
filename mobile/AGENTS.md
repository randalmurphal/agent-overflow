# The Android shell

A Capacitor 7 project whose entire job is to put the SPA on a phone.
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
android`, `./gradlew assembleDebug`. The order is strict — `cap sync`
copies whatever is in `frontend/dist` into the Android assets, so a run
that skipped the SPA step would package a stale bundle and say nothing
about it.

The toolchain is not on PATH on the development box. Both are discovered
with the defaults below and can be overridden by exporting them:

| Variable | Default | Needs |
|---|---|---|
| `JAVA_HOME` | `~/.jdks/temurin-21` | JDK 21 |
| `ANDROID_HOME` | `~/Android/Sdk` | `platform-tools`, `platforms;android-35`, `build-tools;35.0.0` |

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

`minSdkVersion` is 26, not the Capacitor template's 23, because the
barcode scanner's native library declares 26 and the manifest merge
across that gap fails the build outright. `android/variables.gradle`
argues it in place.

## What is committed

The generated `android/` tree is committed, minus build outputs,
`local.properties`, and the `assets/public/` copy of the SPA bundle
(`.gitignore` says which and why). A tree regenerated on every clone
cannot hold the edits native configuration needs, and `minSdkVersion` is
already one of them.

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
plugins register, that the back button arrives — needs an emulator:
`make e2e-android`, which exits clean when there is none.

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
