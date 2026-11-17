# Built-in Browser Tools

## Product contract

Agent Overflow supplies one provider-neutral HTTP MCP server to every
headless Claude and Codex session on a deployment that HAS a browser engine.
The server is registered even when browser tools are disabled; the disabled
server advertises an empty tool list and never starts the engine. This keeps
the off-state cheap while allowing a live provider to rediscover the tools
without restarting its process.

A deployment with no engine — a remote `--connect` backend, `go test`, a serve
host with no Chromium installed on it — registers no server at all, so a
session there is offered no browser tools rather than tools that could only
refuse. Serve mode with a Chromium installed HAS an engine, headless and
windowless; see "Serve mode drives a headless Chromium" below.

Three settings are independent:

- `browserEnabled` (default `true`) controls tool exposure and immediately
  closes browser state when turned off.
- `browserPersistSiteData` (default `true`) decides whether each cookie
  jar's site data lives in a real browser profile on disk or only in
  memory. The site data is the ENGINE's, always: AO reads and writes none of
  it, so the engine's own store is the single source of truth rather than a
  copy of it.
- `browserAllowOutsideWorkspace` (default `false`) widens direct-file opening
  beyond the current workspace/project roots.

A fourth, `browserChromiumPath` (host tier, default empty), is deployment
wiring rather than behavior: it names the Chromium a serve host launches when
the one it wants is not on `PATH`. It must be an absolute path, it is ignored
by every deployment whose engine is the platform's own, and it never causes a
download.

Browser pages start hidden — parked out of the window's layout rather than
opened anywhere the user can see. An agent explicitly presents a page with
`browser_visibility`; AO then opens an ephemeral browser companion beside that
thread and the engine positions that exact page's real view over the pane's
host rect. The user and agent share its URL, DOM, cookies, history, focus, and
tab set; no separate browser window or duplicate browsing session is opened.

The Browser settings panel also exposes a destructive **Clear site data**
action. It closes every page first, so a later teardown cannot write the
cleared state back, then clears BOTH places site data can live: the AO-owned
profile directory, and — on a deployment whose engine keeps its data
somewhere AO cannot reach by path — the engine's own store. On Windows/WSL
that store is the launcher's WebView2 user-data folder on the far side of
the WSL boundary, so the backend asks the launcher to release its browser
environment and delete the folder, and waits for the answer; on macOS it is
WebKit's own data-store directory. The button clears real state on every
platform, and a failure is reported rather than swallowed — never a silent
per-platform no-op.

## Ownership and lifecycle

- One lazily started engine per AO process. AO downloads no engine: the engine
  is the platform's own, hosted by the app or by its launcher.
- One isolated profile per cookie jar with at least one active browser page.
  Each thread's browser chooses its jar from the address row's cookie jar
  menu: `worktree` (default; the threads on a canonical workspace share login
  state, in the profile they used before jars), `project` (the project's
  threads that choose it share one, in any worktree; keyed on the project id
  the app passes in `Access`), `thread` (this browser only, kept between
  sessions) or `private` (always ephemeral: never part of a persistent
  profile, discarded when its last page closes, a new identity each time).
  `internal/browser/jar.go` is the only mapping from a thread's choice to a
  jar; engines get a jar key and an ephemeral flag.
  `browser_session` reports the jar. Agents have no tool to change it, since
  a switch closes the user's tabs. Pages remain owned by the registering
  thread.
- A jar shares site data and nothing else. Navigation and file permissions,
  the record of finished downloads, and headless Chromium's download
  directory and quota stay per canonical workspace, whichever jar a page
  is in. A request the jar's own policy
  sees (headless Chromium's browser-wide interception) has the authority of
  the page its frame belongs to, or for a popup not yet adopted, of its
  opener chain; one whose owner is not resolved may open no workspace's
  files.
- Switching the jar with tabs open asks first in the pane. The Manager then
  suspends every live tab into the new jar: its page closes and a suspended
  record keeps its id, position, label and address, the active tab staying
  active, and the records are saved before any teardown starts, so a
  shutdown or crash mid-switch loses no tab. The tabs come back through the
  ordinary restore: the pane brings back the tab it shows, and the others
  return when selected or used, with the navigation policy applied and no
  page history. A tab that cannot come back stays suspended with the error.
  No restore of the thread is admitted until every page it had closing at
  the switch is gone, old profiles' disposal included, and until then a
  closing page and its record hold one slot in the thread's cap; closing
  the record leaves the slot to the page. Any restore is admitted only while
  its record exists, and forgetting a record (closing the tab, clearing site
  data, turning the browser off) cancels its restore. A per-thread jar generation fences a create or restore that
  began before the switch, so it lands in the new jar; a popup is admitted
  only beside a live opener, which a switch has already closed.
- A thread gets an opaque MCP URL capability at provider-session start, and
  session end revokes it. Its registration does not create a profile or page.
- The first page operation creates the jar's profile and a page. Page
  operations serialize per page; unrelated pages run concurrently.
- The companion presents a page's real view only while a pane is mounted with a
  paintable host rect AND the thread's session is visible. AO presents only the
  thread's explicitly selected tab. No pixels cross the wire in either
  direction, so a hidden pane costs nothing and no connection can receive
  browser image data.
- Every page lays out at the thread's viewport whether or not a pane shows it.
  By default the viewport follows the mounted pane's host rect (whole CSS
  pixels, held within 320×240 and 1920×1200): the page fills the pane at 1:1
  and reflows when the pane resizes, like a tab in any browser. With no pane
  the last pane size stands; a thread that never mounted one lays out at
  1280×720. `browser_viewport set` pins a size instead; the pane then draws
  the page scaled down to fit (never up, centered with margins when the rect
  is larger) and labels the size and scale in its address row. `reset`
  returns to following the pane, and `get` reports the current size, whether
  it is pinned, and the pane size. Screenshots, coordinates, and layout always
  mean the current viewport: a pane resize lays out the shown page at once,
  and every other page of the thread before it is shown or a tool acts on
  it. Hidden pages keep rendering, so screenshots and
  scrolls work with the pane closed, and a screenshot that produces no frame
  fails with a bounded error instead of hanging.
- `pane.close` (Mod+W) on a focused browser companion closes the active tab;
  the companion closes when its last tab does. Closing the companion any other
  way hides the session and keeps its pages, so reopening shows the same tabs.
- A tab the user closes is kept on the thread's in-memory closed-tab stack
  (`internal/browser/companion_tabs.go`): its address, label and position,
  the last 10, for http, https and `file:` addresses of a tab that finished
  opening; an agent's close is not kept. The close that takes the tab out of
  the tab set records it, so racing closes record it once. Reopen pops the
  newest into a new tab at that position, without its label if another tab
  took it meanwhile, since labels stay unique; a reopen that fails before the
  tab exists puts the entry back unless the thread's own history was
  forgotten meanwhile (another thread's forget does not count). The state
  carries `closedTabs`, the stack's depth. The stack is browsing history,
  forgotten with the recent addresses below by the same helpers, and a
  private jar keeps none.
- A tab's state comes from the Manager alone (`internal/browser/lifecycle.go`).
  A new tab is in the state, `creating`, before the engine page exists; the
  user's own new tab is also selected and shown at once, and a failed creation
  stays as a tab with its `error` until closed, while an agent's leaves no tab.
  A close removes the tab, cancels the page's operations and pushes the state
  before the engine closes the page and, with the jar's last page, its
  profile. Until that teardown ends the page counts toward the thread's limit
  and its jar opens no second profile. A teardown failure is a `teardown`
  event: the pane's banner when the pane is open, otherwise a toast that stays.
  Selecting a tab presents it before any page work; a suspended tab shows
  `restoring` until its reload ends. The pane's own request to restore the
  suspended tab it shows (`restore`) selects nothing and acts only while that
  tab is still the active one.
- A pane's tab selections take effect in the order the user made them
  (`selection.go`), though the transport runs each call on its own
  goroutine. Every pane action that selects a tab (`activate`, `step`, and
  the `new`, `reopen`, `duplicate` and `download-open` that select the tab
  they open) carries the pane's mount and a number the client takes when
  the user acts, consecutive from 1 in each mount; a re-attach is a new
  mount and starts again at 1. The Manager admits a mount's numbers in
  order: the next one selects at once, a later one waits for its
  predecessors' selection (never their restore or load), and each then
  does what it says and restores the tab it selected. When a second passes
  with no progress the count moves past the numbers not yet arrived: they
  lose their place, not their effect, and one that arrives later applies
  then, in arrival order. A mount remembers the last 256 numbers it moved
  past; an older one arriving is refused with an error. A repeat of a
  number already admitted, one from a mount the Manager does not have
  (detached, or another thread's), a waiter whose mount detaches, and a
  selection admitted before its mount detached answer success and select,
  open and restore nothing; a reopen keeps its closed tab. More than 256
  waiting in one mount are refused with an error, and that number, sent
  again, is treated as never sent. A selection with no number (an older
  client) or no mount, an
  agent's `browser_select_page` and the chat header's show apply in
  arrival order and never wait. Closing the active tab reselects only when
  that tab is gone.
- Each page reports `loading` and a `favicon`. Every engine reports loading
  (CDP frame start and stop events, WebKitGTK `is-loading`, WKWebView
  `loading`). When a load stops, an engine with a pane reads the favicon in
  the page with shared JavaScript, so the fetch carries the page's session:
  an image data URL within 32 KB, empty when there is none or it is refused.
  Headless Chromium has no pane and reports none.
- The thread's state is pushed whole, every tab with its favicon, and only
  by a change. A write that changes what a tab shows (its info, loading or
  favicon) marks the page until a push to every client carries it; a push
  takes the marks before it reads the tabs, and one client's read of the
  state leaves them. Every writer then publishes, which pushes when any of
  the thread's pages is marked, so a tool call or an engine report that
  leaves the state as it was pushes nothing and no write waits for a later
  change. `browser_pages`' `selected` and `lastOpened` are per call and
  mark nothing.
- A page's cached info has two kinds of writer, both through one commit.
  An engine report commits what the page now shows; each engine numbers its
  reports where it orders them (a native view on its UI thread, a CDP engine
  in its event stream), as it does its loading reports, and the Manager
  drops one older than a report it committed. The fence sees every report
  from the moment the engine page exists: a native view reports through its
  page's own hook, and reports its address and title as its popup is
  adopted; a CDP engine reports by handle, and the Manager holds the newest
  report for a handle no page answers to yet (a page created or restored
  before its attach, a popup during its adoption) until its page takes it.
  A report for a page that left its tabs changes nothing shown. A CDP
  engine's report carries the browser's placeholder title (the address), and
  Chromium sends no report when the document sets its title; the document
  reports its address and title itself, from an isolated world through a
  `Runtime.addBinding` binding (`cdp_page.go`), and that title is the tab's
  while the tab shows that address, whichever of the two arrives first.
  The watcher starts when it is evaluated, so no page event handling can
  stop it. It follows the title element `document.title` reads (the first
  HTML title in tree order, or an SVG root's own title) and observes only
  the nodes a title change passes through (the Document's and the root's
  children, the head's children, that title element's subtree), looked up
  again on each observed change and each report: `document.write`
  replacing the document, a replaced root, a title before the head's or in
  the body, and an SVG title all report, and a busy body costs it nothing.
  Its stated limit is a title a script inserts into or removes from the
  body after load while nothing observed changes; a read asks the document
  and is right. A
  read (an
  operation's, `browser_pages`', a selection's, the history refresh after a
  report) is stamped before it asks; the address and the back/forward state
  each keep the stamp of the write that set them, and a read's part is
  committed only if no write that began later set that field. An
  operation's read commits inside the hold of the page lock it read under,
  so no navigation runs between them. A write that does not know the
  back/forward state (an engine report, a snapshot) keeps what the tab
  shows. A suspension reads the address again while an engine report lands
  during its read, up to four reads, and saves the read that no report
  overtook (the last one when the page keeps navigating). Shutdown, a crash
  and a jar switch save the cached address without reading: shutdown may
  run on the engine's UI thread, a crashed page answers nothing, and a
  switch suspends in one lock hold.
- The chat header's browser button is present wherever the native browser is
  usable for the thread, tabs or not. With no tab it opens the pane on a new
  tab, as a mod-clicked link does on a tab at the link. A new tab the user
  opened takes the address bar's focus, text selected, once it is ready,
  unless the user pressed a key or the pointer first or another tab is shown.
- A thread's pages outlive a stop of its provider session, so a restarted
  session finds the same tabs. Deleting or archiving the thread closes them and
  forgets their saved copies; an archive that keeps a re-engaged session keeps
  its pages. A jar profile with no pages is disposed. An engine with no
  profiles is stopped after a bounded idle delay.
- When the session reaper ends an idle session, and only then, the thread's
  pages are suspended: each engine page is closed and the tab stays in the
  companion and `browser_pages` (`suspended: true`) with its id, address,
  title and label. The page a mounted pane is showing stays live, since
  unloading it would only make the pane reload it. App shutdown saves every
  open page the same way, so it returns after a restart as a suspended tab,
  and cancels a suspension in progress before its next page. The next tool
  call on a suspended page, or presenting it (a mounted pane whose active tab
  is suspended selects it at once), reloads its address under the ordinary
  navigation policy; a refused or failed reload leaves it suspended and
  returns the error to the tool or the pane's banner. Showing the companion
  does not reload the page itself, so a page that cannot be reloaded still
  opens the pane, where it can be navigated elsewhere or closed. A reload
  starts a fresh page: history, console, downloads and clipboard do not
  return. A suspended tab counts toward the thread's page limit but holds no
  workspace or process page slot. Pages are never suspended for being hidden.
- A page whose process ended becomes a suspended tab with an `error` naming
  the reason, at its last address (`internal/browser/crash.go`). The signals
  are CDP `Target.targetCrashed` (hosted and headless), WebView2
  `ProcessFailed` of kind render or browser process exited (other kinds are
  logged and the page keeps running), WebKitGTK `web-process-terminated` and
  WKWebView `webViewWebContentProcessDidTerminate:`. A lost whole browser
  is the same for each of its pages: a headless profile's Chromium that
  ended without its Dispose, and a hosted CDP connection that ended without
  the engine's Stop (the launcher, its relay or WebView2's browser process
  went). The native engines' browser is the app's own process. Presenting a failed tab
  does not reload it; its Reload, or a tool call on it, restores it under the
  navigation policy like any suspended page. The failure is not saved, so
  after a restart it is an ordinary suspended tab.
- While an agent tool call operates on a page, the page's state carries
  `agent` (`internal/browser/activity.go`); it clears 1.5 s after the last
  such call ends, and the state is pushed only when it changes. It is
  presentation only: user actions on the page take no part in it and are
  never refused because of it.
- Suspended pages and the thread's jar choice are saved as one JSON file per
  thread under `browser-pages/` in the config directory, written atomically
  (`internal/browser/page_records.go`); a thread with no saved page keeps a
  file only for a non-default jar. At boot, a file that is unreadable,
  invalid (an unknown jar included) or belongs to a deleted or archived
  thread is removed with a log line, so a delete or archive that cannot
  remove a thread's file logs it and still completes. Clearing site data and
  changing the persistence setting remove every saved page and keep jar
  choices; turning the browser off removes both.
- App shutdown saves the open pages, cancels calls, disposes profiles, stops
  the engine, then closes the MCP listener. Wails runs that shutdown ON the
  desktop UI thread, which the WebKit engines dispatch every native call to,
  so when the caller is that thread the profiles are disposed inline on it
  (`engineUIThread`) rather than fanned out to goroutines that could only wait
  for it. Saving uses the pages' cached address and title and makes no engine
  call.

## The pane's chrome

The tab strip and address row (`frontend/src/lib/components/browser/`)
behave as a desktop browser's:

- Tabs share the strip evenly between 200 and 72 px; past that the tab row
  scrolls sideways (a vertical wheel scrolls it too) and keeps the active
  tab in view. The new-tab button follows the last tab outside the
  scroller, so it never scrolls away. A tab shows a stepped spinner while
  creating, restoring or loading, its favicon or a globe otherwise, and an
  alert when it failed; a crashed tab's tooltip and the page area give the
  reason, and the page area has its Reload. A pulsing agent icon marks a
  page an agent tool is using. Close shows on the active tab and on hover, always
  under compact; a middle click closes. A double-click on the strip's empty
  space maximizes or restores the pane, as a pane title does.
- A tab's context menu (`BrowserTabMenu.svelte`, the shared `ContextMenu`)
  has Reload, Duplicate, Copy address, Open in system browser, Close, Close
  other tabs, Close tabs to the right and Reopen closed tab. Each Manager
  action is a `BrowserCompanionAction` kind. Open in system browser goes
  through the external URL path, which refuses `file:`, so the item is
  disabled for a file tab.
- The address row's buttons never shrink and the address keeps at least
  6rem. Below a 32rem row (36rem under compact) the page's size label, cookie
  jar, open file, show in folder and devtools move into a More menu.
- Focusing the address selects it; a second click places the caret. Enter
  (plain or with Shift) navigates; Escape puts the page's address back and
  moves focus to the page area. DOM focus only: the native page takes
  keyboard focus from a click, as § Keyboard says. Switching tabs replaces
  an edit with the new tab's address.
- Typing in the address opens a suggestion list
  (`BrowserAddressSuggestions.svelte`, matching in `browserAddressMatch.ts`):
  the thread's other open tabs (taking one switches to it), the dev servers
  the thread's processes serve (`stores/devServers.svelte.ts`, rows whose
  `threadId` is the thread's), then its recent addresses, each address once
  and the field's own tab never. Arrows move the highlight without changing
  the typed text, Enter or a click takes a row, Enter with none highlighted
  navigates the text, and Escape closes the list before it reverts the
  field. Nothing is fetched from the network while typing. The list is a
  `Popover`, so the native view yields while it is open. Focusing the field
  reads the recent addresses (`BrowserCompanionRecentAddresses`) and
  rescans the dev servers, so neither rides the state push.
- The recent addresses are the Manager's, per thread, in memory only
  (`internal/browser/recent.go`): the last 50 addresses an open loaded
  (the address bar's navigation, `browser_open`, `browser_open_file`),
  newest first, one entry per address. A failed open, a page left on a
  blank or internal document, a link followed in the page, history moves
  and reloads add nothing. A private jar keeps none; a jar switch, Clear
  site data, turning the browser off or changing site-data persistence,
  and deleting or archiving the thread forget them, and an open those
  overtook records nothing. Every forget advances one Manager-wide
  generation that each thread's state push carries (`recentGen`); the
  field drops its copy of the list, and any read in flight, when the
  generation, the thread or the active tab changes, and reads again while
  it has focus.
- While the page loads, reload is stop. `StopCompanionLoad` takes no page
  lock, so it stops an address-bar navigation in flight. Once the engine
  has stopped, the navigation it interrupted (not one queued behind it)
  answers its abort (CDP `net::ERR_ABORTED`) without an error; any other
  failure stands. Every engine stops a load (CDP
  `Page.stopLoading`, `webkit_web_view_stop_loading`, WKWebView
  `stopLoading`).
- The address takes a backend path, a workspace-relative file, a `file:`
  URL, a host, or a search. A Windows drive path (`C:\x`, `C:/x`), a UNC
  path (`\\wsl.localhost\<distro>\...`, `\\wsl$\<distro>\...`) and the
  `file:` URLs of either resolve to the backend path through the engine's
  renderer mapping (`engineFileURL`, `wslpath -u` on the WSL build) before
  file authorization. A path the mapping cannot reach (another
  distribution, a network share), or any Windows path on an engine without
  one, is an error naming the path, never a search
  (`internal/browser/companion_navigate.go`).
- "Open file" lists the thread's workspace on the computer that owns it
  (the sidebar's `DirectoryBrowser`) and searches it with the composer's
  workspace file search. The chosen absolute path navigates the active tab,
  or a new tab when none can take an address, under the file
  authorization; a refusal is the pane's banner.
- Downloads (`internal/browser/downloads_thread.go`). Once the thread has a
  download the address row shows a downloads button, never moved into the
  More menu. While downloads run it shows how far they are in eighths, or a
  stepped spinner when no running download announced its size. It opens the
  thread's downloads, newest first (at most 20; a running one is never
  dropped): name, the page it began on, received and total bytes, and the
  failure. A running one cancels; a finished one shows in the file manager
  (the reveal hand-off of Show in folder) and, for a type every engine shows
  in a tab (PDF, PNG, JPEG, GIF, WebP, BMP, text, JSON), opens in a new tab.
  Script-capable types (HTML, SVG, XML) are never opened from the list. A
  row acts only on the file its download wrote (device, inode, size and
  modification time against the file as it finished, which a rename keeps
  and a later download on a reused inode cannot match): one moved away,
  changed or replaced since, including by a later download given the freed
  name, is refused with that reason. There
  is no "open with the system's default app": the OS opener refuses `file:`
  URLs (`internal/externalurl`). Clear list drops the settled downloads and
  deletes no file. The list is in memory and goes with the thread; the page's
  own log for `browser_downloads` is separate and goes with the page. The
  list rides the thread's state push: a start, a finish, a failure and a
  cancel push at once, byte progress at most once a second per thread. A
  download that settles other than by the user's cancel is announced once
  (a Kind `download` event): a toast with Show in folder when it finished,
  an error toast with the reason otherwise. `canceled` means AO stopped it:
  the user (settled by the engine's own report, so a finish that beats the
  cancel keeps its file; a repeated cancel never withdraws the first), under
  headless Chromium a quota or its size, or its page leaving by any path (a
  close, a suspend, the engine's close, Clear site data, disabling the
  browser), which settles it at once and releases any reservation; a
  download that starts on a page already leaving is refused to the engine
  and never listed; an end the engine reports on its own is `failed`, with
  the engine's reason when it gives one, and so is a download whose
  Downloads folder cannot be created or written. Where the file goes is
  in [Browser hardening](#browser-hardening).
  On Windows through WSL the launcher takes every download a pane page
  starts (WebView2's `DownloadStarting`, handled, so WebView2's own
  download dialog never opens), WebView2 saves it, and the launcher reports
  it to the backend as whole snapshots (`internal/webview2host/download.go`,
  `internal/browser/hosted_downloads.go`); no download crosses CDP. The
  launcher numbers each download's snapshots, and the backend applies a
  download's snapshots one at a time, each only if newer than the last it
  applied, so a lost report heals on the next and reports that overtake
  each other settle in the launcher's order: the first it gets starts the
  row, a repeat or an older one changes nothing, and nothing after the end
  reopens it. A cancel is answered with the download's snapshot, and a
  running download the launcher has not reported for ten seconds is asked
  for one, so a row whose end was lost settles. An answer settles or
  advances a row and never creates one; for an id the launcher has no
  record of it says so, which ends the row only if it still runs. WebView2
  resumes a dropped download itself, as a new download start that carries
  the interruption's reason; the launcher keeps it as the same row, saving
  to the file the download started with, so one click is one row, one file
  and one end. A resume names nothing else of its download: with two
  downloads of one address running and both resumed, each may take the
  other's row. A second download a script starts with
  no user gesture gets WebView2's own "download multiple files" prompt in
  the page and has no row until it is allowed. A window WebView2 opened
  itself rather than as an AO tab keeps WebView2's own handling: its
  dialog, the file in the Downloads folder, no row.
- Chords, live while the browser pane is the focused pane
  (`browserPaneFocused`): new tab `mod+t`, focus the address `mod+l`, reload
  `mod+r` and `f5`, next and previous tab `ctrl+tab` and `ctrl+shift+tab`,
  reopen closed tab `mod+shift+t`, beside close tab `mod+w`. A bare `f5` is never claimed from the page.
  Next and previous tab send a relative `step` that the Manager applies to
  its own order and selection (`StepCompanionPage`), so presses faster than
  the state push each move one tab.
- The desktop and Windows launcher windows bind `mod+r` to reloading the app
  before the SPA sees the key. While the browser pane is focused and the
  effective keybindings run a command for `mod+r` there, the SPA asks over
  the webview's raw message channel for the chord. The window then hands it
  over, and the SPA replays it as a keydown through the keybinding
  dispatcher, as it does a chord claimed in the page; a chord nothing runs
  goes back and reloads the app
  (`internal/uikeys/pagechords.go`, `frontend/src/lib/native/shellChords.svelte.ts`).
  `mod+shift+r` always reloads the app. The `--connect` window hosts no
  native page and keeps the plain binding.

## Keyboard

The page is a native view beside the SPA's, and the window has one keyboard
focus. Once the user clicks into page content every key goes to the engine,
so AO's chords have to be taken back at the engine:

- The App reduces the effective keybindings to an `AcceleratorSet`
  (`internal/keybindings/accelerator.go`, the Go mirror of the frontend chord
  grammar with `mod` resolved per platform) at startup and after every
  keybindings write. Only chords with ctrl, meta, or alt are in it: bare and
  shift-only keys are typing and always reach the page.
- Each engine gates key events on it before its document sees them: the
  WKWebView subclass's `performKeyEquivalent:`/`keyDown:` (only while the page
  view is first responder), a capture-phase `GtkEventControllerKey` on the
  WebKitGTK view, and WebView2's `AcceleratorKeyPressed` in the launcher, which
  matches in its own process against the set the backend ships in an
  `accelerators` directive and answers with an `accelerator` report.
- A bound chord is swallowed by the engine and hands keyboard focus to the
  app's webview inside the key event, before the next key and before the
  chord is routed: the Manager does it for every chord it claims
  (`keyChord`, through `engineAppFocus` on WebKitGTK and WKWebView), and the
  launcher does it where it claims for WebView2 (`acceleratorPressed`,
  focusing its window, whose WebView2 controller takes the focus), only
  while that window is in the foreground. A chord AO does not claim, and a
  bare Escape, leave focus in the page. No command focuses anything for
  this; the command puts DOM focus where it wants it (the find bar's input,
  the palette), and the keys typed next reach it.
- A page that is not visible never holds keyboard focus. Each way a pane
  stops showing a page (a tab switch, the pane closing, a compact screen
  change, the companion hidden) is the engine's hide of that page, and the
  hide, like the close of a page, gives focus the page held to the app's
  webview through the same code as a claimed chord's handoff
  (`ao_wk_focus_app`, `ao_wkv_focus_app`, and the launcher's
  `releaseFocus`). Showing a page takes no focus.
- The pane hands the keyboard to the page the way a browser does after the
  address bar's Enter or Escape, as a focus intent: a transient request
  that one page take the keyboard, named by the pane mount and a number per
  mount (`focus_intent.go`, `focusBrowserPage` in the frontend store, host
  window only). Being presented, visible or in the foreground makes a page
  eligible; none of them overrides the user's next focus choice. The store
  withdraws the intent on a focus or pointer press outside the page area
  (the composer, the address bar again), another active tab, or the pane
  unmounting, whatever the arm's answer was and however long it took,
  posting the withdrawal to the Windows launcher directly
  (`ao:pane-focus:`, beside the pane geometry) and sending
  `focus-page-withdraw`. The `focus-page` action arms it in
  `Manager.FocusCompanionPage`, which applies it only while the mount
  presents the named page, waiting through a restore or a layout until the
  presentation pass shows it (`applyFocusIntent`, never `ShowPage`), and
  drops it once withdrawn, superseded, its tab closed or ten seconds old
  (`FocusIntentMaxAge`). The engine checks the page is shown and the intent
  live where its hides run, immediately before moving focus:
  `ao_wk_view_focus_presented` grabs GTK focus on the WebKitGTK view,
  `ao_wkv_view_focus_presented` makes the WKWebView first responder, and the
  launcher applies a `focus` directive with
  `ICoreWebView2Controller::MoveFocus` (programmatic) only while it shows
  the page, its window is in the foreground and it has not heard the intent
  withdrawn or replaced (`focusDecision`, `FocusIntents`), so AO never takes
  the OS focus from another application; a directive for a page its own
  presentation hides waits and is applied once at the page's next show,
  under the same checks and the same age bound. Nothing undoes a focus that
  landed, so one can still land in the instant between the user's choice
  elsewhere and the withdrawal reaching where focus is applied: one loopback
  RPC from the SPA to the backend on Linux and macOS, one WebView2 web
  message from the SPA to the launcher on Windows. The outcome
  reaches the pane as a Kind `focus` event naming the intent; a refusal
  (the launcher's `focus-failed` report) takes the pane's action banner
  unless the intent is no longer the latest or a newer action's banner is
  up.
- Only the host's own window mounts a pane (`BrowserCompanionPaneAttach`
  refuses any other connection; see transport.md, the host-window cookie):
  the native view lives in that window. A browser tab on the host offers
  and opens no pane, as a connected browser does. Both openers, a state
  push (which the header button's show and new tab produce) and the reconcile on a thread
  mount, go through `openBrowserCompanion`, which asks
  `canUseNativeBrowser`; a mod-clicked link asks it before minting a
  page, and no layout restore or companion stash reopens a browser pane.
  A thread's pages are presented in one mount, its earliest attached live
  one (`presenterLocked`), until that mount detaches; the presentation,
  the pages' size, and the routed chord and Escape all name it.
- The chord then reaches the frontend as an `accelerator` event on
  `browser:companion-state`, carrying the BOUND spelling (Shift glyph
  variants are normalized by the match) and the presenting mount
  (`mount`). Every host client receives the event, and only the one holding
  that mount replays it; a chord in a page no pane shows is dropped. An
  `escape` event carries the mount too, and closes only that client's find
  bar. The
  store focuses the thread's
  browser companion and replays the chord as a window keydown, so `when`
  gates, rebinds and the palette all behave as if the SPA had been focused.
  A bound chord whose `when` fails there is a no-op — the page never gets it
  back, which is the price of answering synchronously. The replay leaves
  DOM focus where the SPA last had it, so a surface the chord opens returns
  focus there when it closes: focus stays in the app, and the page gets it
  back from a click.
- A bare Escape is never AO's in the page: WebKitGTK and WKWebView offer it
  to the gate, which always lets it through, and while a pane shows the
  page the Manager announces each press as an `escape` event (thread and
  page id) on `browser:companion-state` for the pane's find (see Find),
  except a key held down (WKWebView's `isARepeat`; on GTK 4, which reports
  no repeat, a press while the view has not seen its release or lost
  focus) and a press during an input method composition (the view's
  preedit on GTK, the responder's `hasMarkedText` on macOS). The native
  code only gathers those facts; `pageEscapeAnnounced` decides. An Escape
  with a modifier is an ordinary chord.

## Find

mod+f in the browser pane finds in the page the pane shows, through the
engine's own find-in-page. The pane registers a find provider only while the
shown page's engine has find (`findMode` on the companion state), so the
chord falls through elsewhere. A chord pressed inside the page arrives through
the accelerator path above. Whether AO's bar is open is the pane-find
store's alone; the backend holds only what the engine holds.

- The Manager holds the page's search (`internal/browser/find.go`,
  `BrowserCompanionFind`): whether one runs, its query and current match,
  and its owner (`start`, `next`, `previous`, `stop`). `start` runs only on
  the active page of a shown companion, from a mounted pane. A search ends
  when the page commits another document, stops being the shown active
  page, is suspended or closes, when the owning pane mount goes (unmounted,
  or its connection closed), or when the browser closes; that end is a
  `find` event on `browser:companion-state`, the bar showing the search
  closes, and the engine's search is stopped with it. `stop` never
  restores a suspended page and answers for a gone page as for no search.
- A search's owner is a pane mount and a sequence (`seq` on
  `BrowserFindAction` and `FindState`). Every `start` (each search of a
  bar's query, each open of the engine's own find in "engine" mode) takes
  the next number of one frontend counter, and the Manager refuses a start
  whose sequence is not above every start it took from that mount. A step
  names the exact sequence it steps. A `stop` names the highest sequence
  the provider issued when it made the stop, and ends the mount's search,
  on whichever of the thread's pages it runs, exactly when that search's
  sequence is at or below it: a stop never ends a later search, and one
  made after a start that was refused, dropped or never answered ends the
  search the mount held before. Every answer and end carries the owner
  with the page (id and incarnation), the document the search started on
  and a generation (`FindState`). A `start` replaces the page's search;
  another mount's search replaced so hears its end, and the engine's search
  is replaced rather than stopped. A step whose search is not the page's,
  or a `stop` that finds none at or below its bound, changes nothing, and
  its answer is marked `foreign` and names the search's real owner, "" for
  none, so the provider never takes it as an acknowledgement. Generations come from one
  counter across the Manager, so their order holds across a suspend and
  restore, and an end is always newer than the answers of the search it
  ends.
- Every find transition of a page is serialized on the page lock
  (`managedPage.mu`, which every engine call already takes) and holds it
  from its checks through its engine calls to the state it publishes:
  `start`, step, `stop`, every end and the engine's cleanup. An action's
  10 s deadline starts before it waits for the lock, and an action whose
  deadline passed, or whose context ended at server shutdown, while it
  waited is refused before it publishes anything; every engine call under
  the lock is bounded by the transition's deadline, and a stop that times
  out stays owed. `BrowserCompanionFind` is not caller-bound: a `start`
  still running when its connection closes finishes, and the connection's
  cleanup, which runs after the transport joins its handlers, detaches the
  mount (`DetachPane`), which ends that search and stops the engine's. An end raised where waiting is not allowed (the
  engine's UI thread, a presentation change, a mount detaching, a page
  closing outside its lock) is submitted to the page without blocking and
  applied by the next transition, or by a transition of its own when none
  runs, which `Manager.Close` joins after refusing new ones. Each
  transition applies what was submitted before it first and what was
  submitted during it last, so an end lands strictly before or strictly
  after an action. A presentation end rereads what is shown when it is
  applied. Ends submitted while a long page operation (a screenshot) holds
  the lock wait for it: for that long the search outlives the document or
  the presentation, and nothing else is out of step. An action
  revalidates its page under the lock before any driver call, so one that
  waited out a close never reaches the closed driver, and WebKitGTK and
  WKWebView also refuse any operation on a page whose lifetime ended
  before it reaches the view.
- Whether the engine holds a search (highlights, a match selection, its own
  bar) is tracked apart from the logical session: every `FindStart` sets
  it and only a `FindStop` that succeeded clears it, including after a
  failed start or step. A search the engine holds that no session wants is
  state every transition settles first and last, retrying a stop that
  failed before, whoever owns the search; a `stop` reports each failure of
  that stop. A page that holds neither a search nor an owed engine stop is
  the only one a presentation change skips (`findSession.settled`).
- A new document is each engine's commit signal, so a reload ends the
  search and a same-document navigation (a fragment, the History API)
  keeps it: WebKitGTK `load-changed` with `WEBKIT_LOAD_COMMITTED`,
  WKWebView `webView:didCommitNavigation:`, and CDP `Page.frameNavigated`
  for the main frame (hosted WebView2 and headless Chromium), each reported
  through `pageHooks.DocumentCommitted`. The hook runs where the engine
  reports the commit (the UI thread for WebKitGTK and WKWebView) and only
  stamps the page's next document epoch; the engine submits the end it
  returns in the same callback, without waiting, and that end ends only a
  search started on an earlier document, so a late end never ends a
  search started after the commit. A CDP commit is stamped when its event
  reaches Go, so a search answered first on the new document is ended (the
  bar closes), never left showing the old document's results.
- Escape closes AO's find bar through `find.close` when the SPA has
  keyboard focus. A bare Escape pressed in the page is always the page's
  (see Keyboard); while a pane shows the page, the Manager announces it as
  an `escape` event on `browser:companion-state`, and the pane closes its
  bar on that event if the bar is open for the page it shows.
- The pane's provider (`frontend/src/lib/stores/browserFind.svelte.ts`)
  turns each reader action into an intent (start with its query, step,
  stop) carried with its page, mount and sequence to its answer, and runs
  them one at a time, in order, on its own queue; its header holds the
  state table. It sends `start` on a debounced non-empty query, and when
  the query empties, the bar closes or the pane unmounts it sends `stop`
  through the newest sequence it issued, unless a stop was made since,
  without tracking which search the backend holds. A queued start is
  dropped when the query changes. The bar shows one sequence: an answer or
  failure changes what it shows only for that sequence, and an end or a
  foreign answer for it closes the bar. A failed start or step of the
  shown sequence is that search's result: the bar shows its error until
  the next query or the bar's close; in engine mode, which has no bar, a
  toast reports it. Every failed start or step is
  followed by a stop through its sequence. A queued stop is removed only
  by running. A stop that fails never changes what the bar shows; the
  pane shows a find cleanup error, in its own entry apart from browser
  action errors, until a stop through it or further is answered or the
  pane unmounts. It is sent again on the provider's own timer, at most
  three times in all, until it or a later stop through it is answered or
  the pane unmounts, whose mount detach ends the search. A
  stop that never succeeds leaves the page's highlights until the next
  start, a navigation, a page switch or the pane unmounting, each of which
  ends the engine's search. The Go find tables run each sequence against a
  port of the provider, including starts the Manager refuses, calls left
  outstanding across later reader actions, the stop retries and named
  orders from review, and record each provider's view of it, checking
  after every sequence that a bar shows a count, a current match and
  enabled stepping exactly when the Manager holds a search for its mount
  at the sequence it shows, with that search's count; the frontend replays
  those scripts, the bar's final display included,
  against the real provider and pane-find store
  (`frontend/src/test/fixtures/browserFind/`, regenerated with
  `AO_UPDATE_FIND_SCRIPTS=1` on the tables in `find_pairs_test.go`).
- WebKitGTK (`findMode` "bar") drives the shared find bar through
  `WebKitFindController`: a case-insensitive, wrapping search that counts up
  to 1000 matches (past that the count shows as `1000+` and the position as
  unknown), `search_next`/`search_previous` for Enter and Shift+Enter, and
  `search_finish` on stop, which clears only the highlights, so a stop also
  drops the selection that marks the match, as WKWebView does. The engine
  reports no match index, so the Manager counts the steps.
- WKWebView ("bar") uses `findString:withConfiguration:completionHandler:`.
  It reports only whether a match was found, so the bar shows "Found" or "No
  results" without numbers. A search starts from the selection, so a start
  drops the selection first, and a stop drops the selection that marks the
  match. On both engines that clearing (`webkitClearSelectionScript`) runs
  in an isolated script world (`webkitFindWorld`), so a page's own
  `getSelection` or `removeAllRanges` never reaches it, over the page and
  each same-origin frame on its own, and any failure there fails the stop,
  which stays owed. Frames are not enumerated, since `window.frames` misses
  an iframe in a shadow root: `webkitFindFrameScript`, installed in every
  frame at document start in the same world, announces each frame to its
  parent, which keeps its own direct children (bounded, deduplicated,
  pruned when used). The stop then posts a clear to each announced child of
  the top frame, and a frame obeys a clear only from its parent, clearing
  itself and passing it on to its own children, so a clear only travels
  down and no frame can clear the page above or beside it; the top frame
  obeys none. The listener is a capture-phase listener added before any page
  script, so a page cannot swallow the message first, and the page never
  sees the find's messages. A document regenerated with `document.open`
  runs the script again in the same window; each run replaces its listener
  and announces again, so the window keeps one listener and its children.
  The console capture also runs again: it wraps the console's methods only
  on its first run in a window, since the console and its wrappers outlive
  `document.open`, and each run re-adds only its error and
  unhandledrejection listeners. Nothing waits for a frame, so a frame cannot
  fail the stop, and a frame that runs no script (sandboxed without
  `allow-scripts`) keeps its selection. WebView2's own find bar has no
  selection housekeeping of AO's to fix.
- Hosted WebView2 ("engine") opens the engine's own find bar
  (`ICoreWebView2Find` with the default dialog left on), seeded with the
  SPA's selection or else the page's, and moves keyboard focus into the page.
  The engine's bar counts, steps and closes itself; AO's bar is not shown.
  `ICoreWebView2Find` has no close event, so AO never models that bar as
  open: the pane's find does not read as open, `find.close` does not apply,
  and the launcher's `claimChord` never takes an unmodified key, so Escape
  goes to the engine and the page. mod+f again seeds and opens the bar. The
  Manager's search ends as above and stops the engine's find, which does
  nothing once the user closed the bar. A runtime without the API is
  reported at page create (`find-unavailable`) and the page has no find.
- Headless Chromium has no find: serve mode has no pane to find in.

## Tool surface

All results are bounded. `browser_open` and `browser_open_file` create a new
background page when `page_id` is omitted; supplying one intentionally
navigates that existing page. Other page-scoped tools may omit `page_id` only
while the thread owns zero or one page. With multiple pages, AO returns a
bounded ambiguity error naming the available IDs/labels instead of guessing.
The MCP initialization instructions state the same rule so small subagents get
it without relying on an optional skill.

| Tool | Purpose |
|---|---|
| `browser_session` | Name the thread-owned automation session and its cookie jar. |
| `browser_open` | Create a background HTTP(S) page, or intentionally navigate an existing `page_id`. |
| `browser_new_page` | Create a blank background tab without navigating it. |
| `browser_open_file` | Create a background page for an existing regular HTML, PDF, image, text, or other browser-renderable file, or navigate an existing `page_id`. |
| `browser_pages` / `browser_select_page` | List only thread-owned tabs and explicitly pin one as the companion tab. Selection does not show the companion. AO never inspects another system browser. |
| `browser_label_page` | Set/clear a short case-insensitively unique label for cross-agent coordination. |
| `browser_close_page` | Close one caller-owned page. |
| `browser_visibility` / `browser_viewport` | Explicitly present a `page_id`, hide the companion without closing pages, and get/set/reset the thread viewport: following the pane by default, or a pinned bounded size. Hidden companions present nothing and cost nothing. |
| `browser_snapshot` | Return URL/title, bounded visible text, and bounded interactive-element records with CSS selectors and reusable DOM node IDs. |
| `browser_screenshot` | Return a JPEG of the viewport, a bounded clip, or a height-capped full page. |
| `browser_locator` | Stateless Playwright-equivalent locators (CSS, role/name, label, placeholder, text, test ID, scopes, filters, union/intersection, indexes, and nested frames) with strict query/read/action/check/select/wait behavior and optional navigation/download expectations. |
| `browser_click` | Dispatch a trusted CDP click to a CSS selector. |
| `browser_type` | Focus, optionally clear with platform-native select-all, and dispatch trusted text input. |
| `browser_press` | Dispatch a keyboard chord/key. |
| `browser_pointer` | Coordinate click/double-click/move/scroll and bounded-path drag. |
| `browser_dom` | Visible-DOM snapshot and click/double-click/type/key/scroll by snapshot node ID. |
| `browser_scroll` | Scroll the page or a selected element. |
| `browser_wait` | Wait for duration, locator state, URL glob, commit, DOMContentLoaded, load, or 500 ms network idle. |
| `browser_history` | Back, forward, reload, or stop. |
| `browser_evaluate_readonly` | Bounded inspection JavaScript in the `browser_evaluate` forms, run synchronously: it cannot `await`, and a promise result fails. A CDP-backed engine rejects a possible side effect in the engine itself, which includes some reads such as `getElementById`; an engine that cannot says so in the tool result rather than differing silently. |
| `browser_evaluate` | Explicit mutation-capable JavaScript escape hatch; bounded and serialized. Takes an expression, or a function called with an optional JSON argument, as Playwright's evaluate does; other code runs as statements in an async function body and gives a value only through `return`. The engine reads the code: the Manager offers it as an expression, and as statements only when the engine reports that does not compile, so no code runs twice ([`evaluate.go`](../../internal/browser/evaluate.go)). Nothing uses the page's `eval`, so a page CSP without `'unsafe-eval'` allows it. The page encodes the result as `JSON.stringify` does, plus DOM nodes, collections, errors, `BigInt` and cycles, bounded in size and depth. |
| `browser_clipboard` | Read/write a bounded tab-local MIME clipboard. Paste/copy chords bridge this clipboard without touching the OS clipboard. |
| `browser_console_logs` | Read the bounded tab console/runtime ring by level and substring. |
| `browser_downloads` | List or wait for browser downloads (`in_progress`, `completed`, `failed`, `canceled`, with `totalBytes` when known) and return each finished file's path as the backend sees it. |
| `browser_assets` | Inventory resource, DOM-attribute, computed-style, and inline-SVG assets; bundle selected images, fonts, stylesheets, or videos with page credentials into an AO-owned artifact directory. |

## Parallel agents

Provider subagents inherit the parent thread's MCP capability, so they can
intentionally inspect or modify any page in that AO thread. Generic MCP does
not carry a portable Claude/Codex child-agent identity, so AO does not pretend
it can assign invisible per-subagent sessions. The page handle is the work
handle: a parallel agent opens a page, retains its returned `page_id`, and
passes it on later calls. A smarter agent may call `browser_pages`, locate an
existing page by label, URL, or title, then deliberately reuse its ID.

Ordinary activity updates recency but never changes the companion selection.
Only `browser_select_page`, a user tab selection, or
`browser_visibility {visible:true,page_id}` changes the presented page. Thus
background subagents cannot make a visible companion flicker between tabs.

## Local-file boundary

The browser never launches with `--allow-file-access-from-files`. Public page
JavaScript therefore does not inherit the MCP caller's file authority.
`browser_open_file` resolves symlinks and permits files inside the thread's
workspace or project root, and the workspace's own finished downloads, which
the pane's downloads list opens from: under headless Chromium its AO-owned
download directory; on every other engine the file of each finished
download of the workspace that AO still lists, in any thread's pane list
or a live page's `browser_downloads`, while it is still the file the
download wrote (device, inode, size and modification time), never the
Downloads folder around it (`internal/browser/download_files.go`). A row
that is listed is openable and one that is gone is not, so a download of
an earlier run of the app is an outside file. Outside files
require
`browserAllowOutsideWorkspace`, an explicit off-by-default setting. This gate
exists because the MCP server runs outside provider process sandboxes; OS-user
permissions alone would make it a sandbox bypass.

## Browser hardening

The MCP endpoint validates every request before it dispatches a method:
the peer must be on loopback, the request must carry no `Origin` header,
and it must declare `Content-Type: application/json`. The per-thread
capability URL is therefore not the only thing standing between a
document in a browser and the tools, and requiring JSON forces a
preflight the endpoint refuses rather than letting a `text/plain` CORS
simple request through unannounced.

The engine's own sandbox and site isolation remain enabled.
The manager uses a per-jar profile, grants no camera, microphone,
geolocation, notification, or system-clipboard permission, bounds navigation
and tool deadlines, caps pages and contexts, dismisses JavaScript dialogs,
blocks dangerous navigation schemes, and never exposes a debugging
endpoint outside the local controller process. Asset bundles go only to
AO-owned directories with file/count/byte caps.

A finished download ends up where any browser on the machine that runs the
page's engine puts it: that user's Downloads folder (`downloadSaving` in
`internal/browser/driver.go`). There is no setting.

- Launcher-hosted WebView2: WebView2's default, the Windows user's
  Downloads folder, under the name WebView2 gives (`name (1).ext` on a
  collision). WebView2 keeps the mark of the web. The finished file is
  openable from AO only while the path WebView2 reported is a regular
  file, not a link, of the size it reported; otherwise the row completes
  with the reason and opens nothing.
- WebKitGTK and WKWebView write each download only into the app's
  private staging folder in the Downloads folder,
  `.agent-overflow-downloads` (mode 0700), under a name the Manager gives
  as the engine asks for the destination: `dl-<process token>-<n>`, so a
  file another run left never collides
  (`internal/browser/download_staging.go`). The folder is resolved per
  download, created on demand with a missing Downloads folder, checked to
  be a real folder and not a link before every use, and removed once no
  download stages in it and it is empty. The Manager publishes a finished
  file into the Downloads folder beside it
  (`internal/browser/user_downloads.go`): it hard-links the staged file to
  the sanitized suggested name, or that name with ` (1)`, ` (2)` and on,
  or where the filesystem refuses hard links renames it with a rename
  that refuses to replace (`renameat2` `RENAME_NOREPLACE` on Linux,
  `renamex_np` `RENAME_EXCL` on macOS), taking the next name on one that
  exists, so it replaces nothing, copies nothing and nothing partial ever
  has a public name. A filesystem with neither fails the row with the
  reason. The row stays running until the file is in place and completes
  with its path; a publish that fails is the row's error and its staged
  file goes. Closing the browser waits for a publish under way. On Linux
  the folder is `XDG_DOWNLOAD_DIR` from
  `$XDG_CONFIG_HOME/user-dirs.dirs` (`$HOME/.config` when unset), read as
  xdg-user-dirs writes it (double-quoted, backslash escapes, a leading
  `$HOME`), else `$HOME/Downloads`; on macOS `$HOME/Downloads`.
- A Downloads folder that does not exist is created when a download
  starts. A path that is not a folder, a folder that cannot be created or
  written, or a staging folder that is a link fails the download on its
  row with the reason, before the engine writes; nothing falls back
  elsewhere. A Downloads folder relocated while the app runs takes the
  next download; one moved or removed while a download runs fails that
  download at its publish, and nothing is written anywhere else.
- In the Downloads folder AO applies no quota, per-download cap or
  pruning and never deletes or replaces a file it did not put there. A
  canceled or failed download leaves the folder as it was: WebView2
  removes its own partial file, and the other desktop engines remove
  their staged file and the emptied staging folder. A staged file left
  by a run that ended mid-download is removed when a download of a later
  run first uses the staging folder, once it is 24 hours old; a running
  download's file never is. A download another app process lets stall
  for 24 hours can lose its staged file that way and fails at its
  publish. Nothing that deletes a thread, clears site data, turns the
  browser off or prunes artifacts deletes a file in the Downloads
  folder.
- Headless Chromium in serve mode alone keeps downloads as AO artifacts:
  under the download's id in the jar's AO directory, moved on finishing
  to the workspace's under a sanitized unique name, capped at 512 MiB
  each and 2 GiB reserved per live workspace, whichever jars its pages
  are in. Downloads and asset bundles share a 4 GiB per-process artifact
  quota. Files older than seven days are pruned on the next artifact
  use, while newer files are never silently evicted to make quota room.

A download that begins in a page no thread owns is cancelled, except on
WebView2, where a window WebView2 opened itself (a popup the Manager did
not adopt as a tab) keeps WebView2's own download handling.

## Codex browser parity boundary

The bundled Codex browser skill's supported browser/tab, Playwright locator,
coordinate CUA, DOM CUA, clipboard, console, download, visibility, viewport,
and page-assets capabilities are the compatibility contract. AO exposes them
as stateless MCP calls rather than persistent JavaScript locator objects. The
behavior is equivalent: locator descriptions compose and can be reused across
calls, while every action resolves fresh against the current DOM.
The method-by-method map and its validation owners are recorded in
[`codex-browser-parity.md`](../references/codex-browser-parity.md).

Intentional ownership differences are not missing parity: AO lists only the
managed pages owned by the calling provider thread, keeps one isolated virtual
clipboard per tab instead of modifying the OS clipboard, and writes inbound
files to bounded AO artifact directories. Raw CDP, arbitrary profiles/browser
contexts, permission grants, unbounded network bodies/traces, and control of an
external user browser are not part of the Codex skill API and remain excluded.

## Provider live apply

- Claude: `mcp_toggle` removes/restores the built-in server and its tools.
- Codex: `config/mcpServer/reload` rebuilds the loaded thread's MCP manager
  from its original inline override and fetches the changed tool list.
- A server-side enabled check is authoritative even if a provider holds a
  stale tool schema. New calls are rejected immediately; the global setting
  also stops the engine and thereby cancels calls already in flight.

The composer MCP menu shows the server as `ao-browser-tools`. Its row is a
normal thread/session toggle and defaults on. The Browser setting remains the
global kill switch; when globally disabled the row cannot re-enable it.

The Claude TUI provider is excluded: its PTY launch has no app-owned dynamic
MCP-config channel. User-configured MCP servers continue to work through the
CLI's normal configuration.

## Platform behavior

A native Linux desktop drives the WebKitGTK views embedded in the app's own
window: same tools, same authority, no second browser process, and site data
under an AO-owned profile directory. `browser_visibility` presents that page's
real view behind the pane's host rect.

A windowed macOS desktop drives WKWebView subviews of the app's own NSWindow
on the same terms. The tool contract, the bounds, and the JavaScript every
operation is expressed as are shared with the Linux engine — both are WebKit —
so the differences are only where the platform itself differs:

- **Site data is WebKit's directory, not AO's.** macOS exposes no documented
  way to place a `WKWebsiteDataStore` in a chosen path. On macOS 14+ each
  persistent jar gets its own store keyed by a digest of the jar (a worktree
  jar's is the workspace root's, as before jars); on macOS 13 there is no
  per-jar persistent store at all, so the site-data setting has no effect
  there and every jar is in-memory-only.
- **Clearing site data is WebKit's removal, not a directory delete.** Because
  WebKit owns where those stores live, Clear site data asks it to remove every
  data store AO created — all of them, and only them: the app's own SPA webview
  uses the unidentified default store, which is never one of them. A Mac with
  no persistent stores to remove, including every macOS 13 one, clears
  successfully with nothing to do.
- **Devtools are Safari's.** Views are marked inspectable, and inspection
  happens from Safari's Develop menu rather than an in-app inspector.
- **A full-page screenshot resizes the view.** WebKit's macOS snapshot cannot
  reach past the view's own bounds, so a full-page or clipped capture grows the
  view to the document, captures, and restores. Screenshots are normalized to
  one image pixel per CSS pixel so clip rects mean the same thing they do on
  the CDP driver.
- **`beforeunload` proceeds.** WKWebView exposes no public delegate for it, so
  a requested navigation continues — the same outcome the other engines reach
  by accepting the dialog.
- **Context menus need no suppression.** A hidden page is clipped out of the
  window and receives no mouse input, so the real site menu appears on the
  presented page and nowhere else.
- **Presentation scales the view's bounds.** The page's viewport is the
  view's bounds size; presenting sets the frame to the fitted rect and keeps
  the bounds, so AppKit draws the page scaled and maps input through the same
  transform. WebKitGTK reaches the same result with an exact allocation under
  a scale transform inside a per-page host widget.

The Windows/WSL deployment uses the hosted engine
(`docs/specs/embedded-browser.md`): a page is a WebView2 controller in the
Windows launcher's process, and the backend drives it over CDP through the
launcher's relay tunnel. The tool surface is identical, because the operations
are the same CDP calls; the user-visible half is a real browser view the
launcher positions over the pane's fitted rect, with the page's viewport and
presentation scale set through the CDP device-metrics override. Hidden pages
keep a visible controller inside a hidden container window so they keep
producing frames.

**Serve mode drives a headless Chromium** (`docs/specs/remote-access.md` §7,
and the operator's copy in [serve-mode.md](serve-mode.md) § Browser tools). A
backend with no window cannot host a view, so it launches its own browser
instead: one process per jar profile, started by that profile's first
page and stopped with it, sandbox on. The tool surface is again identical, for
the same reason the WSL one is — the operations are the same CDP calls. What
is absent is the PANE, which is window chrome: an agent on a serve host gets
the tools, and nobody gets a view of them. Selection is an explicit request
that only the serve boot makes, never an inference from the absence of a
window, or `go test` would start browsers. And nothing is ever downloaded: the
Chromium has to already be installed, or the host keeps the windowless answer.

**Ephemeral profiles** (a private jar, or every jar with the site-data
setting off) keep one contract on every engine: the profile is never part of
a persistent profile, and its data is discarded when the jar's last page
closes. Where the data lives meanwhile is the engine's. WebKitGTK uses an
ephemeral network session, which writes nothing to client storage, and
WKWebView a non-persistent data store. WebView2 uses an InPrivate profile,
whose storage the runtime manages and does not document. Headless Chromium
has no in-memory profile: it runs on a temporary user-data directory
(`os.TempDir`), which the profile's disposal deletes; one a crashed backend
leaves behind is deleted by the next engine start's sweep
(`internal/browser/headless_ephemeral.go`). An incognito browser context in
the profile's process would keep it in memory; serve mode has no pane to
choose a jar from, so that is not built.

Every OTHER windowless run — `--connect`, `go test`, and any desktop whose OS
is too old for its native engine — has **no browser engine and no browser
tools**, as does a serve host with no Chromium on it. There is no fallback
browser to launch, and a browser tool call on such a deployment answers one
sentence saying browser tools are not available there. The hello frame carries
the same answer as `browser` in its capability list, so a client can tell "no
browser on that machine" from "turned off in Settings". The mocked
boot modes (`--harness`, `--soak`) are the one exception: they pin a fake engine
so the companion pane's chrome, tab strip, and host rect render with nothing
behind them. That pin is default-on and lifted by one manual gate —
`AO_HARNESS_REAL_BROWSER=1` on an ATTENDED isolated boot (never a soak
autopilot), which turns `make harness-window` / `make harness-wsl` into
the real-engine rig described in `docs/specs/embedded-browser.md` §10.

The companion RPCs and URL/title state events are loopback-only because they
expose local files and authenticated browser content, and the pane mount drives
real native views in the desktop window. A LAN `--connect` client does not
receive or control this surface. Unsupported OS/arch combinations fail the tool
call without affecting provider or app lifecycle.
