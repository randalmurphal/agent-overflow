# Handoff: macOS / phone / tailnet validation of the remote-access campaign

Written 2026-09-03 on the Windows/WSL box for the agent on the owner's Mac.
Everything below is verified against `git log` on `main` at
`9550c2e99`; nothing refers to a conversation you can see.

## 1. Goal state

Pull `main`, and take the remote-access campaign through the finish line
on the legs this repo could not exercise on the WSL box: the native macOS
app itself, the embedded browser pane's WKWebView engine (never compiled
or run anywhere yet), the Android shell on a real phone (APK build,
sideload, pairing, push, bundle staging), and reach over the owner's real
tailnet. Fix what breaks, with the same standards as the rest of the
repo (root cause, class closed, docs swept, gates green), commit to
`main`. iOS is later and out of scope for this pass.

Solo owner, no CI, no PRs, agents commit to `main`. Other people run
this app now, so nothing lands unverified.

## 2. What changed (recent `main`, oldest first)

Remote-access campaign:

- `3564c95a8` Merge branch 'remote-access' into main. The whole campaign
  (pairing, DPoP sessions, scopes, per-device state, tailnet listener,
  serve mode, phone shell, push, multi-backend clients, review pane over
  the wire, browser pane per platform). Spec: `docs/specs/remote-access.md`
  (§16 Phases has a LANDED paragraph per wave, each ending in "Residuals,
  recorded and left"); boundaries and the threat posture:
  `docs/specs/remote-access-boundaries.md`; rulings: `docs/decisions.md`
  § Remote access, browser pane, phone, and spec §18.

Post-merge regressions found on the WSL box, all fixed on `main`:

- `004a7bac2` build: frontend deps follow the lockfile.
- `9f835d2b9` fix(native): the notification tap route imports its two
  stores statically.
- `e02948a97` feat(notifications): "Quiet when" is one picker with four
  readings (`notifyQuietWhen`), replacing two toggles that never shipped.
- `2cd71c311` fix(panes): a draft pane restates the watched-thread set
  when it adopts its created thread.
- `a56b1c1e8` fix(scroll): selection tracking reads the button from the
  pointer stream, never a mousedown latch.
- `08a6b5c60` fix(memory): the idle renderer trim waits for host presence
  instead of reading it at mount. ~50 MB idle renderer regression: the
  home answer (`hasScope('host')`) resolves from the bootstrap manifest
  AFTER App mount, and three mount-time readers took the pre-answer
  `false` as final. `pageGrantsResolved()` in
  `frontend/src/lib/transport/scopes.ts` is now the wait; a read before
  the answer outside a tracking context throws in tests. Doc:
  `frontend/src/lib/transport/AGENTS.md` "The home answer RESOLVES LATE".
- `835c98deb` docs: product rulings live in `docs/decisions.md`.
- `9550c2e99` fix(window): revealing the app no longer un-maximizes it.
  An OS-notification click or a second launch called Wails
  `Window.Restore()`, which un-maximises a maximized window. Now
  `uiwindow.Reveal` (Show, UnMinimise only if minimised, Focus) is the
  one reveal path; `internal/uiwindow/reveal_test.go` fails any
  `.Restore()` call in a file importing the Wails application package.

Review-pane and markdown work also landed after the merge
(`378fe970f`..`22aced5fd`); not remote-specific, just be aware the tree
moved there too.

## 3. Legs that only this Mac can answer

Ordered by how much is unknown. Each names the doc that owns the
contract; read it before touching the code.

1. **Native macOS boot of the merged app.** `make build`, run it, and
   watch for the two things the spec flags for a Mac first-look:
   the CSP under WKWebView (`transport.CSPProduction`; verified in
   Chromium only, spec §16 phase 0 "A baseline CSP": "the WKWebView leg
   is verified by spec reading only and is the first thing to eyeball on
   a Mac boot"), and the page-ticket cookie exchange (`?t=` one-time
   ticket → `ao_page_<port>` HttpOnly cookie; `internal/uiwindow/pageticket.go`,
   `internal/pagehost`). A blank first paint, a missing theme stamp, or a
   `securitypolicyviolation` in the webview console is a real defect.
   Also confirm the reveal fix on macOS: maximize (green button, not
   fullscreen), click a notification, the window must stay maximized.
   `uiwindow.Reveal` uses `UnMinimise` which is `deminiaturize` there.
2. **Browser pane, WKWebView engine.** `internal/browser/wkwebview_*_darwin.go`
   and `wkwebviewglue_darwin.m` were written to the driver contract in
   `docs/specs/embedded-browser.md` §6 and have NEVER been compiled: the
   WSL box cannot build darwin cgo. The spec says outright to "expect a
   fix-up pass". Gate: `AO_HARNESS_REAL_BROWSER=1 make harness-window`
   opens an isolated instance on the real engine. Then the live checklist
   in the spec §10 (audio/video, site context menu, upload, download,
   dialogs, clipboard both ways, show-in-folder, devtools/inspector,
   HiDPI, overlay clip, workspace login isolation). Engine notes worth
   knowing before you start: `internal/browser/AGENTS.md` § The WKWebView
   engine (macOS) (`-callAsyncJavaScript:` needs macOS 11,
   `+dataStoreForIdentifier:` needs macOS 14 and is the only per-workspace
   site-data isolation; zero identifiers is success on 11–13; WKWebView
   ships no dialogs or open panel of its own, so unimplemented delegates
   silently no-op; downloads are `WKDownload`). `make go-test` on macOS
   also runs `wkwebview_engine_darwin_test.go` for the first time.
3. **Android shell on a real phone.** `make apk` has never run (the WSL
   box has no JDK); `mobile/AGENTS.md` § Building lists the toolchain
   (JDK 21, SDK 36) and the strict order (SPA → `cap sync` → JVM tests →
   `assembleDebug`). `make e2e-android` (emulator smoke,
   `e2e/android/shell-boot.spec.ts`) was written against the Playwright
   Android docs and has NOT been executed; `e2e/AGENTS.md` § The emulator
   smoke owns it, including why the device reaches the backend at
   `127.0.0.1` over `adb reverse` and not `10.0.2.2`. Then the real
   device: sideload the debug APK, pair from Settings → Remote access
   (`PairDeviceModal.svelte`; the phone scans the QR, redeems over LAN,
   both sides show the confirmation number, the desktop confirms), the
   app lock, back button, a bundle stage + the 30-second watchdog
   rollback, a push notification tap cold-launching onto its thread
   (needs `google-services.json` on the box; `mobile/AGENTS.md` § Push).
   Two things the docs say are unverified until a device: whether the
   Android WebView raises `contextmenu` on long-press (the touch-menu
   detector, spec §16 wave R4) and sheets under the software keyboard.
   `make apk` leaves `frontend/dist` holding a SHELL bundle; rebuild
   before hand-running any Go binary that embeds it.
4. **Real tailnet.** `internal/tailnet` (tsnet, single-use `Node`,
   status not `Up`, state dir `<config root>/tsnet/` IS the node identity;
   `internal/tailnet/AGENTS.md` § The three properties). Enable it in
   Settings → Remote access (`NetworkTailnetEditor.svelte`), sign in via
   the `AuthURL` the status shows, and pair the phone over the tailnet
   from off-LAN. Confirm disable keeps the state dir and Forget deletes
   it, and that a restart (new Node, same dir) keeps the same node
   identity in the admin console. Then `agent-overflow serve` + the
   service verb on launchd (`docs/architecture/serve-mode.md`; credential
   posture is 0600 files under the config root, no keychain) and attach
   the desktop app to it from the same Mac and from the phone.
5. **Cross-device convergence, two real screens.** Mac desktop + phone on
   the same backend: read markers (mark-unread on one shows on the other,
   spec §16 wave R3), an approval answered on one clears on the other,
   the "Quiet when" picker (`notifyQuietWhen`, device tier, default
   `focused`) actually silences the Mac toast while the thread is on
   screen, and a send from the phone that loses its socket mid-send is
   answered once (sendId, wave R6).

Residuals the campaign deliberately left, so you do not re-find them as
bugs: every "Residuals, recorded and left" clause in spec §16 (a
`localStorage` write failing mid-renewal is only logged; the global
64-entry ticket ring; one pane per compact client is UI-enforced only;
header `xs` buttons at 24px; edit-and-resend `executingThreads` is per
page load; an idle-reaper close drops a requeued message; the send-id
lookup is a newest-64 window; the review-comment nudge is wildcard;
`backends.ts` and `app_preview.go` flagged for a split when next touched).
Fix one only if it bites on the device, and say so in the commit.

## 4. What to re-run

- Every task: `make go-build`, `make go-test`, `cd frontend && pnpm run
  check && pnpm run build && pnpm test`. On macOS use the Make targets,
  never bare `go build`/`go test` (the Makefile exports the cgo
  deployment-target flags Wails needs).
- `make e2e` once on the Mac (desktop + compact Playwright projects
  against the mocked harness; 6 GiB reserved).
- `make verify` before any release artifact.
- `make provider-smoke` only with the owner's OK: it spends real tokens
  and needs logged-in `claude` and `codex`.
- Bindings: any new `//ao:scope`-annotated App method needs
  `wails3 generate bindings -ts` and `methodgen` (see
  `internal/transport/AGENTS.md`); `frontend/bindings/` is never edited
  by hand.
- Verify every fix after a full app restart, not on a hot-reloaded page:
  the memory-trim and notification paths only take effect at boot.

## 5. Ruled out / already decided

- No public exposure of the personal backend: no Funnel, no cloudflared,
  no `public` session class. Reach is loopback, LAN, the owner's tailnet,
  all trusted alike (spec §18, ruled 2026-08-31).
- Release signing is cut; the sha256 sidecar over HTTPS is the trust
  line (ruled 2026-09-01). Debug signing for the APK is intended.
- Push is owner-only; nothing multi-person (relay, team sharing) is
  designed before named accounts exist (spec §18 items 1 and 5).
- Browser pane: an embedded real engine per platform, never a streamed
  or remote fallback (`docs/decisions.md`).
- The `Restore()` reveal bug is Wails' documented semantics, not a
  platform quirk; don't reintroduce `Restore()` anywhere.
- The idle memory regression was the late-resolving home answer, not the
  remote code's allocations; the trim itself was sound.

## 6. Landmines

- **Tests must never reach a real provider binary or the real
  `~/.claude` / `~/.codex`.** Root `CLAUDE.md` § Permanent invariants
  and `internal/kerneltest/AGENTS.md`. A leaked real session burned an
  OAuth grant before. New fixtures wire into `kerneltest.IsolateSpawns`.
- The tailnet state dir and the session signing key are key material at
  rest; never copy, back up, or serve them (`internal/tailnet/AGENTS.md`).
- `.claude/` and `.playwright-mcp/` stay excluded from the Wails dev
  watcher (`build/config.yml`), or the fsnotify storm kills `make dev`.
- Never `git checkout`/`stash`/`reset` on tracked files to undo an edit;
  the tree may hold another session's uncommitted work. Undo by forward
  edit.
- Deliberate behavior has history: `git log -S` before "fixing" something
  that looks wrong; rulings are in `docs/decisions.md`.
- Visible UI changes need the owner's approval; perf and fix work keeps
  pixels identical.
- Every change sweeps `**/AGENTS.md` and `docs/` for claims it falsified,
  in the same commit (`docs/architecture/conventions.md` § Maintaining
  the Guides). The emulator-smoke and `make apk` "never run" claims in
  `e2e/AGENTS.md`, `mobile/AGENTS.md`, `docs/specs/embedded-browser.md`
  §10 and `docs/specs/remote-access.md` §16 become false the moment you
  run them: update them.


## 7. Outcome — the Mac pass (2026-09-03)

Run on the owner's Mac against the isolated harness/serve instances, never
the live app. Everything the WSL box could not exercise is now exercised;
the legs still open are the two that need a second physical device or the
owner present, called out below.

Done and green:

- **Native macOS app.** CSP clean in the webview console, page-ticket
  cookie round-trips, and the `uiwindow.Reveal` maximize-stays-maximized
  fix confirmed (deminiaturize, no `Restore()`).
- **Embedded browser pane (WKWebView).** Compiled and driven for the
  first time on the real engine; the fix-up pass the spec predicted
  found four engine-side gaps, all fixed (`docs/specs/embedded-browser.md`
  §6 records them). Live checklist scripted on the isolated instance.
- **Android.** `make apk` builds (JDK 21 / SDK 36), the JVM unit suites
  pass, and `make e2e-android` drives the shell inside an arm64
  android-36 emulator — five shell defects the unit suites could not
  reach, all fixed. First device run for both.
- **Tailnet, serve mode, launchd service verb.** Enabled on an isolated
  instance, signed in over the owner's real tailnet, identity survives
  restart, disable keeps the state dir and Forget deletes it; the
  `service install|status|uninstall` round-trip works on an isolated
  data dir; desktop and phone both attach.
- **Gates.** `make go-build`, `make go-test` (173 ok), `make e2e`
  (193 passed), and `make verify` all green. `make e2e` had three
  macOS-only failures on arrival, all pre-existing and none in campaign
  code, fixed at the root in `194612b9` (chat-profile leak across the
  harness reset; a provider-signin assertion racing the claude keychain
  probe hold; the awaitNoPages teardown budget). `make provider-smoke`
  spends real tokens and was left for the owner.

Left for the owner (need a second device or hands on the phone):

- **Cross-device convergence** — read markers, approvals, quiet-when, and
  `sendId` echo-suppression observed live between the owner's two devices.

## 8. The real-phone pass (2026-09-04)

The Android leg closed the next morning, on the owner's Pixel 9a over
wireless adb with the owner answering their own lock prompts. All four
smoke cases green on the FCM-enabled APK: boot/pair/unlock/navigate,
bundle staging (a genuine swap — the installed APK's bundle differed from
the backend's), the notification-tap route, and the push last hop — a
real message through the owner's Firebase project and Google into the
tray, redaction rule observed on the far side.

Two additions made it repeatable rather than a one-off:
`AO_ANDROID_HUMAN_LOCK=1` (the suite waits for the owner instead of
typing the emulator PIN at a real lockscreen, and wakes/waits for the
keyguard — a dozing phone renders frozen frames, so every click stalls
"waiting for stable") and the self-skipping real-push case behind
`AO_ANDROID_PUSH_CREDENTIAL` (a manual gate in the `provider-smoke`
sense). `google-services.json` sits gitignored at
`mobile/android/app/`; the service-account key stays outside the repo.

## 9. The phone-UX batch (2026-09-04)

The owner's first hands-on pass with the paired phone produced seven
findings, each fixed at the root and each with a test:

- **Banner clamp**: `TransportStatusBanner` wraps as far as it needs
  to; a phone has no hover to reveal a clamped sentence.
- **Editor-open on the phone**: the surface was already `host`-gated;
  the phone counted as host because `adb reverse` makes it a loopback
  peer. `transport/scopes.ts` now answers `host` false for every paired
  session (spec § Principal tiers), server presence untouched.
- **Header**: the desktop cluster (and the command-palette button) is
  one menu behind `chat-header-more` on compact; the title gets the
  width. A dropdown at the button, not a bottom sheet (owner: "where I
  am clicking", second phone pass). `GitActionsControl` gained
  `trigger={false}` + `openMenu()` so its popover and dialogs stay
  mounted outside the menu.
- **Composer meters**: the `minimal` rung keeps the model and the
  meters and folds the other pickers into `ComposerPickersRollup`; the
  mode row shares `agentModeCycle.ts` with the toolbar button. The
  first cut gave the picker box `min-w-0`, which let the pickers
  overlap the meters while the density ladder still read the row as
  fitting (the phone sat at `compact` with controls painted over each
  other); the box is `shrink-0` now and the e2e case asserts no two
  toolbar controls overlap.
- **Back stack**: `native/lifecycle.ts#answerBackPress` — Escape
  (Settings page → rail through `escapeSettingsOverlay`), terminal
  drawer, on-screen companion (closed, thread revealed), list, exit.
  The first on-device run read "back from review" as a failure; it was
  the soft keyboard eating the press (opening a thread focuses the
  composer, which raises the keyboard, and Android answers the first
  back by closing it). The emulator run with a keyboard-aware press
  passes every rung. Whether a thread should auto-focus its composer
  on a phone at all is an open question for the owner: it costs a
  keyboard on every open.
  Focused-terminal Escape goes to the pane, never into xterm.
- **Sidebar chord pill**: shown only while a modifier is held, the
  jump-hint door.
- **Keyboard cut-off**: `index.html` asks for
  `interactive-widget=resizes-content`, and the scroll observers re-pin
  an idle pinned reader when the viewport shrinks. Viewport-only samples
  now use the controller's shared composer-geometry policy: an active send
  glide owns the remaining distance instead of being instantly landed.

The scroll spring's absence on the phone was not reproduced on a
device in this pass; the hypothesis is the WebView honouring Android's
"remove animations" / battery-saver `prefers-reduced-motion`, which the
controller already respects. Check `matchMedia('(prefers-reduced-motion:
reduce)')` on the device before touching the spring.

## 10. Spring judder on the 120Hz phone (2026-09-04)

Measured on the Pixel 9a over the harness, then left alone (owner asked
for the emulator instead). Scripts in the session scratchpad
(`phone-quant-probe.mjs`, `phone-scroll-probe.mjs`). The ruling and
what shipped are at the end of this section; the findings stand as
written.

- **The panel runs the WebView at 120Hz.** Smooth Display is on
  (`peak_refresh_rate` unset = Infinity; modes 60 and 120). rAF gaps
  were 8.3ms in 1050 of 1073 frames through a streamed turn, idle or
  writing.
- **`scrollTop` writes snap to DEVICE pixels there, not whole CSS
  pixels.** Writes at 0.25px steps read back on a 0.381px grid
  (1/2.625). The "Chromium quantizes to whole CSS pixels" premise
  behind `SPRING_QUANTIZED_MOTION_FLOOR_PX_PER_FRAME` is a DPR-1
  observation; the spring's `wholePixelQuantizationConfirmed` latch
  correctly never engages on the phone, so every tick writes.
- **Why it judders.** The floor and the slew ramp base are 1 CSS px
  per 60Hz frame, integrated by real elapsed time (by design; the
  cadence EMA is telemetry only, so this is NOT a refresh-rate
  detection bug). At 120Hz the slow band is 0.5 CSS px per frame =
  1.31 device px, and the decel envelope's 1.6 → 1 CSS px/60Hz-frame
  band is 2.1 → 1.3 device px per 8.3ms frame. A non-integer
  device-pixel rate snaps to alternating step sizes: the trace shows
  consecutive frames of `1 1 1 2 1`, `2 3 2 3 2`, `3 4 3 4` device px —
  a 33–100% frame-to-frame swing at low speed, which the eye reads as
  jitter without lag (the average rate is right). On a DPR-2 60Hz Mac
  the same floor is exactly 2 device px every frame and the decel band
  is 3.2 → 2, so the alternation there is rare and mild; DPR-1 Windows
  is exact by construction.
- **Options.** (a) A device-aware floor and ramp base: the smallest
  whole number of device pixels per DISPLAYED frame that is at least
  the 60px/s rate (2 dev px per 120Hz frame at DPR 2.625 = 91px/s;
  unchanged at DPR 1 and DPR 2 at 60Hz), from `devicePixelRatio` and
  the live cadence EMA. (b) Integer device-pixel stepping in the low
  band (below ~5 dev px per frame): quantize each write to whole
  device pixels with the existing error carry, so a glide decelerates
  through uniform 4 → 3 → 2 rungs instead of alternating. (c) A
  setting. (c) is possible; (a)+(b) is structural and closes the
  class on every high-DPR, high-Hz device without a knob.
- **Ruling** (owner, 2026-09-04): avoid jitter; where constant motion
  cannot, the glide should stop rather than jitter ("I will almost
  always notice the jittering").
- **What shipped** (branch `spring-whole-pixel-motion`): (a)+(b),
  generalized. The spring writes whole grid pixels off a ladder of even
  cadences (`n` pixels a tick, or one every `k` ticks, hysteresis
  either way), the floor is a rung of that ladder derived from the
  cadence EMA and the grid (1 device px per 120Hz frame on the phone,
  46px/s; unchanged 60px/s at DPR 1 and 2 on 60Hz), it holds through
  to the landing (the sub-pixel "cradle" is gone everywhere), and the
  grid itself is witnessed from readback — device pixels until the
  engine is seen rounding to CSS pixels, which desktop Chromium does at
  every DPR. Mechanism and constants:
  [`frontend-scroll.md`](../architecture/frontend-scroll.md) § the
  `spring.ts` bullet, and the "Whole-pixel motion" block in
  `spring.ts`. Not yet looked at on the phone: the emulator runs 60Hz
  only, so the 120Hz result is unit-traced (`spring.test.ts` 'quantized
  motion floor'), not seen. Visible trade-offs to check on a device:
  the phone's floor is ~46px/s instead of 60; a 144Hz DPR-1 panel's is
  72, a 165Hz one's 82; fractional-DPR desktops on a device-pixel
  engine step whole device pixels (48px/s at 1.25×); every landing is
  firmer.
- **Mac follow-up (2026-09-04).** Two of those trade-offs were wrong
  enough to fix, and the witness hid a freeze.
  - macOS WKWebView FLOORS a fractional `scrollTop` write to the whole
    CSS pixel below (standalone Swift spike on the M2 Air, DPR 2:
    100.75 → 100, 200.999 → 200, `+0.5` repeated never moves, `+1.5`
    repeated moves exactly 1 px a write). It does not round, and there
    is no device-pixel grid. The witness above required observed MOTION
    to latch CSS pixels, so on a Retina display above 60Hz (where the
    ramp starts at 1 device px = 0.5 CSS px) every send glide sat
    frozen for 10–40 ticks a pane until a 3-device-pixel rung floored
    to 1. The 60Hz Mac never showed it: its ramp starts at 2 device
    px = 1 CSS px, and a harness send there glided cleanly (50 ticks,
    50 writes, 0 zero-step ticks). The witness now accepts an
    off-grid write that reads back on the CSS grid within a pixel,
    moved or not; a readback OFF the CSS grid (the Pixel's 1/32 px
    values) latches the device grid for good; and the witness is
    page-wide module state, so a second pane never re-learns it. The
    Pixel measurement (§ above) stands and drives the phone's rung.
  - The floor's cadence bound is 45 changes a second, not 60, so a
    165Hz DPR-1 panel runs one pixel every three frames (55px/s, the
    rung closest to the reference and to main's 60) instead of every
    two (82px/s). 144Hz keeps 72 (the ratio pick), 120 and 240 keep 60.
  - The landing cradle is back, on the grid: the last three pixel
    events run at k, 2k, 3k ticks (`SPRING_LANDING_CRADLE_EVENTS`; 0 is
    the flat stop this section shipped). What to feel-check on the
    165Hz Windows setup against main: the tail is 55px/s even instead
    of 60px/s irregular (3,3,2 ticks a pixel), mid-speed rungs plateau
    where main alternated 2,3,2,3, and the onset base is 1.375 px a
    frame instead of 1.0. Cruise is identical.
  - Harness gotcha that faked two "frozen glide" captures: the
    WKWebView harness window runs rAF only while frontmost. Reveal it
    (`bin/ao-harness rpc HarnessWindowCommand '{"action":"reveal"}'`)
    right before a timing-sensitive send.
