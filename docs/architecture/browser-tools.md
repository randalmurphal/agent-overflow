# Built-in Browser Tools

## Product contract

Agent Overflow supplies one provider-neutral HTTP MCP server to every
headless Claude and Codex session on a deployment that HAS a browser engine.
The server is registered even when browser tools are disabled; the disabled
server advertises an empty tool list and never starts the engine. This keeps
the off-state cheap while allowing a live provider to rediscover the tools
without restarting its process.

A deployment with no engine (a remote `--connect` backend, `go test`, a serve
host with no Chromium installed on it) registers no server at all, so a
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

Browser pages start hidden: parked out of the window's layout rather than
opened anywhere the user can see. An agent explicitly presents a page with
`browser_visibility`; AO then opens an ephemeral browser companion beside that
thread and the engine positions that exact page's real view over the pane's
host rect. The user and agent share its URL, DOM, cookies, history, focus, and
tab set; no separate browser window or duplicate browsing session is opened.

The Browser settings panel also exposes a destructive **Clear site data**
action. It closes every page first, so a later teardown cannot write the
cleared state back, then clears BOTH places site data can live: the AO-owned
profile directory, and, on a deployment whose engine keeps its data
somewhere AO cannot reach by path, the engine's own store. On Windows/WSL
that store is the launcher's WebView2 user-data folder on the far side of
the WSL boundary, so the backend asks the launcher to release its browser
environment and delete the folder, and waits for the answer; on macOS it is
WebKit's own data-store directory. The button clears real state on every
platform, and a failure is reported rather than swallowed, never a silent
per-platform no-op.

## Ownership and lifecycle

- One lazily started engine per AO process. AO downloads no engine: the engine
  is the platform's own, hosted by the app or by its launcher.
- One isolated profile per cookie jar with at least one active browser page.
  Each thread's browser chooses its jar from the address row's cookie jar
  menu: `worktree` (default; the threads on a canonical workspace share login
  state, in a profile keyed on the workspace path), `project` (the project's
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
  is in. The CDP engines pause each document request and file read once,
  browser-wide, and answer it with the jar's policy: the authority of the
  page its frame belongs to, or for a popup not yet adopted, of its opener
  chain. One whose owner is not resolved yet is allowed at once what a
  page with no workspace's authority may open (a web address); one that
  needs an owner's authority (a workspace file) waits up to 2 seconds for
  its target, its popup's report or its frame, then is refused. A page
  attached after its frames were made, a popup at its adoption, reads
  them from its frame tree.
- Switching the jar with tabs open asks first in the pane. The pane sends
  the switch unconfirmed; the Manager refuses it, changing nothing, when the
  thread has a live tab as it captures the switch (`SetJar` with
  `JarSwitchIfNoLiveTab`, code
  `browser_jar_unconfirmed`), and the pane then asks and sends it confirmed
  (`JarSwitchConfirmed`). A tab still being created is not live: the switch
  creates it again in the new jar. The Manager then suspends every live tab
  into the new jar: its page closes and a suspended record keeps its id,
  position, label and address, the active tab staying active, and the
  records are captured once and saved before the switch takes effect, so a
  shutdown or crash mid-switch loses no tab, and a switch whose save fails
  changes nothing and reports the failure. From the capture to the switch
  taking effect, any change to the thread's tabs (one opened, closed,
  moved, relabeled, selected, suspended, restored, crashed or adopted)
  waits for the switch, one save long, and then applies in the new jar, so
  the tabs the switch suspends are the tabs it saved. A switch refused
  after its save (the app shutting down) saves the thread again as it is,
  so the saved copy never names a jar the thread is not in, and reports
  when that save fails; a close of the thread waits for the switch. The tabs come back through the
  ordinary restore: the pane brings back the tab it shows, and the others
  return when selected or used, with the navigation policy applied and no
  page history. A tab that cannot come back stays suspended with the error.
  No restore of the thread is admitted until every page it had closing at
  the switch is gone, old profiles' disposal included, and until then a
  closing page and its record hold one slot in the thread's cap; closing
  the record leaves the slot to the page. Any restore is admitted only while
  its record exists, and forgetting a record (closing the tab, clearing site
  data, turning the browser off) cancels its restore. A per-thread jar generation fences a create or restore that
  began before the switch, so it lands in the new jar, whatever ended it
  in the old one (a crash or close there decides nothing); a popup is admitted
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
- The host window receives every thread's state on
  `browser:companion-state` and keeps it only for the threads a surface
  shows (the watched threads): a thread that leaves them loses its snapshot,
  its place in the state order and its read in flight, and a surface that
  shows it again reads it again (`BrowserCompanionThreadState`).
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
- `pane.close` (Mod+W) on the keyboard's browser companion closes the tab
  it shows; the companion closes when its last tab does. The close names
  the tab the window showed as active at the keypress, or, for a chord
  pressed in a page, the page it was pressed in (the `accelerator` event's
  `pageId`), and takes its place in the pane's order. The tab's x and the
  tab menu's closes name their tab the same way. At its turn the Manager
  closes the named tab if it is still there and picks the next active tab
  in the same lock hold, so a close that arrives after the user picked
  another tab never closes that tab, and Mod+W and the x of one tab close
  it once. A tab still opening or
  restoring closes at once. Closing the companion any other
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
- The client sends a pane's action only from the pane's attached mount
  (`browserActionWire.ts`): until the attach answers the pane's tab strip and
  address bar are unavailable and its keyboard commands send nothing, and an
  action whose pane detached before it was sent is not sent. Its refusal
  goes to the banner of the mount that sent it; once that mount detached it
  is logged instead. Every send first checks that this window hosts the
  thread's browser. An action that names a tab (`navigate` among them) must
  name one.
- A pane's tab commands take effect in the order the user made them
  (`selection.go`, `internal/browser/turnorder`), though the transport runs
  each call on its own goroutine. Every pane action that selects a tab
  (`activate`, `step`, `show-browser`, and the `new`, `reopen`, `duplicate`
  and `download-open` that select the tab they open), every close
  (`close`, `close-others`, `close-right`) and a tab move (`move`) carries the pane's
  mount and a number the client takes when the user acts: an epoch and a
  sequence in it, consecutive from 1. The client starts a mount's next
  epoch with the first command it sends while none of the mount's is
  unanswered, so nothing of an older epoch is still meant to act; a
  re-attach is a new mount and starts again. A close or move must carry them; a
  selection made with no pane mounted (the
  chat header's show, a download opened from its toast) carries none,
  and the backend refuses an action that does not match its kind
  (`parseCompanionAction`). The Manager admits a mount's numbers in order:
  the next one acts at once, a later one waits for its predecessors' commit
  (never their restore or load), and each then does what it says and
  restores the tab it selected. When a second passes with no progress the
  count moves past the numbers not yet arrived: each is outstanding, and
  arriving later it is refused with an error and acts on nothing, so it
  never undoes a choice the user made after it. Every command names its
  tab or steps from what has arrived, so the numbers after an outstanding
  one act. A newer epoch ends the older one: what arrives of it is
  refused. A mount remembers 256 outstanding numbers; one dropped past that
  is refused like any other that lost its place. A repeat of a number already admitted, one from a mount the
  Manager does not have (detached, or another thread's), a waiter whose
  mount detaches, and a command admitted before its mount detached or a
  newer epoch began answer success and act on nothing; a reopen keeps its
  closed tab. More than 256 waiting in one mount are refused with an error,
  and that number, sent again, is treated as never sent. An agent's
  `browser_select_page` applies in arrival order and never waits. Closing a
  tab by its id reselects only when that tab was the active one.
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
  usable for the thread, tabs or not, and elsewhere only while the
  thread's pane is in the layout, to hide it (`browserHeaderButton`). With
  no tab it opens the pane on a new tab, as a mod-clicked link does on a
  tab at the link. Both ask one question (`canPresentNativeBrowser`): the
  window hosts the thread's view, the home backend has an engine, the
  browser is on, and the layout is not compact. A mod-click that fails it
  opens the link the ordinary way, and a header press that fails it by the
  time it runs sends nothing. The state channel has no
  replay, so the client reads the state of each thread it shows once per
  connection of the home backend. The button reads pressed while
  this layout shows the thread's pane, and a press is chosen from that at
  the click: hide it, or show the browser. The show (`show-browser`,
  `Manager.ShowCompanion`) names no tab: in one `Manager.mu` hold at its
  turn it shows the tab the thread has selected then, or the one used last,
  and with no tab it opens one blank tab, selected and shown, which the
  answer names. So an agent's open or the user's selection before its turn
  wins, two shows open one tab between them, and the client decides nothing
  from a state it may not have read yet. Every press sends; a repeated
  press's address focus request replaces the one before it, and its refusal
  is not toasted again while the same text is on screen. A new tab the user
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
  (plain or with Shift) navigates; Escape puts the page's address back.
  Enter, Escape and taking a suggestion leave the address for the page
  (`leaveAddress` in `BrowserPane.svelte`): the page area takes DOM focus
  and the page gets the keyboard through a focus intent (see
  [Keyboard and focus](keyboard-focus.md#focus-intents)). An edit is a
  draft of the tab it was typed for, the active tab or the one a chosen tab
  suggestion is switching to, and Enter navigates only that tab. A page
  push leaves it; switching tabs replaces it with the new tab's address.
- Typing in the address opens a suggestion list
  (`BrowserAddressSuggestions.svelte`, matching in `browserAddressMatch.ts`):
  the thread's other open tabs (taking one switches to it), the dev servers
  the thread's processes serve (`stores/devServers.svelte.ts`, rows whose
  `threadId` is the thread's), then its recent addresses, each address once
  and the field's own tab never. Arrows move the highlight without changing
  the typed text, Enter or a click takes a row, Enter with none highlighted
  navigates the text, and Escape closes the list before it reverts the
  field. Nothing is fetched from the network while typing. The list is a
  `Popover`, so the native view yields while it is open; it registers as an
  airspace layer, not an overlay, since the field keeps the keyboard, so the
  pane's commands (reload, tab switching) stay live under it. Focusing the field
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
- Downloads (`internal/browser/downloads_thread.go`, the row actions in
  `downloads_actions.go`). Once the thread has a
  download the address row shows a downloads button, never moved into the
  More menu. While downloads run it shows how far they are in eighths, or a
  stepped spinner when no running download announced its size. It opens the
  thread's downloads, newest first (every running one, and the 20 that
  settled last; a download's notification finds its download by id while
  it is one of the 256 that settled last, shown or not, until the list is
  cleared): name,
  the page it began on, received and total bytes, and the
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
  an error toast with the reason otherwise. Show in folder stays on the
  toast while the list resolves the download: each list has a generation
  that Clear list and the thread's close change (`DownloadsGen`), and a
  state numbered after the announcement with another one, or from a
  restarted backend, takes the action off. An action on a download the
  list no longer has answers `browser_download_unavailable`, shown as a
  notice. `canceled` means AO stopped it:
  the user (settled by the engine's own report, so a finish that beats the
  cancel keeps its file; a repeated cancel never withdraws the first), under
  headless Chromium a quota or its size, or its page leaving by any path (a
  close, a suspend, the engine's close, Clear site data, disabling the
  browser), which settles it at once and releases any reservation, with
  the bytes the engine last reported; a download a tab's load starts
  while the tab is being created or restored is the tab's from then, and
  one whose tab never opens is canceled the same way; a
  download that starts on a page already leaving is refused to the engine
  and never listed; headless Chromium cancels a refused download at once
  and removes its file when its end arrives or its browser has stopped,
  so one that finished before its cancel leaves nothing; an end the engine reports on its own is `failed`, with
  the engine's reason when it gives one, and so is a download whose
  Downloads folder cannot be created or written. Where the file goes is
  in [Browser hardening](#browser-hardening).
  On Windows through WSL the launcher takes every download a pane page
  starts (WebView2's `DownloadStarting`, handled, so WebView2's own
  download dialog does not open for it), WebView2 saves it, and the launcher reports
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
  record of it says so, which ends the row only if it still runs, listed
  failed with that reason even when its tab closes before the answer is
  applied. WebView2
  resumes a dropped download itself, as a new download start that carries
  the interruption's reason; the launcher keeps it as the same row, saving
  to the file the download started with, so one click is one row, one file
  and one end. A resume names nothing else of its download, so the launcher
  adopts it only when exactly one running download from that address
  reads as dropped (in progress with an interrupt reason; a healthy one
  that never resumed reads none). Otherwise, with two dropped at once or
  one dropped beside a download that already resumed, the resume is a row
  of its own saving where WebView2 chose, and the dropped download's row
  stays running until the user cancels it. A cancel WebView2 takes always
  settles: by WebView2's report, within milliseconds when it comes, or by
  the launcher after five seconds, since a dropped download's operation
  never ends; the launcher reads the download's state first, so one that
  ended unreported ends as it did. In WebView2 that dropped download and
  its resume are one, so the cancel ends the resume's row too if it still
  runs. A download still running at the five seconds is no longer followed
  and is listed failed with that reason; if it really finishes later, its
  file stays in the Downloads folder and no row can open it. A
  cancel WebView2 refuses is answered with the refusal: the row stays
  running with the reason under it, the cancel is withdrawn so its later
  end reads as its own, and the user can cancel again. Every later report
  of the download carries the refusal, so one lost answer does not leave
  the cancel standing, and the status question carries the cancel, so a
  cancel lost on the way to the launcher is made then. A second download a
  script starts with no user gesture gets WebView2's own "download
  multiple files" prompt in
  the page and has no row until it is allowed. A popup's downloads are
  taken the same way. One it starts before the Manager lists its tab is
  listed when the popup's fate is known: in the popup's tab when it is
  adopted, and otherwise in the tab of its nearest opener still listed, as
  it is, finished or running, or canceled with the reason when the popup's
  close ended it (WebView2 cancels a closing page's downloads). A popup
  that closed or was refused while that opener was listed lists them there
  even when the opener closes before the popup's downloads are settled,
  however far the backend has processed the close. A popup
  whose window went before the launcher reported it is never a tab; its
  downloads are listed at its close. Under headless Chromium such a
  download holds the quota of the workspace of the nearest page that opened
  it from its start, and one over it is refused then. With no opener listed
  at its fate it is canceled, and the file AO keeps for it (headless
  Chromium's artifact, WebKit's staged file) is removed. Past 16 such downloads of a popup, or 64
  popups holding some, a download is refused and canceled. When a popup's first navigation is
  a download, WebView2 asks to close the popup, which closes when the
  download ends, and opens its own download dialog over the opener, which
  nothing a host sets prevents; the launcher closes it as it opens, so it
  shows for about 0.2 s and nothing on it can be used.
- Chords, live while the browser pane is the keyboard's pane
  (`paneCommandTarget`: the focused or promoted pane, under compact the one
  on screen; `browserPaneFocused`): new tab `mod+t`, focus the address `mod+l`, reload
  `mod+r` and `f5`, next and previous tab `ctrl+tab` and `ctrl+shift+tab`,
  reopen closed tab `mod+shift+t`, beside close tab `mod+w`. A bare `f5` is never claimed from the page.
  Next and previous tab send a relative `step` that the Manager applies to
  its own order and selection (`StepCompanionPage`), so presses faster than
  the state push each move one tab. Reload names the tab the window shows
  (or the page the chord was pressed in) as close does, and reloads
  nothing when that tab closes first. The tab menu's and the address
  bar's reload name their tab too. Before the pane's attach answers, the
  commands that send are not available (`browserPaneUnattached`): the
  palette does not offer them, and their chords stay the pane's and do
  nothing. The palette offers reload and reopen by the state the window
  shows (a shown tab that takes an address, a closed tab), but a command
  the user issues is decided for the tab it names, never by a state that
  the pane's commands still on their way will change: reload runs when
  its tab takes an address, and reopen is sent, numbered, for the Manager
  to answer at its turn (`browserCommands.svelte.ts` AVAILABLE_WHEN).
- The app reload chord, `mod+r`, is handed to the pane while it is
  focused and a command runs for it there
  ([keyboard and focus](keyboard-focus.md#the-app-reload-chord)).

## Keyboard

How AO's chords reach the app from inside a native page, which page holds
keyboard focus, focus intents and the page's Escape are in
[Browser pane keyboard and focus](keyboard-focus.md).

## Find

mod+f in the browser pane finds in the page the pane shows, through the
engine's own find-in-page; hosted WebView2 opens the engine's own find bar,
and headless Chromium has no find. The Manager's search sessions, their
owners and ends, and each engine's find are in
[Pane find](pane-find.md#browser-pane-find).

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
directory of finished downloads, apart from where the engine writes running
ones; on every other engine the file of each finished
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

On a desktop engine a finished download ends up where any browser on the
machine that runs the page's engine puts it: that user's Downloads folder (`downloadSaving` in
`internal/browser/driver_events.go`). There is no setting.

- Launcher-hosted WebView2: WebView2's default, the Windows user's
  Downloads folder, under the name WebView2 gives (`name (1).ext` on a
  collision). WebView2 keeps the mark of the web. The finished file is
  openable from AO only while the path WebView2 reported is a regular
  file, not a link, of the size it reported; otherwise the row completes
  with the reason and opens nothing.
- WebKitGTK and WKWebView write each download only into the app's
  private staging folder in the Downloads folder,
  `.agent-overflow-downloads` (mode 0700), in a run folder of the
  process's own, `run-<process token>`, under a name the Manager gives as
  the engine asks for the destination, `dl-<n>`, so a file another run
  left never collides (`internal/browser/download_staging.go`). The
  process opens its run folder once and holds it, with an exclusive lock
  on its `.lock` file, until its last staged download ends: every removal
  it makes there goes through that handle, so a run folder moved aside or
  replaced never loses a file of the replacement, and a publish takes the
  staged file only while its path still names the file in the held
  folder. The folder is resolved per download, created on demand with a
  missing Downloads folder, checked to be a real folder and not a link
  before every use, and the run folder and the staging folder are removed
  once no download stages in them and they are empty. The Manager publishes a finished
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
  their staged file and the emptied staging folder. The staged files of
  a run that ended mid-download are removed when a later run first uses
  the staging folder, and only on positive evidence that the run ended:
  the later run takes that run folder's lock. A live run's folder, a run
  folder with no lock file, a file without a staged name, and the files
  releases that staged without run folders left directly in the staging
  folder (no evidence tells a dead one from a live one) are never
  removed. Nothing that deletes a thread, clears site data, turns the
  browser off or prunes artifacts deletes a file in the Downloads
  folder.
- Headless Chromium in serve mode alone keeps downloads as AO artifacts.
  Each browser process writes into a directory of its own inside the
  jar's `downloading` directory (`headless_download_room.go`), removed
  with everything left in it once the process exits. A finished download
  is placed beside it under its id, never replacing a file, then moved
  into the workspace's `finished`
  directory under a sanitized unique name. Downloads are capped at
  512 MiB each and 2 GiB reserved per live workspace, whichever jars its
  pages are in. Downloads and asset bundles share a 4 GiB artifact quota
  per backend process. Files older than seven days are pruned at the
  process's first artifact use, while newer files are never silently
  evicted to make quota room. The process holds the artifact root open
  from its first use, and the quota's measurement and every write AO
  makes there go through it (`artifacts.go`), so a later rename or link
  of its name moves neither. A path AO hands out there, an asset
  bundle's or the download room Chromium is told, must still reach what
  AO holds (`nameReaches`), or the bundle or the launch is refused.
  Under the artifact root, `browser_open_file`
  opens only a file AO moved into a workspace's `finished` directory
  (`authorizeFile`); any other file there never opens and goes with the
  seven-day prune.

A download that begins in a page no thread owns is canceled.

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
clipboard per tab instead of modifying the OS clipboard, saves a desktop
engine's downloads to the user's Downloads folder, and writes asset bundles
and headless Chromium's downloads to bounded AO artifact directories. Raw CDP, arbitrary profiles/browser
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
operation is expressed as are shared with the Linux engine (both are WebKit),
so the differences are only where the platform itself differs:

- **Site data is WebKit's directory, not AO's.** macOS exposes no documented
  way to place a `WKWebsiteDataStore` in a chosen path. On macOS 14+ each
  persistent jar gets its own store keyed by a digest of the jar (a worktree
  jar's key is the workspace path); on macOS 13 there is no
  per-jar persistent store at all, so the site-data setting has no effect
  there and every jar is in-memory-only.
- **Clearing site data is WebKit's removal, not a directory delete.** Because
  WebKit owns where those stores live, Clear site data asks it to remove every
  data store AO created, all of them and only them: the app's own SPA webview
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
  a requested navigation continues, the same outcome the other engines reach
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

A window a page opens (`window.open` with or without a user gesture, a
`target=_blank` link or form, a middle or modified click) becomes a tab on
every engine: the Manager adopts it into the opener's thread, workspace
authority and jar, within the thread, workspace and process caps, or closes
it. Window features change nothing; `noopener`, and a link or form, leave
the popup no `window.opener`. A page whose `window.close()` the engine
honors (on WebView2, a popup or a page with one history entry) closes its
tab. A popup that closes before its tab is listed never becomes one, even
if its page also crashed; one whose page only crashed is listed as a
crashed tab, never shown live first, also when AO could not attach to its
page. On
WebView2 the launcher answers the page's `NewWindowRequested` with a hidden
controller of its own in the opener's profile, starting at the opener's
bounds, so no window of WebView2's own opens and `window.opener` and
`postMessage` work as in a browser; the opener's script waits in
`window.open` until the controller is up. A request the launcher cannot take
(four already being created, its 64 pages, or its opener closing), or
whose controller is not up within 10 seconds, opens nothing, and
`window.open` answers null. The popup reaches the backend as a
report naming its opener and CDP target (`internal/webview2host/popup.go`,
`internal/browser/hosted_popups.go`), after its opener's, and its requests
before its adoption pass the browser-wide policy with its opener chain's
authority, the opener its report names standing in where its target names
none (a link a click opened in a new tab). Headless Chromium runs with its
popup blocker off, so a window opened with no gesture opens there too. On every engine a popup opened by a popup still being adopted
waits for that adoption, up to 10 seconds, and is closed with it when that
popup is not adopted.

**Serve mode drives a headless Chromium** (`docs/specs/remote-access.md` §7,
and the operator's copy in [serve-mode.md](serve-mode.md) § Browser tools). A
backend with no window cannot host a view, so it launches its own browser
instead: one process per jar profile, started by that profile's first
page and stopped with it, sandbox on. The tool surface is again identical, for
the same reason the WSL one is: the operations are the same CDP calls. What
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

Every OTHER windowless run (`--connect`, `go test`, and any desktop whose OS
is too old for its native engine) has **no browser engine and no browser
tools**, as does a serve host with no Chromium on it. There is no fallback
browser to launch, and a browser tool call on such a deployment answers one
sentence saying browser tools are not available there. The hello frame carries
the same answer as `browser` in its capability list, so a client can tell "no
browser on that machine" from "turned off in Settings". The mocked
boot modes (`--harness`, `--soak`) are the one exception: they pin a fake engine
so the companion pane's chrome, tab strip, and host rect render with nothing
behind them. That pin is default-on and lifted by one manual gate:
`AO_HARNESS_REAL_BROWSER=1` on an ATTENDED isolated boot (never a soak
autopilot), which turns `make harness-window` / `make harness-wsl` into
the real-engine rig described in `docs/specs/embedded-browser.md` §10.

The companion RPCs and URL/title state events are loopback-only because they
expose local files and authenticated browser content, and the pane mount drives
real native views in the desktop window. A LAN `--connect` client does not
receive or control this surface. Unsupported OS/arch combinations fail the tool
call without affecting provider or app lifecycle.
