# internal/browser

Built-in browser MCP and companion-pane manager. `driver.go` isolates engine
implementations: launcher-hosted WebView2 on Windows/WSL, WebKitGTK on native
Linux, WKWebView on macOS, and explicitly enabled headless Chromium in serve
mode. Product behavior and authority are defined in
[browser-tools.md](../../docs/architecture/browser-tools.md).

## Ownership and lifecycle

- Register the thread before spawning its provider. First registration starts
  the loopback MCP listener because the provider receives its per-thread
  capability URL in argv; starting it on first tool use is too late.
  Session end revokes that capability; the thread's pages outlive it and
  close when the thread is deleted or archived. Only the reaper's idle end
  suspends them (`suspend.go`), and shutdown saves them for the next boot
  (`page_records.go`); see browser-tools.md for the lifecycle. Every path that
  acts on a page by id goes through `resolvePage` so a suspended page
  restores under the navigation policy. Showing the companion (`Visibility`)
  only checks the tab exists; the pane's selection restores it.
- One manager owns one engine. Profiles are per cookie jar, pages are per
  thread, and every operation rechecks thread ownership. Closing the last page
  disposes its profile; manager shutdown joins all engine work.
- `jar.go` alone maps a thread's jar choice to a jar identity; engines see
  only the jar key and an ephemeral flag, and the worktree jar keeps the
  canonical workspace as its key so existing profile ids do not change. Jars
  carry site data only: navigation and file policy, download directories and
  quotas stay per workspace (`workspaceAuthority`); a jar profile's own
  policy answers with the authority of the page owning the request's frame
  or of its opener chain, and with none when unresolved (`jarAllows`). A
  switch (`jar_switch.go`) advances the thread's jar generation and suspends
  its live pages into durable records in one lock hold; the tabs return
  only through the restore path, and any create or restore admitted under
  an older generation must retry, never land in the old jar.
- A tab's lifecycle is Manager state (`lifecycle.go`): a page being created is
  a pending tab, a closed page leaves the tab set before its engine teardown
  runs, and both keep counting toward caps and holding the jar's
  profile until the engine is done. A teardown that runs after its tab left
  reports through `reportTeardown`. Loading and favicon state is
  `pagestate.go`; a page whose process ended becomes a failed suspended
  record (`crash.go`); the agent marker is `activity.go`; the closed-tab
  stack and the tab menu's actions are `companion_tabs.go`. Every user
  selection of a tab is ordered per pane mount by `selection.go` inside
  the lock hold that sets the active tab.
- Pages start hidden. Present only the selected page for a visible companion
  pane. A presentation pass decides its show in one `Manager.mu` hold after
  its hides and any layout wait (`decidePresentation`), against the
  selection and live pages as they are then, so a page that lost the
  selection or its process meanwhile is never shown; the focus intent's
  presented check also requires the selection. The pane uses a native view
  positioned over the SPA; pixels do not cross the transport. Keyboard
  focus enters a page's view only by a click or a focus intent
  (`focus_intent.go`): the pane's request for one page, applied once while
  its mount presents that page and withdrawn by the user's next focus
  choice. Every engine checks what it shows and whether the intent is
  live on its own UI thread, immediately before moving focus.
- A page always lays out at the thread viewport (`sessionViewport`) whether
  hidden or presented. The viewport follows the mounted pane's size (whole
  pixels within the viewport bounds; the last size once the pane is gone;
  1280x720 before any pane) unless `browser_viewport set` pinned one. Pane
  sizes are applied latest-wins per thread in `viewport.go`, never inline in
  the rect report, and only to the active page; any other page takes the
  viewport under the operation's own lock (`lockPageForOperation`) and is
  never newly shown while it lags (`presentPanes`); whichever apply lays a
  held page out runs the pass that shows it (`presentLaidOut`), so a show
  waits for the layout and never for page work. The pane is a viewer:
  `placePage` (`internal/paneplace`, shared with the Windows launcher) fits
  the viewport
  into the host rect and every engine draws the page at that
  `PanePlacement` scale without resizing it. A host that positions in whole
  pixels snaps edges, never origin and size apart (`paneplace.Layout`). A
  position-only change does no device-metrics work. Hidden pages must keep
  producing frames so screenshots and scrolls work with the pane closed.
- An omitted `page_id` may resolve only when the thread owns at most one page.
  With multiple pages, require an explicit handle. Never infer a caller or use
  MRU selection.
- Keep the existing resource bounds on profiles, pages, viewport size,
  operations, snapshots, screenshots, downloads, assets, console data, and MCP
  bodies. Do not start an engine per tool call.
- Engine callbacks must not synchronously perform teardown that waits on the
  callback's own event loop or UI thread.

## Engine rules

The CDP engines use `chromedp` through the shared driver seam. The hosted
engine reaches the Windows controller only through `internal/cdprelay`; the
headless engine starts one Chromium process per profile with an isolated user
data directory. Headless mode is selected explicitly by serve boot and must not
become a fallback for an ordinary windowless process.

chromedp delivers each event method on its own subscription, so handlers run in
no fixed order across methods. State fed by several methods must settle the
same way under any interleaving and bound what it keeps for unmatched events
(`cdp_events.go`).

All GTK/WebKit calls go through `gtkDo`; all AppKit/WKWebView calls go through
`wkDo`. Do not call either while holding a lock reachable from a native
callback. Native callbacks carry integer IDs, never Go pointers. Keep shared
page-operation JavaScript in `webkitjs.go` and `pagejs.go`; selectors and user
text cross as JSON values rather than source fragments.

Hidden native WebKit pages must remain attached to their window in a clipped
1x1 host at their viewport so layout, animation, and snapshots remain live.
Presentation scales the view (a GTK allocation transform, an AppKit bounds
size) inside a per-page clip. Do not use opacity, natural-size requests, or a
resize of the page as substitutes.

Platform differences remain explicit. WebKit input is programmatic and reports
`isTrusted=false`; CDP native input must assert resulting DOM state. WKWebView
site data uses identified WebKit stores where available and has no AO-owned
profile directory. Unsupported platform capabilities return a clear refusal.

## Files, downloads, and site data

`browser_open_file` resolves symlinks and accepts regular files within the
thread workspace or project root, or the workspace's own download directory,
unless the explicit outside-workspace setting widens access. Use direct
`file://` navigation.

Downloads use sanitized unique names under AO-owned artifact directories and
never the user's Downloads directory. Preserve quota reservation, per-download
bounds, page cancellation, and denial of sensitive website permissions. Every
change to a page's download entry goes through `noteDownloadLocked` and
`publishDownloads` (`downloads_thread.go`), which keep the thread's list the
pane shows and coalesce byte progress; an engine reports one terminal event
per download, and that report alone settles the user's cancel. Every
removal of a live page goes through `pageLeftLocked`, which settles the
page's running downloads and, under the page's `downloadMu`, ends its
admission of new ones. The hosted engine sets each page's download
behavior from the page's own session before handing the page over, has the
profile's other live pages set it again, one profile-serialized walk per
leaving page, before that page's session goes, and on a refusal leaves the
page open and denies the profile's downloads (`hosted_downloads.go`). A
profile's download-policy change, whenever it happens, reaches the user
only through `engineEvents.DownloadPolicyChanged`.

Site data belongs to the engine and is isolated per cookie jar. Persistent and
ephemeral profiles must remain distinct; a private jar is always ephemeral.
Clearing site data first closes pages, then removes AO-owned profile trees and
asks engines with externally managed stores to clear themselves. A
platform-specific silent no-op is incorrect.

## Tests and references

Unit tests use `fake_engine.go` and fake CDP endpoints. They must not start or
download a browser. Keep `ManagerOptions.FakeEngine` as the default for test
boots. Real Chromium launch and CDP page operations are covered only by the
documented `AO_HEADLESS_CHROMIUM_SMOKE=1` manual gate. Real WebKitGTK and WKWebView
behavior is covered only by the `AO_NATIVE_BROWSER_SMOKE=1` manual gate
(`native_engine_smoke_test.go`), which opens a window. On Linux it starts its
own headless weston and is refused without one, unless
`AO_NATIVE_BROWSER_SMOKE_SESSION_DISPLAY=1` allows the session display.

- [embedded-browser.md](../../docs/specs/embedded-browser.md)
- [codex-browser-parity.md](../../docs/references/codex-browser-parity.md)
- [in-app-browser-spike.md](../../docs/architecture/in-app-browser-spike.md)
- [internal/cdprelay](../cdprelay/AGENTS.md)
- [internal/webview2host](../webview2host/AGENTS.md)
