# Embedded Browser

Status: implemented on Windows (WebView2), Linux (WebKitGTK) and macOS
(WKWebView). Serve mode drives a headless system Chromium for the tools
alone, never a pane (§9).

## 1. Decision

The browser pane is a real browser engine per platform, presented inside
the pane's own chrome (AO tab strip, address bar, toolbar). It is never a
streamed simulation of a page, and no streamed fallback exists, remote
included ([decisions](../decisions.md#remote-access-browser-pane-phone)).
Site data is AO-owned per cookie jar (§3, §4), and one MCP tool surface
drives real pages on every engine. Agent pages are parked pages of the
same engine, so an agent and the user share a session wherever they share
a jar.

## 2. Engine per platform

| Platform | Engine | Where it lives | Agent-tool driver |
|---|---|---|---|
| Windows (WSL build) | WebView2 (Chromium) | second WebView2 environment in the launcher process, controllers positioned in the launcher's Win32 window | CDP (chromedp), relayed (§5) |
| macOS | WKWebView (WebKit) | subviews of the Wails NSWindow's content view, in-process | native driver (§6) |
| Linux | WebKitGTK 6.0 (WebKit) | overlay children in the Wails GTK4 window, in-process | native driver (§6) |
| Serve mode (any platform) | headless Chromium (a system install, never downloaded) | one process per cookie jar, launched by this backend, no window and no pane | CDP (chromedp), in-process ([remote access](remote-access.md#dev-server-preview-across-machines-the-port-gateway), "Headless Chromium engine (serve mode)") |

The engine draws the page in the window's own compositor, so media, IME,
selects and the site's own menus behave as that engine ships them.
Chromium fidelity on macOS and Linux is out of scope; the tab menu's
"Open in system browser" covers engine-specific testing.

The in-process engines receive the window's native handle
(`ManagerOptions.NativeWindow`, from `WebviewWindow.NativeWindow`); the
Windows launcher hosts its controllers in its own window.

## 3. Ownership model

`internal/browser.Manager` is the policy layer: Access checks, the page
registry and its per-thread ownership, page and profile caps, bounds on
every tool result, artifact quotas, the AO-managed per-tab clipboard and
the MCP server. The engine behind it carries operations out against live
pages and nothing else (§6).

- One isolated engine profile per cookie jar (a named WebView2 profile, a
  WebKit network session or data store, a Chromium user-data directory).
  Each thread's browser chooses its jar: `worktree` (the default, shared
  by the threads on a canonical workspace, identified by the workspace
  path), `project` (shared by the project's threads that choose it, in any
  worktree), `thread` (this browser only, persistent) or `private` (always
  ephemeral, a new identity each time it is created, discarded when its
  last page closes). `jar.go` is the one mapping from (thread, choice) to
  a jar; engines receive only a jar key and an ephemeral flag. Pages stay
  thread-owned. WebKitGTK views are always constructed against an explicit
  AO-owned network session: the default session writes into
  `~/.local/share/webkitgtk/`.
- A jar carries site data only. Navigation and file permissions, the
  record of finished downloads, and headless Chromium's download
  directories and quotas stay per workspace, so two worktrees sharing a
  project jar keep their own authority.
- A page is parked until `browser_visibility` presents it inside the
  pane. A parked page keeps its viewport, layout and fresh snapshots out
  of sight: WebKitGTK keeps the view mapped in a host that measures 0x0
  and clips it to nothing, WKWebView keeps it in a 1x1 layer-masked park
  view beneath the SPA, and WebView2 keeps the controller visible inside a
  hidden clip container.
- Limits: 8 pages per thread, 24 per workspace across its jars, 24 per
  jar, 64 in total, and at most 12 live profiles. A closing page and the
  suspended record it left under the same id count once. Labels and
  parallel-agent page handles:
  [browser tools](../architecture/browser-tools.md#parallel-agents).

## 4. Site data

The engine persists each jar like a normal browser profile: cookies,
localStorage, IndexedDB, service workers, HTTP cache and everything else
it keeps.

| Engine | Where a persistent jar lives |
|---|---|
| WebKitGTK | `<data root>/browser-profiles/<jar digest>/` (`data/`, `cache/`), directories 0700 |
| headless Chromium | `<data root>/browser-profiles/<jar digest>/chromium` |
| WebView2 | a named `CoreWebView2Profile` per jar, inside the pane environment's one user-data folder per app mode, beside the SPA's under the launcher's data root (`appidentity.BrowserProfilesDir`) |
| WKWebView | a `WKWebsiteDataStore` per jar (`dataStoreForIdentifier:`, identifier derived from the jar in `wkwebview_identity.go`), stored where WebKit keeps it; before macOS 14 every store is non-persistent |

A worktree jar's digest is of the canonical workspace path.

- Site data at rest has the protection of the user's own browser profile:
  filesystem permissions, no AO encryption. Each boot deletes the
  encrypted site-data checkpoints an earlier version wrote
  (`pruneEncryptedCheckpoints`).
- `browserPersistSiteData=false` maps every jar to an ephemeral profile,
  and a `private` jar is always one. The contract every engine keeps: an
  ephemeral profile is never part of a persistent profile, and its data is
  discarded when the jar's last page closes. WebKit keeps it in memory (an
  ephemeral network session, a non-persistent data store); WebView2 uses an
  InPrivate profile, whose storage the runtime manages; headless Chromium
  has no in-memory profile and uses a temporary user-data directory
  ([browser tools](../architecture/browser-tools.md#platform-behavior)).
- **Clear site data** closes every engine page, deletes
  `<data root>/browser-profiles/`, and has each engine whose store AO does
  not own clear it: the launcher deletes and recreates the pane
  environment's user-data folder (`OpClearData`), and WKWebView removes
  every data store AO created. A failure is returned to the caller, never
  reported as cleared. Each thread's jar choice survives it.

## 5. Windows: hosting and the CDP relay

### Hosting

The launcher creates a second WebView2 environment ("pane environment")
with its own user-data folder, entirely separate from the SPA webview's
environment. Each tab is one controller, because a controller is a
positionable window and a bare CDP target is not. Each jar is a named
`CoreWebView2Profile` in that environment, so every pane page shares one
browser process and one debugging port.

Runtime constraints the host keeps, each documented where it is enforced:

- **Z-order**: WebView2 inserts each new controller's child window at the
  bottom of the host's child z-order, below the SPA, so every show raises
  it (`raiseChild`). WebView2 does not expose the child HWND; the host
  finds it by diffing its child list around creation (`newChildWindow`).
- **Environment overrides**: `WEBVIEW2_*` variables override the API
  arguments even when set but empty, collapsing every environment onto one
  shared profile with no error. The launcher clears them in its own
  process before creating any environment (`envscrub.go`).
- **Debugging port**: the pane environment's loopback DevTools port is the
  one the relay reaches. The host owns that switch and refuses a launcher
  switch that restates it (`envargs.go`). The SPA environment carries a
  port of its own only in diagnostic modes (`appidentity.DevToolsPort`),
  never in production.
- `ERROR_INVALID_STATE` at controller creation means "same user-data
  folder, different browser arguments", not a process-wide conflict.
- **Presentation**: the presented page's controller is positioned over
  the pane's fitted rect (the viewport scaled down to fit, centered by
  whole CSS pixels). CSS pixels convert to client pixels by the report's
  `devicePixelRatio`. The page's size is never the controller's:
  `Emulation.setDeviceMetricsOverride` pins the thread viewport and its
  `scale` draws it to fit, and screenshots divide the clip by the page's
  `devicePixelRatio` so they stay one image pixel per CSS pixel. A parked
  page keeps `IsVisible=true` inside its hidden clip container:
  `IsVisible=false` stops compositing, so rAF stops and a capture waits
  for a frame that never comes (`host_apply_windows.go`).
- **Popups**: every window a page opens, a middle, Ctrl or Shift click on
  a link included, becomes a launcher page. The launcher takes
  `NewWindowRequested`'s deferral, creates a hidden controller in the
  opener's profile and hands its view over (`put_NewWindow`), so no window
  of WebView2's own opens and `window.opener` works. It reports the page
  (`ReportPopup`) before any other report of it, and the backend adopts or
  closes it like any page (`popup.go`). WebView2 opens its default download
  dialog by itself over the opener when a popup's first navigation is a
  download; every page closes that dialog as it opens
  (`download_dialog_windows.go`).
- The host's COM bindings (`com_windows.go`) replace go-webview2's
  high-level wrapper, which cannot detect a failed HRESULT on 64-bit or
  pass environment options; only its loader is reused.

### CDP relay: no cross-boundary socket

The WSL backend never dials the Windows host, and nothing listens beyond
loopback.

- The launcher dials one additional WebSocket to the backend
  (`/browser-cdp`, the same direction and auth as the notification bridge:
  Windows to WSL loopback, launch token).
- That socket multiplexes TCP streams (`cdpframe.go`): the backend asks
  for a stream, the launcher opens `127.0.0.1:<cdp-port>` locally and
  pipes bytes. The launcher forwards only to the one port it was created
  with, so the tunnel is not a general proxy (`cdptunnel.go`).
- The backend exposes the tunnel as an ephemeral loopback listener inside
  WSL (`internal/cdprelay`), and chromedp attaches to it. The
  `webSocketDebuggerUrl` in `/json/version` is rewritten to that listener,
  so the only address the backend dials is its own.

The CDP page driver (locator, interaction, operations, console ring,
screenshots) is the one headless Chromium uses. Downloads are the
exception: the launcher owns WebView2's (§8).

### Pane directives

Backend to launcher control (create, show, hide, position, close
controllers, profile selection, devtools, accelerators, clear data) rides
the notification bridge as the `browser:host` channel, a closed set of
ops (`webview2host.Directive`). Launcher reports (controller created,
page closed, engine process died) ride the same connection's RPC path
(`BrowserHostReport`), as `updater:install` does. The launcher applies
one page's directives in the order they arrived.

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

No CDP exists for WKWebView or WebKitGTK. The in-process engines implement
the engine seam in `internal/browser/driver.go` (`browserEngine`,
`engineProfile`, `pageDriver`) with the engines' own APIs; the CDP engines
implement the same interfaces with chromedp. The two WebKit engines share
their tool logic through tag-free files (`webkitjs.go`, `pagejs.go`).

- JS evaluation: `webkit_web_view_call_async_javascript_function` /
  `callAsyncJavaScript`. Locator, snapshot and DOM tools are shared JS
  expressions (`pagejs.go`, `webkitjs.go`).
- Screenshots: `webkit_web_view_get_snapshot` / WKWebView
  `takeSnapshot`, which return fresh pixels for parked pages too.
- Input: JS-driven and untrusted (`isTrusted: false`) on every page,
  parked or presented: `element.click()` for a plain click, which runs the
  element's activation behavior including a file input's picker, and
  constructed mouse, wheel, keyboard and drag events otherwise
  (`webkitjs.go`). The agent tools have no trusted input on these engines,
  so a site that accepts only trusted input (some payment frames and
  anti-bot checks) ignores agent input there. User input is always real.
- Downloads, file chooser, dialogs and popups: WebKit delegate signals.
  On WebKitGTK `download-started` fires on the network session, not the
  view. A parked page's script dialogs are answered at once (`confirm`
  answers false) and its file picker is canceled, because nobody is there
  to answer them; a presented page gets the engine's own. WebKitGTK also
  suppresses a parked page's context menu.
- Console: an injected capture script (the user content manager,
  `WKUserScript` on macOS).
- `browser_evaluate_readonly` has no engine-level side-effect check on
  WebKit; its result carries a second entry saying so.

A parked view stays in the window, because WebKit guarantees layout and
snapshots only for a view in a window. Background-page tooling is built
on snapshots and JS evaluation, never on the page animating itself. On
WebKitGTK a view never changes widget parent between parked and
presented: a reparent unrealizes it and drops the frame WebKit last drew,
and a static page draws no other, so it would come back empty. No engine
hides a view with `opacity: 0`, which stops rAF.

## 7. The pane: chrome, rect sync, airspace

The pane's chrome is AO's tab strip, address bar, navigation buttons and
close (`BrowserPane.svelte`); its surface is an empty host rect the native
view is positioned over.

- Tabs render the Manager's state only: a tab being created, restoring,
  loading or failed shows that state, and the host rect says so while
  the active tab has no page to present
  ([browser tools](../architecture/browser-tools.md#ownership-and-lifecycle)).
  The strip and address row behave as a desktop browser's
  ([browser tools](../architecture/browser-tools.md#the-panes-chrome)).
- The SPA observes the host rect (`ResizeObserver` + scroll/layout +
  the pane-layout and sidebar stores: a divider drag on another pane
  slides this one sideways without resizing it, which no observer or
  event reports) and reports it over a binding, together with the
  visible clip intersection and the pane's resolved background color.
  A page's base color, painted wherever its document leaves the
  background transparent, is a property of the page, not of where it
  sits (`applyPageBase`): white under a document, as in any browser, and
  the presenting pane's color under a blank tab (the engine's white
  while the thread's pane never reported one). The Manager applies it,
  shown or hidden, when the page joins its tabs, when its committed
  address changes between a document and none, and when the pane's color
  changes, and a presentation pass brings it up to date before it shows
  the page, so no show overtakes a base still being applied; geometry
  carries no color. The Manager also fits the page's viewport into that
  rect (`placePage`: scale = min(rect/viewport, 1), centered by whole CSS
  pixels so a page that follows the pane starts at the rect's origin, clip
  intersected) and the platform host positions the native view at the
  fitted rect, drawn at that scale, and crops it to the clip, so a pane
  half behind the sidebar shows its visible half without the page
  relayouting. The rect also carries the SPA's `devicePixelRatio`, which
  the hosted engine folds into the CDP scale so webview zoom and DPI
  cancel out. Rect updates coalesce per frame, and each mount has at most
  one report in flight with only the newest waiting behind it, so reports
  land in order; one the backend already accepted is not resent, and a
  failed one is retried, bounded, then shown in the pane's banner. A host
  that positions in whole pixels snaps edges, never origin and size apart,
  with `floor(v + 0.5)`. On Linux each page lives in its own `AoViewHost`
  widget: an overlay child at the clip rect through four margins with
  `ALIGN_FILL`, allocating the view at exactly the viewport under a
  translate+scale `GskTransform` (`gtk_widget_set_size_request` cannot
  shrink a WebKitWebView, whose natural size sticks at the largest
  allocation it ever had, while an explicit `gtk_widget_allocate` takes any
  size, and GTK maps pointer input back through the transform). On macOS
  the view's frame is the fitted rect and its bounds stay the viewport.
  The Linux host wraps the Wails window's child in a `GtkOverlay` (ref the
  existing child, `set_child(NULL)`, wrap, re-set), and the SPA survives
  without a reload.
- **Airspace**: the native view always paints above the SPA, so an AO
  surface that would intersect the browser rect hides the view for the
  surface's lifetime. Every overlay primitive (popover, modal, palette,
  menu) registers its painted root in the airspace registry
  (`utils/paneAirspace.svelte.ts`), and the pane reports its view not
  visible while a registered surface intersects its clip. The toast stack
  and a promoted pane register as layers: the view hides while a toast
  covers it, and the pane's keyboard commands stay live, since a toast is
  not an overlay that takes the keyboard.
- Pane hidden / thread switched / layout drag in progress → view
  hidden. A drag that places a pane (a sidebar thread row or tool, a pane
  title) hides every view in the strip from its `dragstart` to its end,
  under the strip's drag shield (an airspace surface), because a view
  takes the drag from the page: nothing later than the `dragstart` is
  sure to reach the SPA. Nothing is torn down; page state lives on, the
  page keeps its viewport and keeps producing frames. By default the
  viewport is the pane's own size (`viewport.go`: the rect's whole pixels
  within the viewport bounds, applied latest-wins per thread), so a
  presented page is at 1:1 and reflows with the pane. A resize lays out
  only the presented page; another page of the thread takes the viewport
  before it is shown or a tool acts on it, and the thread's state is
  pushed once the resize settles. Moving the pane does no page-metrics
  work. When the agent pinned a size with `browser_viewport set`, the
  address row shows `1280 × 720 · 62%` style labels so a scaled
  presentation is visible as such. The placement never changes a page's
  size.
- DevTools: Windows opens `OpenDevToolsWindow` on the page's controller
  (full Chromium devtools). Linux opens the WebKitGTK inspector
  (`webkit_web_inspector_show`). macOS views are inspectable from Safari's
  Develop menu; WKWebView has no call that opens its inspector, so the
  pane's devtools action is refused there.

Focus and keyboard are native: the engine view receives real OS input
when focused. The SPA's global shortcuts would not fire while the browser
view has focus, so every engine gates modifier chords on the app's bound
set before its document sees them and hands a bound one back for the SPA
to dispatch ([browser tools](../architecture/browser-tools.md#keyboard)).

## 8. Tool surface mapping

Every tool keeps one contract, schema and bound on every engine
([browser tools](../architecture/browser-tools.md#tool-surface)). Per-driver
mechanism:

| Tool | CDP driver (Windows, serve mode) | WebKit driver (mac/linux) |
|---|---|---|
| open / new_page / open_file / pages / select_page / label_page / session / close_page | CDP targets + Manager registry | view lifecycle + Manager registry |
| visibility / viewport | show/hide controllers; the thread viewport is always the device-metrics override, the pane only scales | show/hide views; viewport = the view's allocation (Linux) or bounds (macOS), parked or presented |
| snapshot / dom / locator | shared JS over CDP | shared JS expressions |
| click / type / press / pointer / scroll | CDP Input (trusted) | JS-driven, untrusted (§6) |
| screenshot | CDP capture | engine snapshot API |
| wait / history / evaluate / evaluate_readonly | CDP | JS + load-event delegates; readonly is best-effort |
| clipboard (isolated per tab) | AO-managed, engine-agnostic | same |
| console_logs | CDP Runtime/Log | injected capture |
| downloads | WebView2: the launcher handles each page's `DownloadStarting` (no dialog), WebView2 saves to the Windows Downloads folder, and the launcher reports each download as numbered whole snapshots the backend applies in the launcher's order (`internal/webview2host/download.go`). Headless Chromium: Browser.download events → AO artifact dirs | WebKitDownload delegate → a staged file the backend names in its private staging folder in the user's Downloads folder; the backend moves each finished file beside it (`internal/browser/download_staging.go`, `internal/browser/user_downloads.go`) |
| assets | JS inventory + CDP resource bundling | JS inventory + fetch-in-page bundling |

`browser_evaluate` evaluates natively on every engine, not through the
page's `eval`, so a page whose script policy forbids `'unsafe-eval'` still
runs it (`evaluate.go`).

The engine owns what the pane shows a user: file uploads through the
real picker, JS dialogs and site context menus on the presented page, and
page clipboard writes reaching the OS clipboard under the engine's own
permission rules. `browser_clipboard` never touches the OS clipboard.

WSL file paths for `open_file` on Windows resolve through
`\\wsl.localhost\<distro>\...`, because the engine is a Windows process.

## 9. Windowless deployments

A deployment without a window has no engine and no browser tools, unless
it asks for headless Chromium:

- Remote `--connect` clients get no browser pane and no browser tools:
  the windowed engines live in the desktop app instance. The port gateway
  ([remote access](remote-access.md#dev-server-preview-across-machines-the-port-gateway))
  is the remote answer for dev servers.
- Serve mode gets the browser tools and no pane, over a headless Chromium
  this backend launches
  ([serve mode](../architecture/serve-mode.md#browser-tools)). That
  Chromium is a system install the operator provided; no engine is ever
  downloaded. A serve host without one has no engine and no tools, and the
  boot log says which setting names one.
- Every other windowless boot, `go test` included, selects
  `unavailableEngine`, and the App offers its threads no browser tools.

A file page's toolbar button reveals the file in the OS file manager
(`RevealPageFile`) rather than copying a file object
([decisions](../decisions.md#remote-access-browser-pane-phone)).

## 10. Testing

- Unit: Manager policy tests run against the fake engine
  (`fake_engine.go`); no unit test starts a browser.
- Harness/e2e: an isolated boot runs the fake engine, and the browser
  pane host rect renders without an engine. That pin is default-on in
  both isolated boot modes, `--harness` and `--soak`
  (`newIsolatedProviderApp` → `IsolationConfig.MockBrowserEngine`), which
  keeps `make go-test`, `make e2e` and every unattended boot display-free
  and browser-free.
- Engine smoke suites (manual, opt-in): `AO_NATIVE_BROWSER_SMOKE=1` runs
  the WebKitGTK and WKWebView suites (`internal/browser/native_*smoke*`,
  under a Weston of their own on Linux) and the WebView2 suites
  (`internal/webview2host/*_smoke_windows_test.go`, on a desktop of their
  own), and `AO_HEADLESS_CHROMIUM_SMOKE=1` runs the system-Chromium suites
  (`internal/browser/headless_*smoke*`). How to run each:
  [internal/browser](../../internal/browser/AGENTS.md) and
  [internal/webview2host](../../internal/webview2host/AGENTS.md).
  Windowless selection keeps answering `unavailableEngine`
  (`TestSelectEngineWithoutAWindowHasNoEngine`).
- **Real-engine harness gate (manual, opt-in).** `AO_HARNESS_REAL_BROWSER=1`
  lifts the fake-engine pin, so the instance selects whatever real engine
  its deployment has and the harness becomes the real-browser rig:

  | Command | Engine it exercises |
  |---|---|
  | `AO_HARNESS_REAL_BROWSER=1 make harness-window` | the native engine: WebKitGTK on Linux (under WSLg), WKWebView on macOS (§6) |
  | `AO_HARNESS_REAL_BROWSER=1 make harness-wsl` | the launcher-hosted WebView2 engine, the real Windows leg (§5) |

  Everything else about the boot is unchanged: isolated data root,
  mocked providers, harness RPC surface. Site data stays isolated too:
  the native engines write under `<dataRoot>/browser-profiles/`, and the
  launcher's WebView2 user-data folder is per mode
  (`appidentity.BrowserProfilesDir("harness")` →
  `browser-profiles-harness`), so a harness run never touches the
  developer's own pane cookies.

  The variable is refused whenever `--autopilot` is armed: that is what
  makes an isolated instance a soak rather than a harness, and a rig left
  streaming for hours with nobody watching must never grow a browser
  engine. The refusal is logged.

  Windowed boots work here because the two window facts are needed at
  different times: engine selection only reads whether a window getter
  exists, and the window pointer is resolved lazily when the first tool
  call starts the engine. So an isolated boot installs an empty getter
  before `App.Start` (`newIsolatedProviderApp`) and the windowed shell
  fills it in when Wails creates the window.
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
- Live verification checklist (per platform, in the real app or on the
  isolated instance the gate above opens): audio/video, site context menu
  on a custom-menu test page, file upload, download, dialogs, clipboard
  both directions, show-in-folder reveal, devtools, HiDPI crispness,
  overlay clip behavior, cookie-jar isolation. Native sheets (macOS
  `NSAlert` dialogs, the `NSOpenPanel` upload) cannot be driven without
  Accessibility trust and are checked by a person at the machine.
