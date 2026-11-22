# Pane find

How find works in a pane: the bar, the store and its providers, regions and
nested surfaces, and the browser pane's find in a native page. The rules
for adding find to a pane kind are in the
[panes guide](../../frontend/src/lib/components/panes/AGENTS.md#find).

## Bars and providers

Every pane kind with find mounts `PaneFindBar.svelte` directly under its
header, in the flow (a terminal floats it instead, `floating`, so opening it
never resizes the PTY), keyed by its layout pane id (a companion's own id,
not its source's), and registers a `PaneFindProvider` for that id while its
body is mounted (`stores/paneFind.svelte.ts`). The store owns open, query,
focus return and which bar holds keyboard focus (`isPaneFindBarFocused`);
the provider owns matching, stepping and clearing. Escape belongs to the
`find.close` command, never the bar. The bar returns focus to what had it
when the bar opened, else to its `fallbackFocus`, checks that focus landed
and reports a fallback that refused it. Matches are painted with
`utils/findHighlights.ts` under `findHighlightName(paneId)`, and every find,
filter and highlighter matches by `utils/findMatch.ts`, terminal find
included.

Find acts on the keyboard's pane (`stores/panes.svelte#paneCommandTarget`),
and on no overlay above it; the store drops a pane's state and reads when
the pane leaves the layout.

- Thread timelines use `chat/ThreadPaneFind.svelte` over
  `stores/threadFind.svelte.ts`; a plain scroll container uses
  `scrollTextFind.svelte.ts`.
- A provider with asynchronous reads runs them through
  `paneFindReads(paneId)`: one in flight and at most one waiting per layout
  pane, across body remounts. Such a provider applies an answer only for
  the query and owner it asked, updates in place on a content change
  without moving the view, and withdraws any jump it requested when its
  query changes or it closes.
- Review panes use `review/ReviewPaneFind.svelte` over
  `stores/reviewFind.svelte.ts`, keyed by `ReviewPane`'s `findKey`: its
  layout pane id as a companion, the detail region key on the Pull requests
  page; a review hosted without a key registers nothing.
- The browser pane uses `stores/browserFind.svelte.ts`, where the engine
  searches and matches are the engine's to paint; its calls run on its own
  queue, not `paneFindReads`, because a queued stop is backend cleanup that
  no other read may replace.
- A provider that cannot count reports `count: null`, and one with
  `openNative` opens its engine's own find UI instead of the bar, which
  never reads as open, so its Escape stays the engine's.

## Regions and nested surfaces

A pane whose body holds a list beside a detail declares find regions
(`declarePaneFindRegions`) and keys each region's provider, bar, reads and
highlights by its region key (`paneFindListKey`, `paneFindDetailKey`)
instead of the layout pane id. Find acts on one region at a time
(`paneFindTarget`): the one on screen when only one is (compact, a stacked
level), else the one last focused or pressed (`paneFindRegion.ts` on the
region's element), else the declared initial one. Each region's state is
its own, so both bars may be open; a region with no provider has no find and
mod+f falls through. Everything the store does per pane (open, close, focus
return, `paneFindAvailable`, drop on layout removal) holds per region.

A list region's provider is a filter: it has no `next` or `previous`, the
bar shows the matching row count and Enter calls `focusFirst`, and its query
narrows the rows in the page's store so every count follows one source. The
Pull requests page (`pulls/pullsPaneFind.svelte.ts`) and Workflows home
(`workflows/workflowsPaneFind.svelte.ts`) filter this way.

The thread terminal, in a drawer or a terminal pane, is a nested find
surface (`declarePaneFindSurface`, attached with `paneFindSurface`): each
tab has its own provider, bar and state, and the pane's find commands act on
the active tab's while the pane's last focus or press was inside the
terminal, or while the pane has no provider of its own. A tab's state goes
when its terminal ends, on screen or not: the terminal store's `removeTab`
drops its region under every pane (`dropPaneFindRegion`). Terminal matching
is in the [terminal guide](../../frontend/src/lib/components/terminal/AGENTS.md#find).

## Browser pane find

mod+f in the browser pane finds in the page the pane shows, through the
engine's own find-in-page. The pane registers a find provider only while the
shown page's engine has find (`findMode` on the companion state), so the
chord falls through elsewhere. A chord pressed inside the page arrives through
the accelerator path ([keyboard and focus](keyboard-focus.md#replay-in-the-app)). Whether AO's bar is open is the pane-find
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
  ([keyboard and focus](keyboard-focus.md#escape)); while a pane shows the page, the Manager announces it as
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
  orders, and record each provider's view of it, checking
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
