# Embedded Browser

Status: implemented. Both platform spikes passed every item
(verdicts + evidence: /tmp/spike-webview2-dual/VERDICTS.md,
/tmp/spike-webkitgtk-embed/VERDICTS.md); their recipes are folded in
below. macOS is built to the same driver contract and verified on a
Mac (§10). The §9 deletion wave has landed: managed Chrome, the
screencast path and its four RPCs, and the encrypted site-data
checkpoints are gone, and a windowless deployment has no engine and no
browser tools, with one exception landed in wave 9: serve mode drives a
headless system Chromium for the TOOLS alone, never a pane (§9).

## 1. What this replaces, and why

The shipped browser pane is a JPEG screencast of a headless Chrome
target: 15fps lossy frames over a long poll, input forwarded as CDP
events, an AO-drawn context menu, RPC clipboard bridges. A parity audit
(2026-08-31) found fifteen classes of missing browser behavior — no
audio, no site context menus, no real file pickers, dialogs silently
dismissed, page clipboard writes reaching nothing, no devtools, video at
15fps JPEG. The user's ruling: the streamed pane is not a browser and
dies entirely. No streamed fallback survives anywhere, including remote.

Replacement: a real embedded browser engine per platform, presented
inside the existing pane chrome (AO tab strip, address bar, toolbar),
with AO-owned per-workspace site data, and the same MCP tool surface
driving real pages. Agent pages are hidden views/targets of the same
engine, so the agent/user shared-session model is unchanged.

## 2. Engine per platform

| Platform | Engine | Where it lives | Agent-tool driver |
|---|---|---|---|
| Windows (WSL build) | WebView2 (Chromium) | second WebView2 environment in the launcher process, controllers positioned in the launcher's Win32 window | CDP (chromedp), relayed — §5 |
| macOS | WKWebView (WebKit) | subviews of the Wails NSWindow, in-process | native driver — §6 |
| Linux | WebKitGTK 6.0 (WebKit) | sibling GTK widgets in the Wails GTK4 window, in-process | native driver — §6 |
| Serve mode (any platform) | headless Chromium (a SYSTEM install, never downloaded) | one process per workspace profile, launched by this backend, no window and no pane | CDP (chromedp), in-process — `docs/specs/remote-access.md` §7 |

The engine renders real pixels in the real compositor: audio, video,
IME, drag-drop, cursor, tooltips, selects, site context menus, find,
zoom, print, fullscreen all behave as the engine ships them. Chromium
fidelity on mac/linux is explicitly out of scope (user-accepted); the
"open in system Chrome" escape hatch covers engine-specific testing.

Wails integration goes through the pinned fork
(`github.com/randalmurphal/wails/v3`; revision and patch inventory in `go.mod`),
which already carries AO-specific window/webview APIs
(`SuspendWebview`, `CallDevToolsProtocolMethod`). New fork surface:
expose the native window handle / add a child-browser-view API per
platform. Fork changes ship as focused commits on that branch.

## 3. Ownership model (unchanged where it can be)

`internal/browser.Manager` remains the policy layer: Access checks,
page registry, per-thread ownership, page/context caps, bounds on every
tool result, artifact quotas, the MCP server and its capability URLs.
What changes underneath it is the engine: instead of one managed
headless Chrome process, pages are engine pages hosted as above.

- One isolated engine profile per cookie jar (a WebView2 profile / a
  WebKitNetworkSession with a dedicated data directory;
  spike-verified isolated: cookies, localStorage, IndexedDB all
  per-session, verified across process restarts). Each thread's
  browser chooses its jar: `worktree` (the default, shared by the
  threads on a canonical workspace, with the same profile ids as
  before jars), `project` (shared by the project's threads that choose
  it, in any worktree), `thread` (this browser only, persistent) or
  `private` (always ephemeral, a new identity each time it is created,
  discarded when its last page closes). `jar.go` is the one mapping from
  (thread, choice) to a jar; engines receive only a jar key and an
  ephemeral flag. Pages stay thread-owned. WebKit views are ALWAYS
  constructed against an explicit AO-owned data dir: merely
  instantiating the default session writes into
  `~/.local/share/webkitgtk/`.
- A jar carries site data only. Navigation and file permissions, the
  record of finished downloads, and headless Chromium's download
  directories and quotas stay per workspace, so two
  worktrees sharing a project jar keep their own authority.
- Pages start hidden (invisible controller / unparented or hidden
  view). `browser_visibility` presents one page inside the pane.
- Limits: 8 pages per thread, 24 per workspace across its jars, 24
  per jar, 64 in total, and at most 12 live profiles. A tab being
  reopened by a jar switch counts once. Label rules and parallel-agent
  page-handle discipline are unchanged.

## 4. Site data

Engine-managed, on disk, per cookie jar, AO-owned directory tree
(`<data root>/browser-profiles/<jar-hash>/`; a worktree jar's hash is
the canonical workspace's, so its directory predates jars). Like a normal
browser profile: cookies, localStorage, IndexedDB, service workers,
HTTP cache — everything the engine persists, not just the two
categories the checkpoint system encrypted.

Consequences, deliberate:

- The AES-GCM checkpoint machinery (`state.go`, keyring key) is
  deleted. Site data at rest has the same protection as the user's own
  browser profile: filesystem permissions (0700 dirs).
- `browserPersistSiteData=false` maps every jar to an ephemeral profile,
  and a `private` jar is always one. The contract every engine keeps: an
  ephemeral profile is never part of a persistent profile, and its data is
  discarded when the jar's last page closes. WebKit keeps it in memory (an
  ephemeral network session, a non-persistent data store); WebView2 uses an
  InPrivate profile, whose storage the runtime manages; headless Chromium
  has no in-memory profile and uses a temporary user-data directory
  (browser-tools.md, Platform behavior).
- **Clear site data** closes every engine page, then deletes every
  jar's profile directory. Each thread's jar choice survives it.
- Old encrypted checkpoints are deleted on first boot of the new code
  (they cannot be imported into an engine profile; they were only
  cookies + localStorage).

## 5. Windows: hosting and the CDP relay

### Hosting

The launcher creates a second WebView2 environment ("pane
environment") with its own user-data folder, entirely separate from
the SPA webview's environment. Browser pages are one controller per
tab (spike-verified preferred over `Target.createTarget`: a
controller is a positionable window, a bare CDP target is not).

Spike-verified recipes, all load-bearing:

- **Z-order**: WebView2 inserts each new controller's child window at
  the BOTTOM of the host's child z-order — a fully visible pane is
  indistinguishable from no pane until
  `SetWindowPos(paneChild, HWND_TOP, …)`. The child HWND is found by
  diffing the host's child list around controller creation.
- **Env-var scrub**: `WEBVIEW2_USER_DATA_FOLDER` (and siblings)
  override the API arguments even when SET-BUT-EMPTY, silently
  collapsing every environment onto one shared profile — the pane
  would read the SPA's cookies with no error anywhere. The launcher
  clears the `WEBVIEW2_*` overrides in its own process before
  creating any environment. (Found live on this machine.)
- **One debug-port owner**: if two environments carry
  `--remote-debugging-port`, the first browser process wins the port
  and the isolation inverts silently. Only the pane environment gets
  the flag; the SPA environment never does, so app UI is not
  debuggable through it. Loopback on the Windows side only.
- `ERROR_INVALID_STATE` at controller creation means "same user-data
  folder, different browser args", not a process-wide conflict.
- **Hidden throttling**: a hidden controller's rAF STOPS (not slows)
  and timers drop to ~1Hz; CDP evaluation and input still work, which
  is how the tools read pages. Try the anti-throttling Chromium flags
  (`--disable-background-timer-throttling` etc., the incident
  2026-08-31 set) on the pane environment; if WebView2 ignores them,
  the WebKit posture (snapshot/JS-driven background pages) already
  covers it. After `PutIsVisible(true)` the page's
  `document.visibilityState` can keep reporting `"hidden"` for
  seconds — nothing gates on `visibilitychange`; the host's own
  visibility signal is authoritative.
- The visible page's controller is positioned over the pane's FITTED
  rect (the viewport scaled down to fit, centered by whole CSS pixels).
  CSS pixels convert to client pixels by the report's
  `devicePixelRatio`. The page's size is
  never the controller's: `Emulation.setDeviceMetricsOverride` pins
  the thread viewport and its `scale` draws it to fit, and screenshots
  divide the clip by the page's `devicePixelRatio` so they stay one
  image pixel per CSS pixel. Hidden pages keep `IsVisible=true` inside
  a hidden clip container: `IsVisible=false` stops compositing, which
  froze presented views and hung `Page.captureScreenshot` until the
  pane reopened.
- Per-workspace isolation: prefer one pane environment + a named
  `CoreWebView2Profile` per workspace (one browser process, one CDP
  port); verify cross-profile cookie isolation at implementation
  start, falling back to environment-per-workspace (one port each,
  same relay) if profiles leak.
- **Popups**: every window a page opens raises `NewWindowRequested`
  (`IsUserInitiated` false only for a script's call with no gesture).
  The launcher takes the deferral, creates a hidden controller in the
  opener's profile, hands its view over with `put_NewWindow`, sets
  `Handled` and completes the deferral; no window of WebView2's own
  opens, and `window.opener` and `postMessage` work. The view gets a new
  CDP target at the completion, naming the opener, which the launcher
  reports (`ReportPopup`) under a page id it mints before any other
  report of the page and after its opener's; the backend adopts or closes
  it like any page. A popup that goes before its report is reported then,
  with its downloads and its close, only when it has downloads to
  report.
  `Handled` with no new window opens nothing (`window.open` answers
  null). `window.close()` raises `WindowCloseRequested`. A popup's
  downloads reach its own `DownloadStarting`. A popup whose first
  navigation is a download asks to close, and about 35 ms later WebView2
  opens the opener's default download dialog whatever the host does: the
  popup's `DownloadStarting`, attached before `put_NewWindow` and before
  the deferral completes, sets `Handled` and reads it back set, the opener
  raises no `DownloadStarting` for it, and closing the popup inside its
  close request changes nothing. The launcher closes the dialog on
  `IsDefaultDownloadDialogOpenChanged` about 6 ms after it opens; it shows
  about 0.22 s, WebView2's close animation. Measured by
  `popup_smoke_windows_test.go` on runtime 157.
- The spike patched two real go-webview2 bugs the launcher-side
  implementation must not reinherit: `int64(res) < 0` never detects a
  failed HRESULT on 64-bit, and `environmentOptions` was hard-wired
  to 0 (making `AdditionalBrowserArguments` unreachable). Evidence
  and harness: `/tmp/spike-webview2-dual/`.

### CDP relay — no cross-boundary socket, ever

The WSL backend never dials the Windows host, and nothing listens
beyond loopback. Ruled after the first spike's reachability probing
tripped corporate-network alarms; the relay is also simply the tighter
design.

- The launcher dials one additional WebSocket to the backend (same
  direction and auth as the existing notification bridge: Windows →
  WSL localhost, launch token).
- That socket is a TCP tunnel multiplexer: the backend asks for a
  stream, the launcher opens `127.0.0.1:<cdp-port>` locally and pipes
  bytes.
- The backend exposes the tunnel as a local loopback listener inside
  WSL; chromedp's `remote.NewAllocator` connects to it. `/json`
  responses get their `webSocketDebuggerUrl` host rewritten to the
  tunnel listener.
- The tunnel accepts only connections from the backend process's own
  loopback and only forwards to the single CDP port the launcher
  registered — not a general proxy.

Existing `internal/browser` CDP code (locator, interaction,
operations, console ring, screenshots) carries over nearly unchanged on
Windows: CDP is CDP. Downloads are the exception: the launcher owns
WebView2's (see the capability table).

### Pane directives

Backend → launcher control (create/show/hide/position/close
controllers, profile selection, devtools open) rides the existing
notification-bridge channel mechanism as a new directive channel;
launcher → backend acks/events (controller created, engine process
died) ride the same connection's RPC path, like `updater:install`
already does. The launcher applies one page's directives in the order
they arrived.

A pane's position does not need the backend: the SPA and the
controllers share the launcher's UI thread. The SPA posts each geometry
report to the launcher (`chrome.webview.postMessage`, Wails'
`RawMessageHandler`) as well as to the backend, only where the launcher
advertised that it takes them. Each distinct report is numbered once
per mount and carries that number on both paths. A bounds directive
names the mount, the page's viewport and the number of the report it
was placed from; with a report for that mount at that number or later,
the launcher places the page from it with the shared rule
(`internal/paneplace`) and ignores the directive's position, and shows
the page only while the backend shows it and the report is visible. A
directive newer than the launcher's report (a failed post) stands, as
it does without one (an older SPA, backend or launcher on either side).
A directive without a number (an older backend) yields to any report.
The backend still receives every report: it owns the viewport and which
mount presents.

## 6. macOS / Linux: native driver

No CDP exists for WKWebView/WebKitGTK. The agent tool surface gets a
second driver implementation speaking the engines' own APIs,
in-process (the backend IS the app process on these platforms).

Seam: `internal/browser` gains a per-page engine interface (final
shape decided at implementation start, from what the tools actually
need — roughly: navigate/history, evaluate JS (with world/user-gesture
options), snapshot image, input dispatch, viewport metrics, download +
file-chooser + dialog + popup + context-menu delegates, console sink,
lifecycle). The CDP driver implements it with chromedp; the WebKit
driver with:

- JS evaluation: `webkit_web_view_evaluate_javascript` /
  `evaluateJavaScript`. Locator/snapshot/DOM tools are re-expressed as
  shared JS expressions where they aren't already.
- Screenshots: `webkit_web_view_get_snapshot` / WKWebView
  `takeSnapshot`. Spike-verified: snapshots work on hidden views and
  return FRESH pixels after DOM mutation.
- Input, two-tier (spike-verified): JS-driven interaction
  (`element.click()`, focus+value+events) is the default for hidden
  AND visible pages — untrusted (`isTrusted:false`) but drives
  ordinary handlers including the file-picker open. XTest (XWayland)
  escalation gives genuinely trusted mouse+keyboard for the VISIBLE
  pane only, at the cost of hijacking the real pointer — used only
  when a page demands user activation, behind explicit agent-driving
  UI. GTK4 has no synthetic-event injection (no public GdkEvent
  constructors) and hidden views are unreachable by any trusted
  mechanism; a small class of sites (payment iframes, anti-bot)
  ignores untrusted input — documented parity note for agent
  automation on mac/linux. User input is always real regardless.
- Downloads / file chooser / dialogs / popups / context menu: WebKit
  delegate signals, all spike-verified working. `download-started`
  fires on the NetworkSession, not the view; context-menu signal
  appends AO items to the real menu.
- Console: injected capture script via user content manager.
- `browser_evaluate_readonly` loses Chrome's engine-level
  side-effect rejection on WebKit; it becomes best-effort there
  (documented in the tool result, not silently different).

Hidden agent pages are hidden views. Spike-measured: an unmapped view
runs `setInterval` at ~1Hz and zero rAF, with no API override — so
background-page tooling is built on snapshots + JS evaluation, never
on the page animating itself. Loads and JS execution work normally.
Windows is unaffected. Three hiding traps are load-bearing: a
background view parks as an empty clipping box among the pane
overlay's children, which cost the window no size (a `GtkFixed` at
offscreen coordinates propagates its size and ballooned the window to
6280x5837); a view never changes widget parent between hidden and
shown, because a reparent unrealizes it and drops the frame WebKit
last drew, and a static page draws no other, so it came back empty;
and `opacity:0` is never used to hide (it kills rAF while timers keep
running).

## 7. The pane: chrome, rect sync, airspace

Presentation is unchanged in shape: AO's tab strip, address bar,
navigation buttons, close — the same `BrowserPane.svelte` chrome — with
the frame `<img>` replaced by an empty host rect the native view is
positioned over.

- Tabs render the Manager's state only: a tab being created, restoring,
  loading or failed shows that state, and the host rect says so while
  the active tab has no page to present (as built: browser-tools.md
  § Ownership and lifecycle). The strip and address row behave as a
  desktop browser's (browser-tools.md § The pane's chrome).
- The SPA observes the host rect (`ResizeObserver` + scroll/layout +
  the pane-layout and sidebar stores — a divider drag on ANOTHER pane
  slides this one sideways without resizing it, which no observer or
  event reports) and reports it over a binding, together with the
  VISIBLE CLIP INTERSECTION and the pane's resolved background color.
  A page's base color, painted wherever its document leaves the
  background transparent, is a property of the page, not of where it
  sits (`applyPageBase`): white under a document, as in any browser, and
  the presenting pane's color under a blank tab (the engine's white
  while the thread's pane never reported one). The Manager applies it,
  shown or hidden, when the page joins its tabs, when its committed
  address changes between a document and none, and when the pane's color
  changes, and a presentation pass brings it up to date before it shows
  the page, so no show overtakes a base still being applied; geometry
  carries no color. The Manager also fits the page's viewport into that rect (`placePage`:
  scale = min(rect/viewport, 1), centered by whole CSS pixels so a page
  that follows the pane starts at the rect's origin, clip intersected) and the
  platform host positions the native view at the fitted rect, drawn
  at that scale, and crops it to the clip, so a pane half behind the
  sidebar shows its visible half without the page relayouting. The
  rect also carries the SPA's `devicePixelRatio`, which the hosted
  engine folds into the CDP scale so webview zoom and DPI cancel out.
  Rect updates coalesce per frame, and each mount has at most one report
  in flight with only the newest waiting behind it, so reports land in
  order; one the backend already accepted is not resent, and a failed
  one is retried, bounded, then shown in the pane's banner. A host that
  positions in whole pixels snaps edges, never origin and size apart,
  with `floor(v + 0.5)`. On Linux each page lives in its
  own `AoViewHost` widget: an overlay child at the clip rect through
  four margins with `ALIGN_FILL`, allocating the view at exactly the
  viewport under a translate+scale `GskTransform` (spike-verified:
  `gtk_widget_set_size_request` cannot SHRINK a WebKitWebView —
  natural size sticks at the largest-ever allocation — while an
  explicit `gtk_widget_allocate` takes any size, and GTK maps pointer
  input back through the transform). On macOS the view's frame is the
  fitted rect and its bounds stay the viewport. Wails window surgery:
  ref the existing child, `set_child(NULL)`, wrap in a `GtkOverlay`,
  re-set — the SPA survives without a reload.
- **Airspace**: the native view always paints above the SPA. Any AO
  overlay (popover, modal, palette, menu) that would intersect the
  browser rect requires the view to be clipped or hidden for the
  overlay's lifetime. The existing popover-ownership layer
  (`utils/popoverOwnership.ts`) is the signal source: overlay open →
  hide/clip native view (a static snapshot of the last frame may be
  shown in the host rect to avoid a blank hole — implementation
  choice, decided by feel). This is the one place the embed is
  visibly not-a-DOM-element; every native embed has it.
- Pane hidden / thread switched / layout drag in progress → view
  hidden. A drag that places a pane (a sidebar thread row or tool, a pane
  title) hides every view in the strip from its `dragstart` to its end,
  under the strip's drag shield (an airspace surface), because a view
  takes the drag from the page: nothing later than the `dragstart` is
  sure to reach the SPA. Nothing is torn down; page state lives on, the page keeps
  its viewport and keeps producing frames. By default the viewport is
  the pane's own size (`viewport.go`: the rect's whole pixels within the
  viewport bounds, applied latest-wins per thread), so a presented
  page is at 1:1 and reflows with the pane. A resize lays out only the
  presented page; another page of the thread takes the viewport before
  it is shown or a tool acts on it, and the thread's state is pushed once
  the resize settles. Moving the pane does no page-metrics work. When the agent pinned a
  size with `browser_viewport set`, the address row shows
  `1280 × 720 · 62%` style labels so a scaled presentation is visible
  as such. The placement never changes a page's size.
- DevTools: Windows — `OpenDevToolsWindow` on the pane controller
  (full Chromium devtools). Linux — WebKitGTK inspector,
  spike-verified opening docked in-app. macOS — `isInspectable`,
  inspected from Safari (external; documented).

Focus and keyboard are native: the engine view receives real OS input
when focused. The SPA's global shortcuts would not fire while the browser
view has focus, so every engine gates modifier chords on the app's bound
set before its document sees them and hands a bound one back for the SPA
to dispatch (as built: docs/architecture/browser-tools.md § Keyboard).

## 8. Tool surface mapping

All 28 tools keep their contracts, schemas, and bounds. Per-driver
mechanism:

| Tool | CDP driver (Windows) | WebKit driver (mac/linux) |
|---|---|---|
| open / new_page / open_file / pages / select_page / label_page / session / close_page | as today | view lifecycle + Manager registry (no engine variance) |
| visibility / viewport | show/hide controllers; the thread viewport is always the device-metrics override, the pane only scales | show/hide views; viewport = the view's allocation (Linux) or bounds (macOS), parked or presented |
| snapshot / dom / locator | as today (CDP) | shared JS expressions |
| click / type / press / pointer / scroll | CDP Input (trusted) | JS-driven default (untrusted); XTest escalation for the visible pane; parity note |
| screenshot | CDP capture | engine snapshot API |
| wait / history / evaluate / evaluate_readonly | as today | JS + load-event delegates; readonly is best-effort; evaluate does not use page `eval`, so a page CSP without `'unsafe-eval'` allows it |
| clipboard (isolated per tab) | AO-managed, engine-agnostic | same |
| console_logs | CDP Runtime/Log | injected capture |
| downloads | WebView2: the launcher handles each page's `DownloadStarting` (no dialog), WebView2 saves to the Windows Downloads folder, and the launcher reports each download as numbered whole snapshots the backend applies in the launcher's order (`internal/webview2host/download.go`). Headless Chromium: Browser.download events → AO artifact dirs | WebKitDownload delegate → a staged file the backend names in its private staging folder in the user's Downloads folder; the backend moves each finished file beside it (`internal/browser/download_staging.go`, `internal/browser/user_downloads.go`) |
| assets | as today | JS inventory + fetch-in-page bundling |

New behavior the engines give for free (previously impossible): file
uploads via the real picker (user) and `DOM.setFileInputFiles` /
run-file-chooser (agent); JS dialogs presented for the visible page
(auto-dismissed only for hidden agent pages, as today); site context
menus; page clipboard writes reaching the OS clipboard under the
engine's own permission rules; basic-auth prompts; PDF viewing
(Windows/macOS built-in; Linux spike-verified: WebKitGTK ships a
built-in pdf.js viewer — its text lives in an internal
`webkit-pdfjs-viewer://` iframe, so snapshot/locator tools cannot read
PDF text on Linux, a documented parity note).

WSL file paths for uploads/open_file on Windows resolve through
`\\wsl.localhost\<distro>\...` when the engine (a Windows process)
must read them.

## 9. What is deleted

- Headless Chrome entirely: install/launch machinery, the
  Chrome-for-Testing artifact, lifecycle flags, `discard.go` blank-flip
  recovery (its incident stays recorded in AGENTS.md history).
- The whole screencast path: `companion.go` streaming (screencast,
  ack worker, burst window, keepalive, scale), frame long-poll RPCs
  (`BrowserCompanionNextFrame` / `Input` / `Subscribe` / `Resize`),
  the frontend frame loop and `<img>` surface.
- The pixel-era bridges the engine obsoletes: `CompanionReadSelection`
  / `CompanionInsertText` (native selection/paste now),
  `BrowserContextMenu.svelte` (site menus are real; AO items join the
  engine's menu via delegates).
- Encrypted site-data checkpoints (§4).

Kept, then reshaped (2026-09-01): `CopyPageFileToClipboard` became
`RevealPageFile` — the pane's file-page toolbar button now opens the OS
file manager with the file selected instead of writing a file object to
the clipboard. Live verification showed Teams refuses a pasted file
object no matter which clipboard formats carry it (bare `CF_HDROP`,
virtual-file `FileGroupDescriptorW`/`FileContents`, even Explorer's own
copy), while a drag from Explorer — including `\\wsl.localhost` paths —
attaches fine. The WSL staging pipeline went with the copy. The Globe
header button and `BrowserCompanionThreadState` hydration survive
(thread browser state still exists); the button is present whenever the
native browser is usable and opens a first tab when there is none. The MCP server, Access model,
bounds, and artifact quota code survive untouched.

Remote `--connect` clients get **no browser pane and no browser
tools**: the windowed engines live in the desktop app instance. The
remote-access campaign's port gateway (its spec § "Dev-server preview")
is the remote answer for dev servers; anything more is that campaign's
scope.

**Serve mode is the exception, and only for the TOOLS.** It gets no pane
— there is no window to put one in, and the streamed pane stays deleted
— but it does get the browser tools, over a headless Chromium engine
this backend launches (`docs/specs/remote-access.md` §7, "Headless
Chromium engine (serve mode)"). That Chromium is a SYSTEM install the
operator provided, never a downloaded one, so a serve host without one
falls back to exactly what this section originally described: no engine,
no tools, one line in the boot log. The deliberate regression stands for
every other windowless deployment.

## 10. Testing

- Unit: engine interface gets a fake driver; Manager policy tests run
  against it (today's fake-controller pattern continues).
- Harness/e2e: harness boots with the fake driver; the browser pane
  host rect renders without an engine. That pin is DEFAULT-ON in both
  mocked boot modes (`newIsolatedProviderApp` →
  `IsolationConfig.MockBrowserEngine`), which is what keeps
  `make go-test`, `make e2e`, and every unattended boot display-free
  and browser-free.
- **Real-engine gate (manual, opt-in).** `AO_HARNESS_REAL_BROWSER=1`
  lifts that pin, so the instance selects whatever real engine its
  deployment has and the harness becomes the real-browser rig:

  | Command | Engine it exercises |
  |---|---|
  | `AO_HARNESS_REAL_BROWSER=1 make harness-window` | the native engine — WebKitGTK on Linux (under WSLg), WKWebView on macOS (§6) |
  | `AO_HARNESS_REAL_BROWSER=1 make harness-wsl` | the launcher-hosted WebView2 engine, the real Windows leg (§5) |

  Everything else about the boot is unchanged: isolated data root,
  mocked providers, harness RPC surface. Site data stays isolated too —
  the native engines write under `<dataRoot>/browser-profiles/`, and the
  launcher's WebView2 user-data folder is already per-mode
  (`appidentity.BrowserProfilesDir("harness")` →
  `browser-profiles-harness`), so a harness run never touches the
  developer's own pane cookies.

  The variable is REFUSED whenever `--autopilot` is armed: that is what
  makes an isolated instance a soak rather than a harness, and a rig left
  streaming for hours with nobody watching must never grow a browser
  engine. The refusal is logged, not silent.

  Windowed boots work here because the two window facts are needed at
  different times: engine SELECTION only reads whether a window getter
  exists, and the window POINTER is resolved lazily when the first tool
  call starts the engine. So an isolated boot installs an empty getter
  before `App.Start` (`newIsolatedProviderApp`) and the windowed shell
  fills it in when Wails creates the window.
- Windows/Linux real-engine integration otherwise stays manual: there is
  no automated suite that spawns an engine, and windowless selection
  must keep answering `unavailableEngine`
  (`internal/browser/manager_test.go`).
- A placing drag never reaching a page: the browser suite
  (`paneDragBrowserView.browser.test.ts`) proves the pane reports its
  view not visible from a sidebar `dragstart` with no other drag event,
  and the Manager presents no page for such a rect (`paneCanShow`). That
  a real OS drag then reaches the SPA and not the hidden view is checked
  by hand on each engine through the real-engine gate: open a page whose
  `dragenter` and `drop` listeners change its title, scroll the strip so
  the browser pane touches the sidebar, and drag a sidebar thread
  straight from its row onto the page, hold, and release over it. The
  view hides as the drag starts, the title never changes, and the view
  returns when the drag ends.
- macOS: compiled, run and driven on the user's Mac 2026-09-03 through
  the real-engine gate (`AO_HARNESS_REAL_BROWSER=1 make harness-window`,
  tools called over the MCP endpoint off the mock provider's argv). The
  fix-up pass found four engine-side gaps, all fixed in shared or glue
  code: a right-click never raised `contextmenu`, an anchor `download`
  navigated instead of downloading, a statement list failed to parse
  and every script failure read "A JavaScript exception occurred"
  (see the engine and file-handling rules in `internal/browser/AGENTS.md`).
  Plus one engine-agnostic one: `browser_evaluate_readonly` wrapped an
  IIFE in a second call.
- Live verification checklist (user, per platform — run it in the real
  app, or on the isolated instance the gate above opens): audio/video,
  site context menu on a custom-menu test page, file upload, download,
  dialogs, clipboard both directions, show-in-folder reveal, devtools,
  HiDPI crispness, overlay clip behavior, workspace login isolation.
  macOS 2026-09-03, scripted on the isolated instance: audio and a
  canvas-stream video play; a right-click raises the page's custom
  menu; downloads land in the artifact dir by response type and by
  anchor attribute; hidden-page dialogs dismiss (alert void, confirm
  false, prompt null); `browser_clipboard` is the AO-managed tab
  clipboard while a page's `navigator.clipboard.writeText` reaches the
  OS pasteboard under WebKit's own rules (and `readText` is refused
  without a real gesture); devtools answer the explained refusal;
  dpr 2 with a crisp native pane; two workspaces see separate storage
  and cookies and a thread cannot address another thread's page; a
  bound chord typed into the page view opens the SPA's palette. Left
  for a person at the machine, because a native sheet cannot be
  scripted without Accessibility trust: the presented-page NSAlert
  sheets, the NSOpenPanel upload, show-in-folder, and eyeballing the
  overlay clip.

## 11. Delegation plan

Opus subagents (no sub-spawning), one wave per row, each gated by
line-by-line review and `make check` + `make test` green:

| Wave | Scope |
|---|---|
| W0 | Fork APIs (window handle / child-view hosting per platform) |
| W1 | Engine seam in `internal/browser`: extract driver interface, CDP driver keeps all existing behavior |
| W2 | Windows host: pane environment in launcher, directive channel, CDP tunnel, rect/visibility wiring |
| W3 | Linux host: GTK child views + WebKit driver |
| W4 | macOS host: NSView glue + shared WebKit-driver logic (to contract) |
| W5 | Frontend: host-rect pane surface, overlay clip, store slimming, chrome behaviors |
| W6 | Deletion sweep (§9) + docs/AGENTS.md truth sweep |
| W7 | Test waves: fakes, integration gates, harness, e2e |

No platform ships partial: the campaign is done when Windows and Linux
are verified live here and macOS compiles clean on the user's machine
with the same suites.
