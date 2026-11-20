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
  an older generation must retry, never land in the old jar. From its
  capture to its commit a switch holds its lifetime's structural gate:
  every change to that lifetime's tabs takes `Manager.mu` through
  `lockStructure`, which waits for the commit, so the commit suspends the
  tabs it captured and wrote. `SetJar` takes the caller's `JarConsent`: an
  unconfirmed switch is refused while the thread has a live tab, in the
  hold that captures it, and a switch refused after its write (the
  Manager's close) saves the thread back as it is.
- A thread's browser lives as long as the thread (`thread_life.go`): one
  `threadBrowser` per lifetime owns its session, browsing history, pane
  color and download list, and every tab names it as its owner. An action
  binds before its first wait (`Bind`, `BindThread`) and reaches only the
  lifetime it bound; `CloseThread` detaches the lifetime in its first hold,
  so a commit through it answers `ErrThreadClosed`, and closes only what it
  detached. Look a lifetime up by thread id only for what shows the thread
  as it is now (state, presentation, the saved copy).
- A tab's lifecycle is Manager state (`lifecycle.go`): a page being created is
  a pending tab, a popup being adopted reserves its page from admission to
  listing (`adoptPopup`), a closed page leaves the tab set before its engine
  teardown runs, and each keeps counting toward caps and holding the jar's
  profile until the engine is done. A native engine lists a page in its
  profile, where `Dispose` finds it, only once its view is in place. A teardown that runs after its tab left
  reports through `reportTeardown`. Loading and favicon state is
  `pagestate.go`; a page whose process ended becomes a failed suspended
  record (`crash.go`); the agent marker is `activity.go`; the closed-tab
  stack and the tab menu's actions are `companion_tabs.go`. A pane's tab
  selections, and the keyboard's commands on the active tab
  (`active_commands.go`), are admitted per mount in the user's order
  (`selection.go` over `turnorder`, which owns the numbering): each takes
  its turn before the lock hold that sets the active tab, adds the tab or
  resolves the active one, and that hold ends it with `Turn.Commit`, whose
  standing says whether to act. Page work never delays the next turn, and
  the one function that selects answers the tab its caller may restore.
- Pages start hidden. Present only the selected page for a visible companion
  pane. A presentation pass decides its show in one `Manager.mu` hold after
  its hides and any layout wait (`decidePresentation`), against the
  selection and live pages as they are then, so a page that lost the
  selection or its process meanwhile is never shown; the focus intent's
  presented check also requires the selection. A page's base color is the
  page's own, applied shown or hidden by `applyPageBase` whenever its
  address, its pane's color or its listing changes, and by the pass
  before it shows a page, so a show never overtakes one under way; a
  presentation carries geometry only. The pane uses a native view
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

Every engine reports a page it opened by itself as `PopupOpened`, and the
Manager alone adopts or discards it (`adoptPopup`). A popup whose opener is
still being adopted, or not reported yet, waits for it (`awaitOpener`), so
an engine reports a popup under the handle its adoption keeps; a WebKit
popup view gets its policy and create handlers when it is made
(`pageHandle`). A
download a popup starts before its tab is listed names the popup by the
handle the Manager adopts it under (`downloadStart.Popup`), and the Manager
parks it, newest progress only, until the popup's fate lists it in the
popup's tab or its nearest listed opener's (`popup_parked.go`); the tab
and its parked downloads are listed in one `parkMu` hold that a page's
close also takes. An engine keeps no download of its own for a popup.
Headless Chromium runs with its popup blocker off, so a window a page opens
with no gesture is a popup there as on WebView2. On the hosted engine a
popup is the launcher's report (`hosted_popups.go`), attached only on its
opener's connection, within the attach timeout, and canceled by its close.
Every engine reports a popup before anything else of it, and any page's
close and crash as it observes them, holding none back: the Manager
records each in the popup's admission (`popupadmit`) before it acts, and
the listing's one commit decides the tab (a close refuses it, a crash
alone lists it as a crashed tab from its first push, also when its attach
then fails); a listed page's close and crash run in report order
(`engine_pages.go`). An admission is kept while its popup is pending or
listed or a popup of it is, so a download finds the nearest open
ancestor however many popups come and go; only ended ones go to a bounded
history, past which a popup's ancestry is unknown, never empty. A WebKit popup view's close and crash handlers are
installed at its creation, and until its adoption it names itself by its
popup number. A hosted page is one record with one state
(`hosted_pages.go`): it admits downloads from its bind or popup report
until its close or discard is received, and its lane retires it after
the reports received before that, ending the downloads still running
(`retirePage`). An engine that processes a page's end after receiving it
records the end at receipt (`PageEndReceived`), and the Manager tells a
listed popup's driver it is listed (`listingDriver`). The CDP engines answer the
navigation policy only browser-wide (`profileOptions.Allow`), one pause per
request, so a popup's requests before its adoption have its opener chain's
authority, the launcher's popup report counting as an opener edge on the
hosted engine. A request whose owner does not resolve yet is answered at
once with what a page with no workspace's authority may open (`jarAllows`);
only one that needs an owner's authority waits up to `requestOwnerWait`
for it, then is refused (`awaitRequestOwner`). A CDP page reads its frame
events, the attach and detach of its cross-site iframes' targets, its
main frame's loading and its frame tree read at attach from one input in
the browser's order (`cdporder`, which both CDP engines connect through),
so neither needs a time window or event counts, a cross-site iframe stays
the page's across its move to a renderer of its own (`frameTracker`), and
it installs no interception of its own. chromedp gives no ordered join of
methods; do not add per-method subscriptions for state two methods share.
Every CDP page attach, a created page's or a popup's, is bounded by the
engine's attach timeout, ends with its caller's cancel, and ends at the
engine's `Interrupt` (`startCDPPage`).
Each CDP connection keeps one bounded record of its targets
(`cdp_targets.go`), which goes with the connection.

The CDP title watcher (`cdp_page.go`) starts at evaluation, depends on no
page event, follows the title element `document.title` reads, and observes
only the title path; never observe a document's whole subtree from it.

A headless browser's target and download events come from one input in
the browser's order too (`cdporder.WatchBrowser`, `headlessProfile.subscribe`),
so a popup's creation is handled before its crash, its destroy and its
downloads. Outside `cdporder`, chromedp delivers each event method on its
own subscription, so handlers run in no fixed order across methods. State
fed there by several methods, such as the hosted connection's targets,
must settle the same way under any interleaving and bound what it keeps
for unmatched events (`cdp_targets.go`).

All GTK/WebKit and AppKit/WKWebView calls go through `uiThread`
(`native_uithread.go`, a `uicall.Thread`): `Run` for an operation, which
never runs it after it answered withdrawn, and `Release` for giving up
what is held, which is never withdrawn. Nothing else in this package posts
to the UI thread; the forbidigo rule in `.golangci.yml` refuses any other
call of Wails' `Invoke*`. Do not call `uiThread` while holding a lock
reachable from a native callback. A closure posted to the UI thread
carries ids and looks the profile or download up there
(`withProfileOnUI`, `withDownloadOnUI`). A download's destination decision is held
by its profile from WebKit's hand-over until its completion or the
profile's close takes it, once (`internal/browser/destination`). Native
callbacks carry integer IDs, never Go pointers. Keep shared page-operation
JavaScript in `webkitjs.go` and `pagejs.go`; selectors and user
text cross as JSON values rather than source fragments.

Hidden native WebKit pages must remain attached to their window in a clipped
empty host at their viewport so layout, animation, and snapshots remain live.
A WebKitGTK page's host keeps one widget parent from its first park to its
close: a reparent drops the frame WebKit last drew, and a static page shown
again stays empty.
Presentation scales the view (a GTK allocation transform, an AppKit bounds
size) inside a per-page clip. Do not use opacity, natural-size requests, or a
resize of the page as substitutes.

Platform differences remain explicit. WebKit input is programmatic and reports
`isTrusted=false`; CDP native input must assert resulting DOM state. WKWebView
site data uses identified WebKit stores where available and has no AO-owned
profile directory. Unsupported platform capabilities return a clear refusal.

## Files, downloads, and site data

`browser_open_file` resolves symlinks and accepts regular files within the
thread workspace or project root, or the workspace's own finished downloads
(`authorizeFile`), unless the explicit outside-workspace setting widens
access. The download rows the Manager still lists are the one record of
which files in the Downloads folder are openable (`listedDownloadFile`), and a file opened on a row's grant is
opened only while it is still that row's file, checked in the page's
operation after any tab creation (`openFile`);
keep no second one. Use direct `file://` navigation.

Where a download goes is the engine's `DownloadSaving`; the rule is
browser-tools.md's Browser hardening. An engine never writes into the
user's Downloads folder by a path AO chose: an engine in this process
writes only inside the app's private staging directory there, into the
file the Manager names as the download starts (`download_staging.go`),
and the Manager moves the finished file beside it without copying
(`publishFinishedDownload`, `publishDownload` in `user_downloads.go`),
never replacing a file; WebView2 saves there itself and only a regular
file of the reported size is openable (`engineSavedDownload`, whose
comment states the limit). Close waits for a publish under way.
Headless Chromium alone keeps downloads as artifacts with quota
reservation, the per-download cap and pruning; keep those bounds on that
path only. A page's download log (`downloads_log.go`) and its thread's
list (`threadDownloadList`) each enforce their own bound as an entry
settles; nothing outside them trims. Every change to a page's download
entry is made in a hold of its log and goes through
`noteDownloadStartLocked` or `noteDownloadLocked` and `publishDownloads`
(`downloads_thread.go`), which keep the thread's list the pane shows and
coalesce byte progress; an engine reports one terminal event per
download, after its start, and that report alone settles the user's
cancel. The engine that saw the end names its cause (`downloadState`:
canceled, failed, untracked, page gone); the Manager lists it from that,
never from whether a cancel was pending. Every removal of a live page
goes through `pageLeftLocked`, which settles the page's running
downloads, except one whose file is being published, which its publish
settles, and, in the same hold of its log, ends its admission of new
ones. The hosted engine's
downloads are the launcher's numbered snapshots (`hosted_downloads.go`),
each a download of one page, keyed by page and id, so a report naming
another page's download is refused; its page's lane applies them in
order, each only when newer than the last applied; an answer never
creates a row, and a download is asked about only once admitted; a cancel
made while its admission is in flight is kept and sent as it is admitted.
Cancels are numbered, and the refusal of the last one sent withdraws the
Manager's cancel from whichever snapshot first carries it. A launcher
report is acknowledged at receipt, never after its processing
(`hosted_pages.go`, `hosted_report_lanes.go`): receipt changes only the
page's record, and a snapshot, close, discard or crash runs afterwards on
its page's lane, in order, with an unlisted popup's on its opener's, so no
path conversion holds another page's report and a discard settles after
the work received before it. `DiscardPage` waits for that within its
context, then answers that the discard is pending while the lane still
finishes it. A hosted page ends at the launcher's close, which follows
its downloads' ends; its CDP target's destroy ends it only past
`hostClosedFallback`. One status loop asks for a quiet download's snapshot,
or one whose last cancel no snapshot names yet, and names that cancel, so a
lost report or cancel never leaves a row running; headless Chromium's arrive over
CDP (`headless_downloads.go`), which keeps each download from its start to
its end, and the file of one the Manager did not take until its end
arrives or the browser has stopped, then removes it. Preserve the denial of sensitive website
permissions.

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
own headless weston. A window on the session display, the only one macOS
has, also needs `AO_NATIVE_BROWSER_SMOKE_SESSION_DISPLAY=1`; the run is
refused without it.

- [embedded-browser.md](../../docs/specs/embedded-browser.md)
- [codex-browser-parity.md](../../docs/references/codex-browser-parity.md)
- [in-app-browser-spike.md](../../docs/architecture/in-app-browser-spike.md)
- [internal/cdprelay](../cdprelay/AGENTS.md)
- [internal/webview2host](../webview2host/AGENTS.md)
