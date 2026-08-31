# internal/browser/

Built-in browser MCP over one engine behind the `driver.go` seam:
launcher-hosted WebView2 controllers on the Windows/WSL deployment, WebKit
views embedded in the app's own window on the native Linux (WebKitGTK) and
macOS (WKWebView) desktops, and a fake engine for the mocked boot modes.

**A deployment without a window has NO engine and no browser tools.** The
engines live in the desktop app instance, so a remote `--connect` backend, a
headless serve mode, and `go test` get `unavailableEngine`, whose one refusal
sentence is the entire windowless story. The App declines to construct the MCP
server at all in that case, so the model reads an absence rather than 28 tools
that could only fail.

## Ownership and isolation

- `MCPServer` owns the loopback Streamable HTTP endpoint. Every provider
  thread receives an unguessable capability URL; unregistering the thread
  revokes it and closes only that thread's pages.
- An ENGINE is reached only through the seam in `driver.go`: `browserEngine`
  (the process and its profile factory), `engineProfile` (one workspace's
  isolated site data), and `pageDriver` (every per-page tool operation).
  `hosted_engine.go` is the launcher-hosted implementation of those three (its
  per-page half is `cdp_page.go` — CDP is CDP), `webkit_*.go` the WebKitGTK one,
  `wkwebview_*.go` the WKWebView one, `fake_engine.go` the mocked-boot one, and
  `engine_unavailable.go` the null object every other deployment gets (spec
  `docs/specs/embedded-browser.md` §6). An engine implements the seam and
  nothing else.
- WHICH engine is a WIRING answer, never a `runtime.GOOS` check. `selectEngine`
  is a three-fact table: `ManagerOptions.FakeEngine` (the harness and soak
  pins, spec §10) wins first; then `ManagerOptions.PaneHost`, non-nil exactly
  when the executable built a CDP relay (WSL only), which selects the hosted
  engine; then the native one, only when `ManagerOptions.NativeWindow` answers
  a real window AND the platform half can actually host it. Anything else is
  `unavailableEngine`. `Manager.Available()` is how the App asks, and it is
  what gates the MCP server.
- `unavailableEngine` is a VALUE, not a nil `browserEngine`. Every Manager path
  — start, teardown, pane presentation, devtools — would otherwise need its own
  nil check, and one missed check is a nil deref inside a tool call. Here the
  refusal is one sentence in one place.
- An engine difference the tools can feel is ANSWERED, never assumed away:
  `pageDriver.ReadOnlyCaveat` is the pattern. Neither WebKit engine has an
  equivalent of CDP's `throwOnSideEffect`, so both return the SAME sentence,
  which the Manager appends to the tool result as a second content entry — the
  JSON payload stays byte-identical on every engine, and a caller reading two
  engines' answers never has to notice which gave it.
- `Manager` owns POLICY and never engine mechanics: `Access` checks, the page
  registry and its per-thread ownership, labels, session/visibility state, every
  cap and bound, artifact quotas, the AO-managed per-tab clipboard, and the MCP
  server. A canonical workspace gets one isolated profile, while every page is
  tagged with its provider thread owner. A thread can never address another
  thread's page. Policy must not migrate into a driver, and engine specifics
  must not stay in `Manager` — an engine reports facts through `pageHooks` /
  `engineEvents` and the Manager alone decides what they mean.
- The engine is not started by app startup or MCP registration. The first tool
  that needs a page starts it. It stops two minutes after the final workspace
  profile becomes idle.
- Pages are created hidden and stay hidden. The user-visible surface is the
  calling thread's companion pane, presenting the exact same page the MCP tools
  drive; do not reintroduce a separate browser window or a second session.
- **No engine is ever downloaded.** AO used to install a Chrome-for-Testing
  build on first use; that whole path (`internal/chromium` and the
  `browser:install-progress` channel) is deleted. This package makes no network
  request of its own — every byte it fetches is a page a tool navigated to.
  History worth keeping: managed Chrome needed a blank-flip recovery, because a
  tab whose renderer died came back as a live-but-blank target that every
  subsequent tool call happily addressed. The engines here report page death
  through `engineEvents.PageClosed` instead, so a dead page LEAVES the registry
  rather than lingering as a plausible one. Any future engine must report the
  same way; do not re-add a recovery pass that guesses from page content.

## The hosted engine (Windows/WSL)

`hosted_engine.go` is the launcher-hosted implementation of the same three
interfaces. Page OPERATIONS are unchanged — CDP is CDP, so `cdp_page.go` drives a WebView2
controller exactly as it drives a Chrome tab. Only LIFETIME differs.

- **Selection is a wiring fact, not a platform test.** `ManagerOptions.PaneHost`
  is non-nil exactly when the executable built a CDP relay, which happens only
  under WSL (`internal/app/bootstrap.go`, `SetBrowserCDPRelay`). "Which engine"
  and "is there a launcher to host windows" therefore cannot disagree. Do not
  add a `runtime.GOOS` branch in this package.
- **A page id is the handle.** `hostedPage.Handle()` answers the backend's own
  page id, never the CDP target id: the id is what directives address, what the
  launcher's reports name, and what `Manager.removeClosedPage` and `engineEvents`
  key on. The engine keeps a private bidirectional target↔page map and re-keys
  browser-level CDP events before reporting them, so a target this engine never
  created is dropped rather than reported under a handle nobody owns.
- **Every wait is bounded.** `NewPage` emits a `create` directive and waits for
  the launcher's `created` report (`hostCreateTimeout`), then attaches chromedp
  through the relay (`hostAttachTimeout`, which also bounds chromedp's own
  unbounded first attach). A launcher that never answers is an error, never a
  hang; every failure path after the directive emits a `close`, because the
  controller may already be real.
- **`Start` is a deliberate no-op.** The launcher builds its WebView2
  environment on the first directive and its tunnel dials back only after that,
  so the CDP connection cannot exist until `NewPage` has emitted a `create`.
  Blocking in `Start` would wait for a tunnel that `Start` is what unblocks.
- **Show/hide/bounds/devtools are engine mechanism.** The Manager's visibility
  path calls them through the `paneHost` interface (`syncPanePresentation`);
  the Manager still decides WHICH page is presented. Visibility is deduped in
  the engine because it is recomputed on every selection, focus and page-list
  change.
- **One carve-out, deliberate.** `AttachPage` fails: the launcher does not
  surface WebView2's `NewWindowRequested`, so no popup is ever reported and a
  driver for a controller nobody created would be worse than a loud failure.
- **Unverified CDP support stays on the CDP path.** `Browser.cancelDownload`,
  `Browser.setDownloadBehavior` and `Browser.setPermission` are not confirmed on
  WebView2. The existing code path is kept rather than guessed at; if one turns
  out unsupported, the refusal is bounded by the Manager's own caps.

## The WebKitGTK engine (native Linux desktop)

`webkit_cgo_linux.go` is the ONLY cgo in the engine; `webkitglue_linux.c` holds
what needs real C function pointers (GTK/WebKit signal handlers, the two async
completion callbacks, the window surgery). Everything else is ordinary Go.

- EVERY GTK/WebKit call goes through `gtkDo` (Wails' main-thread dispatch,
  bounded). Calling GTK from a goroutine is undefined behaviour, not a race
  that shows up under load.
- NEVER call into GTK while holding a lock a C callback can take. `gtkDo`
  waits for the GTK thread, and that thread may be running a delegate that
  wants the lock. Locks here cover map bookkeeping only.
- No Go pointer is ever handed to C. A callback addresses a page, profile,
  call, or download by uint64 id and the Go side resolves it — which also
  makes a callback arriving after teardown a lookup miss, not a crash.
- Hidden pages are MAPPED, parked in a 1x1 clipping `GtkScrolledWindow` at
  their own slot. That keeps a real viewport and fresh snapshots at no window
  cost. A `GtkFixed` at offscreen coordinates balloons the window, and
  `opacity:0` kills rAF: both are banned (spike-verified).
- The pane rect is four `GtkOverlay` margins with `ALIGN_FILL`, never a size
  request: `gtk_widget_set_size_request` cannot SHRINK a WebKitWebView, whose
  natural size sticks at its largest-ever allocation.
- Every page operation is one JS function body built in `webkitjs.go` — which
  is deliberately tag-free and cgo-free so the builders are compiled and unit
  tested on every platform. Same for the screenshot arithmetic in
  `webkitimage.go`. A selector or user string crosses as a JSON literal, never
  as spliced source, and an unframed call passes `[]` (not `null`, which is
  not iterable).
- Node handles do not exist: CDP addresses a node by remote object id, WebKit
  re-resolves the frame chain and selector per operation. A selector that no
  longer matches exactly one element is the stale-locator error, which is the
  same answer the CDP driver gives.
- Engine-visible differences to preserve rather than paper over: input is the
  untrusted JS tier (`element.click()`, focus + value + events), the viewport
  IS the widget size (no device-metrics override), assets are read through the
  page and capped well below the Manager's bundle cap because they cross as
  base64, and the streamed companion pane cannot run here at all — it speaks
  CDP directly and is replaced by the presented native view (spec §7/§9).

## The WKWebView engine (macOS)

`wkwebview_cgo_darwin.go` is the ONLY cgo in the engine; `wkwebviewglue_darwin.m`
holds what needs real Objective-C (the WKWebView delegates, the two async
completion blocks, the AppKit panels, the view surgery). Everything else is
ordinary Go, and every page operation is the SAME `webkitjs.go` /
`pagejs.go` builder the WebKitGTK driver uses — both engines are WebKit, so the
difference is the call that carries the body, never the body. Do not fork a
builder for macOS.

- EVERY WebKit/AppKit call goes through `wkDo` (Wails' main-thread dispatch,
  bounded). Same rule, same reasons, same lock discipline as `gtkDo`: a lock
  here covers map bookkeeping only, and no wkDo-backed call happens under one.
- A `//export` callback runs ON the main thread, so it must never call `wkDo`
  itself — the dispatch would queue behind the delegate it is running inside
  and freeze the UI for a full `wkCallTimeout`. And it must never release an
  object the delegate is about to hand back to WebKit: the popup path returns
  the very view a failed lookup would like to destroy, so that close goes to a
  goroutine and lands on a LATER main-thread turn. Both traps caught the
  WebKitGTK engine too and are fixed there in the same shape.
- Hidden pages are IN THE WINDOW, parked in a 1x1 layer-masked `NSView` at
  their own slot, added BELOW the SPA webview. An unparented WKWebView is the
  trap: WebKit only guarantees layout and snapshots for a view inside a window.
  This is the direct analogue of the Linux 1x1 clipping `GtkScrolledWindow`.
- The pane rect arrives as a `PaneRect` in SPA CSS pixels and is scaled by the
  content view's bounds over the rect's viewport (same proportional rule as
  every host), then flipped against the content view's own `isFlipped`, never
  assumed. AppKit needs no reparenting surgery: a second subview joins Wails'
  content view directly. `wkwebview_pane_darwin.go` is the `paneHost` half,
  mirroring `webkit_pane_linux.go` — and deliberately NOT `paneDevTools`:
  WKWebView has no public call that opens its inspector, so the pane's
  devtools button gets the Manager's explained refusal and the real
  inspector is Safari's Develop menu against the inspectable view.
- Two APIs decide what this engine can be. `-callAsyncJavaScript:…` (macOS 11)
  is the one call every operation goes through, so `ao_wkv_supported()`
  answering no leaves that Mac with NO engine and no browser tools — a
  capability answer exactly like "is there a window". There is no fallback
  browser to quietly launch instead. `+dataStoreForIdentifier:` (macOS 14) is the only
  documented per-workspace persistent site data, and it lives in WebKit's own
  directory: there is NO macOS counterpart to the AO-owned `browser-profiles/`
  tree (spec §4), and on macOS 11–13 the site-data setting has no effect at all.
  Do not invent a directory to make the platforms look alike.
- A full-page or clipped screenshot RESIZES the view, captures, and restores:
  `WKSnapshotConfiguration` cannot reach past the view's bounds, unlike
  WebKitGTK's `FULL_DOCUMENT` region. Frames are normalized to one image pixel
  per CSS pixel (premultiplied BGRA, so `webkitimage.go` decodes both engines),
  because a backing-scaled image would crop clip rects at the wrong place.
- WKWebView ships NO dialogs and NO open panel of its own: an unimplemented
  delegate method makes the JavaScript call return immediately, so both answers
  are spelled out — dismissed/refused on a hidden page, a real NSAlert or
  NSOpenPanel on the presented one. `beforeunload` has no public delegate and
  proceeds, which is the outcome the other engines reach by accepting it.
  Context menus need no suppression: a clipped hidden view takes no input.
- Downloads are `WKDownload` (macOS 11.3), forced into the profile's artifact
  directory by handing `decideDestinationUsingResponse:` a handle-named path —
  a nil destination is how one is refused. WKDownload has no per-chunk
  callback, so the profile SAMPLES its `NSProgress`; without that the Manager
  could only enforce its per-download byte cap after the whole file was written.
- No Go pointer is ever handed to Objective-C. Ids resolve Go-side, page and
  profile identity live on the view as associated objects, and the console
  handler reads `WKScriptMessage.webView` rather than baking an id into the
  handler — which is what keeps a popup sharing its opener's user content
  controller from reporting under the opener's identity.

## Local files and website capabilities

- `browser_open_file` resolves symlinks and requires a regular file. Default
  access is limited to the thread workspace or project root; the explicit
  outside-workspace setting widens that to files readable by the app process.
- Use direct `file://` navigation. Do not add a local content server merely to
  render HTML, PDF, images, or text.
- Downloads land only in AO-owned artifact directories, use sanitized unique
  names, are capped at 512 MiB each / 2 GiB reserved per live workspace, share
  a 4 GiB per-process artifact quota with asset bundles, and
  are canceled with their page. They never write to the user's Downloads
  folder. Sensitive site permissions remain denied. JavaScript dialogs are
  dismissed; beforeunload is accepted so requested navigation can continue.
  Popups inherit the opener thread and its page limit.

## Resource bounds

- One engine, at most 12 live workspace profiles, 8 pages per thread.
- Pages start hidden. PRESENT only the explicitly selected page of a thread
  with a mounted, visible companion; ordinary agent activity must never steal
  that selection. Keep the viewport cap (1920×1200).
- **Pixels never cross the wire.** The pane's page content is a real native
  view the engine positions over the pane's host rect; a `CompanionEvent`
  carries tab state and nothing else, so a hidden pane costs no encoding and no
  wire traffic at all. Do not reintroduce a frame stream in any form — the
  screencast, its ack worker, its keepalive, and its four RPCs are deleted
  (spec §7/§9), and the engines that replaced managed Chrome cannot speak CDP
  screencast anyway.
- Operations are serialized per page and time-bounded. Snapshot text/elements,
  locator matches, console/clipboard data, downloads/assets, evaluation output,
  MCP request bodies, and screenshot dimensions are capped. Preserve or
  tighten these bounds when adding tools.
- Do not start a fresh engine per tool call or leave a profile alive after its
  final page closes.

## Parallel-agent page selection

- An open/open-file call without `page_id` creates a new background page.
  Supplying `page_id` is the intentional existing-page path.
- Other page-scoped tools may resolve an omitted `page_id` only with zero/one
  owned page. Multiple pages are an error that directs the caller to
  `browser_pages`; never restore MRU guessing.
- Generic MCP has no portable provider-subagent identity. Keep coordination on
  explicit page handles and short thread-unique labels rather than inventing a
  caller identity or a second session token every model must carry.

## Persisted site data

- **Site data is the ENGINE's, on disk, per workspace** (spec §4). Nothing here
  reads or writes cookies or localStorage: the hosted engine keeps a named
  `CoreWebView2Profile`, WebKitGTK a `WebKitNetworkSession` under
  `<data root>/browser-profiles/<workspace-hash>/`, and WKWebView a
  `+dataStoreForIdentifier:` store inside WebKit's own directory.
- `browserPersistSiteData=false` is an EPHEMERAL session, not a suppressed
  write. It reaches an engine as `profileOptions.Persist`.
- Clear site data closes the engine's pages for the workspace, then deletes the
  profile directory. There is nothing else to clear.
- The AES-GCM checkpoint store (`state.go`, its keyring key, the harness
  force-file-key path) is DELETED. Its leftovers — `browser-state/` and
  `browser-state.key` — are pruned on the first boot of this code, because they
  were encrypted with a secret nothing reads any more. Do not reintroduce a
  second, staler copy of state the engine already holds.

## Tests

- **No test starts a browser.** `fake_engine.go` is production code — the
  harness and soak boots render the pane's chrome and host rect on it (spec
  §10) — and the same engine carries this package's policy tests
  (`manager_test.go`): pages exist, navigate, are owned by one thread, and
  close, with nothing rendering them. Anything needing real page content
  (snapshot, screenshot, evaluation, locators, input) refuses BY NAME, so a
  test that thinks it is driving a renderer fails loudly instead of asserting
  against invented content. Do not build a second fake.
- Windowless selection must keep choosing NO engine, and that rule is tag-free
  (`manager_test.go`) because its only failure mode is a silently launched
  browser. It is also what keeps `make go-test` display-free.
- Both WebKit engines' testable half is everything pure: the JS builders, the
  screenshot pixel path, the profile identifier, and the platform half of
  engine selection. Their live half needs a real GTK or AppKit
  window and is proven by running the desktop app, not by the suite. A rule
  whose only failure mode is silent belongs in the tag-free half: a malformed
  `wkStoreIdentifier` costs a workspace its isolation with no error anywhere,
  which is why it does not live in the darwin file that produced it.
- Trusted keyboard tests must assert the resulting DOM state, not merely a
  successful CDP call. Encode modifier chords as modifiers/editing commands;
  concatenating modifier key runes presses and releases them before the key.

## References

- `docs/architecture/browser-tools.md`: shipped product and authority contract.
- `docs/references/codex-browser-parity.md`: bundled Codex browser API map and validation ownership.
- `docs/architecture/in-app-browser-spike.md`: measured engine and ownership decision evidence.
- `docs/specs/embedded-browser.md`: the embedded-pane feature spec.
- `internal/webview2host/AGENTS.md`: the launcher half the hosted engine's
  directives and reports cross to.
- `internal/cdprelay/AGENTS.md`: the tunnel the hosted engine attaches chromedp
  through.
