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
- `browserPersistSiteData` (default `true`) decides whether a canonical
  workspace's site data lives in a real browser profile on disk or only in
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
- One isolated profile per canonical workspace with at least one active browser
  page. Threads on that workspace intentionally share login state; pages remain
  owned by the registering thread.
- A thread gets an opaque MCP URL capability at provider-session start, and
  session end revokes it. Its registration does not create a profile or page.
- The first page operation creates the workspace profile and a page. Page
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
  mean the current viewport. Hidden pages keep rendering, so screenshots and
  scrolls work with the pane closed, and a screenshot that produces no frame
  fails with a bounded error instead of hanging.
- `pane.close` (Mod+W) on a focused browser companion closes the active tab;
  the companion closes when its last tab does. Closing the companion any other
  way hides the session and keeps its pages, so reopening shows the same tabs.
- A thread's pages outlive a stop of its provider session, so a restarted
  session finds the same tabs. Deleting or archiving the thread closes them and
  forgets their saved copies; an archive that keeps a re-engaged session keeps
  its pages. A workspace profile with no pages is disposed. An engine with no
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
- Suspended pages are saved as one JSON file per thread under
  `browser-pages/` in the config directory, written atomically
  (`internal/browser/page_records.go`). At boot, a file that is unreadable,
  invalid or belongs to a deleted or archived thread is removed with a log
  line, so a delete or archive that cannot remove a thread's file logs it and
  still completes. Clearing site data and turning the browser off remove
  every saved page.
- App shutdown saves the open pages, cancels calls, disposes profiles, stops
  the engine, then closes the MCP listener. Wails runs that shutdown ON the
  desktop UI thread, which the WebKit engines dispatch every native call to,
  so when the caller is that thread the profiles are disposed inline on it
  (`engineUIThread`) rather than fanned out to goroutines that could only wait
  for it. Saving uses the pages' cached address and title and makes no engine
  call.

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
- The chord then reaches the frontend as an `accelerator` event on
  `browser:companion-state`, carrying the BOUND spelling (Shift glyph
  variants are normalized by the match). The store focuses the thread's
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
  three times in all, until it or a later stop through it is answered. A
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
| `browser_session` | Name the thread-owned automation session. |
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
| `browser_downloads` | List or wait for browser downloads and return their AO-owned local paths. |
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
workspace or project root. Outside files require
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
The manager uses a per-workspace profile, grants no camera, microphone,
geolocation, notification, or system-clipboard permission, bounds navigation
and tool deadlines, caps pages and contexts, dismisses JavaScript dialogs,
blocks dangerous navigation schemes, and never exposes a debugging
endpoint outside the local controller process. Downloads and asset bundles go
only to AO-owned directories with file/count/byte caps; download filenames are
sanitized across macOS, Linux, and Windows and never target the user's normal
Downloads directory. A download that begins outside every AO page is
cancelled. Downloads are capped at 512 MiB each and 2 GiB reserved
per live workspace; downloads and asset bundles share a 4 GiB per-process
artifact quota. Files older than seven days are pruned on the next artifact
use, while newer files are never silently evicted to make quota room.

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
  workspace gets its own persistent store keyed by a digest of the workspace
  root; on macOS 13 there is no per-workspace persistent store at all, so
  the site-data setting has no effect there and every workspace is
  in-memory-only.
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
instead: one process per workspace profile, started by that profile's first
page and stopped with it, sandbox on. The tool surface is again identical, for
the same reason the WSL one is — the operations are the same CDP calls. What
is absent is the PANE, which is window chrome: an agent on a serve host gets
the tools, and nobody gets a view of them. Selection is an explicit request
that only the serve boot makes, never an inference from the absence of a
window, or `go test` would start browsers. And nothing is ever downloaded: the
Chromium has to already be installed, or the host keeps the windowless answer.

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
