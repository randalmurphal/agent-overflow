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

**One bundle.** There is no shell flavour of the SPA. The APK syncs
the ordinary `frontend` build, and that same build is what the backend
embeds and serves to a paired phone (`internal/bundle`), so the native
seams have to be IN it: the Capacitor packages the seams import
(`@capacitor/app`, `@capacitor/barcode-scanner`,
`@aparajita/capacitor-biometric-auth`, and `@capacitor/core` for the
`registerPlugin('Bundle')` / `registerPlugin('Push')` plugins that have
no npm package) are dependencies of `frontend/` as well as of this
package, pinned to the same versions. The seams issue those imports
only behind `isNativeShell()`, and each is a dynamic `import()`, so on
the desktop they are chunks nobody fetches. The first device run
(2026-09-03) is what retired the earlier `AO_SHELL=1` alias-to-a-stub
arrangement: the phone downloaded the backend's bundle, found every
seam stubbed, could not confirm the launch healthy, and rolled back.

Adding a plugin means adding it to both `package.json` files at one
version; `cap sync` reads this package's list to wire the native side.

`make apk` rebuilds `frontend/dist` on its way, exactly as `make build`
and the harness targets do; it is the same bundle, so nothing that
embeds `dist` afterwards is affected.

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
from `native/boot.ts` after the app has mounted. Getting that far is the
check: the module graph loaded, `main.ts` ran to its end, the app
rendered, and the plugin answered. **Reaching the backend is not part of
it**, on purpose. A phone launched with no network would otherwise roll
back a working bundle, record its id in `rolledBack`, and refuse it on
every later hello — stranded on the old app until the desktop built a
newer one. A bundle that boots but cannot talk shows the transport
banner, which is that problem's own surface.

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

### What the shell decides, and where

`native/bundleSync.ts` is the whole JS half: it watches every attached
backend's hello (through `stores/transportStatus.svelte.ts`, because a
wire subscription lives in `stores/`), decides, downloads over the paired
session, and hands the bytes to `stage`. Four things about it are worth
knowing before changing it:

- **Which backend.** The newest attached one — highest `bundleVersion`,
  home on ties and whenever the versions do not parse. One app cannot run
  two bundles; picking home would strand a phone on an old desktop, and
  picking the most recently attached would make the answer depend on
  pairing order.
- **The decision is a pure function** (`decideBundleSync`), one row per
  case, and each row has a unit test. Add a case by adding a row and a
  test, not by adding a branch to the driver around it. That includes the
  per-bundle attempt cap: `attempts` is an INPUT and `exhausted` is a row,
  because a failed attempt has to answer the very next hello as well as
  its own retry timer. While the cap lived only beside `scheduleRetry`,
  the decision kept answering `download`, `run()` re-evaluated after every
  failure, and a backend serving a bundle this phone cannot take was
  refetched with no delay and no end. A limit the pure decision cannot see
  is not a limit.
- **Only a SUCCESS re-evaluates from `run()`.** After a failure the retry
  timer owns the next look, so the one path with a delay on it is the only
  path back.
- **A phone running the APK's own bundle knows its id from
  `bundle-id.txt`**, written into `frontend/dist` at build time by
  `frontend/scripts/bundleId.ts` with the same rule `internal/bundle`
  uses. The two implementations are pinned against each other by one
  fixture directory and one golden id under `internal/bundle/testdata/`.
  Without that file a phone that has never staged anything would download
  the bundle it is already running, on every connection, forever.
- **Two sentences, and nothing else is ever shown.** Downloads,
  verifications, retries and rollbacks are silent; the person hears about
  a bundle only once it is staged and a restart would pick it up, or when
  this APK is below a backend's `minShellBuild`. Both live in
  `stores/bundleNotice.svelte.ts` and render through the transport
  banner.

### Two dependencies worth knowing

`state()` reads the APK's own `versionCode` itself rather than through
`@capacitor/app`, so this seam has exactly one native dependency: the
shell compares that number against the bundle's `minShellBuild` before
downloading, and a seam that needed a second plugin to answer would be a
second plugin an update could not ship. And `android.jar`'s `org.json`
is a stub whose methods throw, so `app/build.gradle` puts the real
implementation on the unit-test classpath (`orgJsonVersion`); nothing
ships it to a device.

## Push

The backend wakes this phone when it is not connected
(`docs/specs/remote-access.md` §9, "Push"). `internal/push` composes the
message, `internal/app/app_push.go` decides which phones get it, and
`mobile/android/.../push/` is the renderer. `frontend/src/lib/native/
push.ts` registers this device's token with every attached backend, and
`stores/pushPresenter.svelte.ts` is the in-app half.

**Our own plugin, not `@capacitor/push-notifications`.** That plugin
builds tray notifications only for Google-composed `notification`
messages, and it cannot cancel one — and cancelling is half of this
feature, because a notification about a turn that has since resumed is
exactly the stale alert retraction exists to prevent.

### Three classes, and what each is for

- **`TrayNotifier`** — the one place a notification is built or
  cancelled, and the only place that decides anything. Takes a `Tray`
  interface and no Android type, exactly as `BundleStore` takes a
  directory, so `TrayNotifierTest` is a plain JVM JUnit test that `make
  apk` runs before it assembles. Used by BOTH paths (the pushed message
  and the in-app presenter), so the two cannot drift apart on channel,
  tag, or what a retraction cancels.
- **`AndroidTray`** — the platform half: the builder call, the channel,
  and the launch intent. Deliberately thin, because it is the part only a
  device can prove.
- **`PushMessagingService`** — Google's delivery callback. Every message
  reaches it, foreground or background, because the backend sends
  data-only messages; `onNewToken` writes the token down and then offers
  it to the bridge.
- **`PushPlugin`** — the bridge: the permission prompt, the token, the
  in-app `present` / `retract`, and the `tap` event.

### The foreground drop

A pushed PRESENTATION is dropped while the app is on screen: the socket's
own `notification:send` is already showing it, exactly as on the desktop,
and posting a tray notification over an app the person is looking at is
the double notification this seam exists to avoid.

A RETRACTION is never dropped. The notification being withdrawn was
posted while the app was in the background, and coming to the foreground
does not take it off the tray — gating the withdrawal on the flag that
gated the posting would strand exactly those notifications forever. Same
rule, same reason, as `notifyOS` on the backend.

The foreground flag is a static on `PushPlugin`, set from its own
resume/pause hooks rather than through `androidx.lifecycle:
lifecycle-process`. Same answer, one fewer dependency, and it fails in
the right direction: a process that was killed comes back with it false,
so a message arriving before the app is up is POSTED rather than dropped.

### The tag is `<backend>|<id>`

`manager.notify(tag, 0, …)` and `manager.cancel(tag, 0)`, where the tag
is the sending backend's identity and `internal/notify`'s stable id
(`thread:<id>`, `approval:<thread>:<request>`), composed by
`TrayNotifier.tagFor` from the message's `backend` and `id` keys. The id
is what makes a later state change REPLACE a notification instead of
stacking a second one beside a fact that is no longer true, and what lets
a retraction cancel exactly the right one; the backend is what keeps two
machines' identical ids apart on one tray, and what lets the socket
presenter below compose the SAME tag for the same moment. The
`PendingIntent` request code is that tag's hash for the same reason: one
shared request code would hand the second notification the first one's
extras and open the wrong thread.

### The target is opaque here

The tap route crosses as ONE JSON string, from the message to the intent
extra to the web layer, and Java never parses it. Its field names belong
to `internal/notify.Target` and to the page's `parseNotificationTarget`;
a third spelling in Java would be a third thing to keep true, and this
side needs none of them to carry it.

Both intent doors are read, and they must be: `handleOnNewIntent` for a
tap on a running app, and `getActivity().getIntent()` in `load()` for a
tap that woke a DEAD one. Reading only the first loses every tap on a
phone that had been idle, which is most of them. The cold-start tap is
held until the page asks for it with `takePendingTap`, because `load()`
runs long before any listener is attached and an event fired into nobody
is a tap that opened nothing.

### The web side: registration, presentation, and the tag they share

`native/push.ts` owns ONE fact and its lifecycle: this launch's
registration token, and which backends have been told it. Registering is
idempotent — the backend keys one row per device — which is what lets
"a backend was attached" and "the token rotated" both answer with the
same call and no bookkeeping about which pairs are already done. A
DENIED permission is remembered for the launch and asked again on the
next cold start: no prompt storm, no permanent refusal, and restarting
the app is a thing people already do.

**Registration starts when the lock first passes, not at boot.** Its
first act is the `POST_NOTIFICATIONS` prompt, and a platform prompt
raised while the lock's own credential prompt is up is two system
dialogs on one screen: whatever the person types goes to the one on
top. That is how every unlock in the emulator smoke failed on the first
device run (2026-09-03), and it is the general rule, not a push one —
anything that raises a platform dialog on boot waits for the gate
(`boot.ts`, `onceUnlocked`). The presenter and the tap route raise
nothing and start at once. The smoke pre-grants the permission with
`pm grant`, so its runs never see the dialog at all.

Withdrawing is the asymmetric half. `unregisterPushFrom` is installed as
a step in `transport/detachSteps.ts` rather than watched for here,
because it is an RPC over the very socket being taken away: a phone that
has already let go has no way left to say "stop waking me". Both removal
doors run it — a machine detached in Settings, and `unpairHome()`.

`stores/pushPresenter.svelte.ts` is the other end of the foreground drop,
on the socket path: it presents `notification:send` only while the lease
is `background`, and never gates a retraction. **Its tray tag is the same
`<backendId>|<id>`**, composed from the frame's origin — the backend's
identity, which is the string the fan-out stamps on the pushed message as
`backend`. That is what makes a backgrounded phone whose socket is still
alive show ONE notification for a moment it was told about twice, and
what keeps `provider-auth:claude` from two machines as two notifications.
The rule has one home, `push.TrayTag`, and two mirrors; a change to one
is a change to three.

Settings → Notifications carries the owner's half, all of it
`access:admin`: the status line so "my phone stopped buzzing" is
answerable from somewhere other than the machine, and the credential
field, step-up gated on top, so the key can be installed on a serve host
nobody sits at.

### `google-services.json`, and this box

The file lives at `mobile/android/app/google-services.json` and is
**gitignored**. The owner obtains it from the Firebase console for the
app's own project (Project settings → Your apps → Android app
`dev.agentoverflow.app` → google-services.json) and drops it there; the
matching service-account key goes into Settings → Notifications → Phone
push on the owner's backend.

`app/build.gradle` applies the google-services plugin only when the file
exists, so **an APK builds and its JVM tests pass without it** — which is
this development box's state, and the state `make apk` is verified in.
The `firebase-messaging` dependency itself is UNCONDITIONAL, and that was
checked rather than assumed: the library needs the plugin to INITIALISE,
not to compile. Without the file there is simply no default
`FirebaseApp`, `PushPlugin.getToken()` answers its typed
`{configured: false}`, and the web seam stops there. Making the
dependency conditional would have meant two source sets, one of which
nobody compiles.

**Only a device with that file and a real Firebase project can prove the
last hop.** Everything up to it — what the backend composes, who it is
sent to, what the tray does with each message — is covered by
`internal/push`, `internal/app`, the Playwright spec, and
`TrayNotifierTest`. The hop itself has a manual case:
`e2e/android/shell-boot.spec.ts`'s real-push test, self-skipped unless
`AO_ANDROID_PUSH_CREDENTIAL` names the service-account key (first real
delivery 2026-09-04, Pixel 9a).

## Backup and device transfer are off

Nothing this app stores leaves the device by either route Android
offers. The manifest sets `android:allowBackup="false"` and
`android:fullBackupContent="false"`, and `android:dataExtractionRules`
points at `res/xml/data_extraction_rules.xml`, which excludes every
domain from BOTH `<cloud-backup>` and `<device-transfer>`.

**The reason is identity, not privacy.** The shell holds a device
signing key (`frontend/src/lib/transport/deviceKey.ts`, IndexedDB in the
app's own data directory) and the rotating session minted against it.
The backend believes one `devices` row is one machine: a cloud restore
or a phone-to-phone transfer would put that key on a SECOND handset, so
two machines would present one identity, revoking either would revoke
both, and reuse detection on the refresh secret would end the family the
moment the other one dialed.

Two things that look redundant and are not. `allowBackup="false"` alone
is NOT enough on API 31+ — it disables cloud backup and leaves
device-to-device transfer running, which is what the rules file turns
off — and `fullBackupContent="false"` is what covers the devices below
31 that never read the rules file. And every domain is named on both
sides of the rules file because an unlisted domain is INCLUDED; there is
no wildcard, so a domain added to the app later needs a line here.

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
| `native/lifecycle.ts` | pause/resume to `setClientLease`, hardware back as one stack (`answerBackPress`: Escape, terminal drawer, on-screen companion, list, exit) |
| `native/lock.ts` | the app-lock gate: covers on PAUSE, prompts on cold start and on a resume past the window |
| `native/qr.ts` | `scanPairingQr()` |
| `native/pickers.ts` | a documented stub |
| `native/boot.ts` | what runs before anything mounts; `adoptPairingEndpoint` is the one place both pairing doors (scanned code, `#pair=` hash) point the shell at a backend |
| `native/bundleSync.ts` | the one door for downloading a newer bundle from an attached backend, and for reporting this launch healthy (§ The bundle plugin) |
| `native/push.ts` | permission, token, `RegisterPushToken` on every attached backend, and the tap route; the unregister rides both removal doors through `transport/detachSteps.ts` (§ Push) |
| `stores/pushPresenter.svelte.ts` | the socket's half of the tray: background-only presentation, ungated retraction, and the tag both paths share (§ Push) |

Two things `main.ts` keeps true for the shell. The `#pair=` hash is
checked BEFORE the first-run screen for every client, so a shell can be
handed a pairing link (an app link, or the emulator smoke navigating to
one) and takes it. And the lock screen is a FIXED, full-bleed element
mounted after the app's root with the root marked `inert` while locked:
the app underneath stays mounted and warm, and nothing under the paint
can take focus or a tap.

The cover goes up on PAUSE, not on resume, and that is the half a lock
is easy to get wrong. A gate that waits for resume leaves the app's own
pixels in the task switcher's thumbnail for the whole time the phone is
away, and on screen for the frame between the window being shown again
and the resume handler running. What resume decides is only whether to
PROMPT, and a prompt that was owed when the app paused (a cold start
still waiting, a dismissed prompt) is still owed when it comes back,
however short the trip: the cover and the debt are two facts, and
`lock.ts` keeps them apart. The prompt itself is an ACTIVITY of its own,
so it pauses this app on the way up and resumes it on the way down, and
on Android that resume lands after the answer: `lock.ts` marks the
pause a prompt caused and lets the matching resume through untouched.
Read as a trip, that resume raised a second prompt after a passed one
(the success had cleared the pause timestamp, so it looked like a cold
start) and re-raised a dismissed one on the spot, with no way out of it
short of killing the app (2026-09-03, the first device run). There is
deliberately NO `FLAG_SECURE`:
the 2026-09-03 ruling is bank-app behaviour, a timed re-lock and
nothing more, and a person may screenshot or record their own
threads. The task switcher shows the cover, which is the lock screen,
because the cover is painted before the OS takes the thumbnail.

Read `frontend/src/lib/native/lifecycle.ts`'s header before touching the
lifecycle: the pause signal is the ONLY visibility signal this client
sends and it is not `document.visibilityState`, and the back button
closes an overlay by dispatching Escape through the keybinding path
rather than by holding a registry of overlays — then, in order, closes
the terminal drawer, closes the companion on screen and reveals its
thread, returns to the list, and only from the list leaves the app.

## Verifying without a device

`make e2e` covers the cross-origin transport for real, in a real browser,
against the real Go server: `e2e/tests/compact-shell-origin.spec.ts`
serves `frontend/dist` from a second HTTP server so the page and the
backend are genuinely different origins. That is the half of this shell
that can be tested on a laptop.

The other half — that the bundle boots under the fixed origin, that the
plugins register, that the app lock gates the app, that the back button
arrives, that a notification tap's extras cold-launch onto the right
thread — needs an emulator: `make e2e-android`, which exits clean when
there is none. It drives the shell's own WebView through Playwright's
Android API rather than a Chromium of its own; `e2e/AGENTS.md` §
The emulator smoke owns the details. Its first device run was the Mac
pass (2026-09-03, an arm64 android-36 emulator): five shell defects the
unit suites could not reach, all fixed in that pass, and green since.

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
- **At-rest protection for what the shell stores.** Three facts, stated
  plainly because their absence is easy to misread as coverage:
  1. The device key is a WebCrypto P-256 (ES256) key pair created
     non-extractable and kept in IndexedDB
     (`frontend/src/lib/transport/deviceKey.ts`). Non-extractable means
     the private half never leaves the WebView as bytes, so it cannot be
     copied out by page code. It is NOT hardware-backed: it lives in the
     app's own data directory, and it is protected by the Android
     sandbox and by full-disk encryption, not by the Keystore or by any
     user-presence check.
  2. The thread replica (`ao-replica-<backendId>`) is NOT encrypted at
     rest. It holds message text, tool output and diffs for every thread
     the phone has opened, in the clear, in the same data directory.
  3. The biometric lock (`frontend/src/lib/native/lock.ts`) is an
     independent prompt in front of the UI. It gates the SCREEN. It is
     not the key's gate and not the replica's gate: neither one is
     sealed by the ceremony, so passing it is what reveals the app, and
     failing it does not make the stored bytes any harder to read.

  The target design, when this is picked up: mint the device key inside
  the Android Keystore with `setUserAuthenticationRequired`, so the
  biometric ceremony becomes the key's own unlock rather than a screen
  in front of it, and the phone stops being able to sign a device proof
  while locked. That needs a native plugin (WebCrypto cannot reach the
  Keystore), which is why it is not a small change and why it is named
  here rather than half-built. Seal the replica the same way, with a key
  wrapped by that Keystore key, so its contents follow the same unlock.
  Until both land, the honest claim in any UI copy or spec sentence is
  that a phone whose disk is readable and whose lock screen is off gives
  up the replica; do not write copy that implies the biometric prompt
  protects the data.
