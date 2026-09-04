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

- **Android on a real Pixel 9 over wireless adb** — sideload, lock, back,
  bundle staging, push. Only the emulator half is machine-testable here;
  push's last hop needs `google-services.json` and a real Firebase project.
- **Cross-device convergence** — read markers, approvals, quiet-when, and
  `sendId` echo-suppression observed live between the owner's two devices.
