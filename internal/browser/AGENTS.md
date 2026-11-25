# internal/browser

Built-in browser MCP server and companion-pane manager. `Manager` is the
policy layer; `driver.go` is the seam to the engines: launcher-hosted WebView2
on Windows/WSL, WebKitGTK on native Linux, WKWebView on macOS, and explicitly
requested headless Chromium in serve mode. Product behavior and its mechanism
are in [browser-tools.md](../../docs/architecture/browser-tools.md); the
engines per platform in
[embedded-browser.md](../../docs/specs/embedded-browser.md). The contracts
of the functions and types named below are their doc comments.

## Where to look

| Change | Start here |
|---|---|
| Thread lifetimes, binding an action, thread close | `thread_life.go`, `thread_close.go` |
| Tab lifecycle, creation, listing, teardown | `lifecycle.go`, `lifecycle_create.go`, `creation_end.go`, `engine_pages.go` |
| Suspend, restore, saved pages, crashes | `suspend.go`, `restore.go`, `page_records.go`, `crash.go` |
| Cookie jars and their switch | `jar.go`, `jar_switch.go` |
| Pane command order, tabs, closed tabs | `selection.go` over `turnorder/`, `companion_tabs.go`, `thread_tabs.go` |
| Presentation, viewport, base color, focus | `presentation.go`, `viewport.go`, `page_base.go`, `focus_intent.go`, `internal/paneplace`; [keyboard and focus](../../docs/architecture/keyboard-focus.md) |
| Find | `find.go`, `find_session.go`, `find_ends.go`; [pane find](../../docs/architecture/pane-find.md#browser-pane-find) |
| Popups | `popup_adoption.go`, `popup_listing.go`, `popup_parked.go`, `popupadmit/` |
| Downloads and opening files | `downloads*.go`, `download_staging.go`, `user_downloads.go`, `download_files.go`, `destination/` |
| CDP engines (hosted, headless) | `cdp_*.go`, `cdporder/`, `hosted_*.go`, `headless_*.go` |
| WebKit engines | `webkit_*_linux.go`, `webkitglue_linux.c`, `wkwebview_*_darwin.go`, `wkwebviewglue_darwin.m`, shared `native_*.go`, `webkitjs.go`, `pagejs.go` |
| Tool schemas and dispatch | `mcp.go`, `tool_types.go` |
| Chords claimed in a page | `keys.go`, `harness_keys.go`; [keyboard and focus](../../docs/architecture/keyboard-focus.md#chords-in-the-page) |

## Ownership and lifecycle

- Register the thread before spawning its provider: the provider receives
  its capability URL in argv, so the MCP listener must exist first.
- One Manager owns one engine, profiles are per jar and pages per thread;
  every operation rechecks thread ownership.
- Every entry point takes an `Access` this Manager bound (`Bind`,
  `BindThread`, `BindMount`; `Manager.bound` refuses any other). Bind
  before the first wait and act only on the lifetime bound. Look a
  lifetime up by thread id only to show the thread as it is now.
- Act on a page by id only through `resolvePage`. Across any wait, act on
  the incarnation you hold (`tabHold`, `heldLocked`), never the id again.
- Change a lifetime's tabs only in a hold taken through `lockStructure`,
  which waits out a jar switch's capture and commit.
- A created, restored or adopted page is listed only by its one decision
  (`unlistedOutcomeLocked`, from `createReserved`, `restorePage` and
  `commitPopup`). Do not add another listing path.
- Select a tab only from the tabs as the selecting hold finds them
  (`ensureActivePageLocked`, `repairActivePageLocked`), never from an id
  remembered across a hold.
- A pane command that reads or changes the tab strip takes its turn in its
  mount's order (`selection.go`) and commits in the hold that applies it.
- `jar.go` alone maps a thread's jar choice to a jar. Engines see only the
  jar key and an ephemeral flag; navigation, file and download authority
  stay per workspace (`jarAllows`, `workspaceAuthority`).
- A presentation pass decides its show in `decidePresentation`. Base color
  goes through `applyPageBase`. Keyboard focus enters a page only by a
  click or a focus intent (`focus_intent.go`), which the engine checks on
  its UI thread immediately before moving focus.
- Pages lay out at the thread viewport (`sessionViewport`). Pane sizes are
  applied only in `viewport.go`, placement only through `placePage`
  (`internal/paneplace`, shared with the Windows launcher). Presentation
  never resizes a page.
- An omitted `page_id` resolves only when the thread owns at most one
  page. Never infer a caller or use MRU selection.
- Keep the resource bounds on profiles, pages, viewport size, operations,
  snapshots, screenshots, downloads, assets, console data and MCP bodies.
  Do not start an engine per tool call.
- Engine callbacks must not synchronously perform teardown that waits on
  the callback's own event loop or UI thread.

## Engine rules

- An engine implements the `driver.go` interfaces and nothing else; no
  policy crosses the seam. The hosted engine reaches WebView2 only through
  `internal/cdprelay`. Headless Chromium is selected only by the serve
  boot's explicit request, never as a fallback for a windowless process.
- Every engine reports a page it opened itself as `PopupOpened` before
  anything else of it, and every page's close and crash as it observes
  them. The Manager alone adopts or discards a popup (`runAdoption`), and an
  engine keeps no download of its own for one (`popup_parked.go`).
- The CDP engines answer the navigation policy browser-wide only
  (`profileOptions.Allow`); a page installs no interception of its own.
- State that two CDP event methods feed is read from one ordered input
  (`cdporder`); do not add per-method subscriptions for it. State fed by
  separate subscriptions settles the same way under any interleaving and
  bounds what it keeps (`cdp_targets.go`). Every CDP page attach is bounded
  (`startCDPPage`).
- The CDP title watcher (`cdp_page.go`) observes only the title path;
  never observe a document's whole subtree from it.
- The hosted engine acknowledges a launcher report at receipt
  (`hostedPages.receive`, which takes no engine lock) and processes it on
  the page's lane (`hosted_report_lanes.go`).
- All GTK/WebKit and AppKit/WKWebView calls go through `uiThread`
  (`native_uithread.go`, a `uicall.Thread`); the forbidigo rule in
  `.golangci.yml` refuses any other `Invoke*`. Do not call it while
  holding a lock a native callback can reach. A closure posted there
  carries ids and looks its object up on the thread (`withProfileOnUI`,
  `withDownloadOnUI`); native callbacks carry integer ids, never Go
  pointers.
- What the two WebKit engines do alike lives once: page operations in
  `native_page_ops.go`, presentation state in `native_pane.go`, popups in
  `native_popups.go`, page JavaScript in `webkitjs.go` and `pagejs.go`.
  Selectors and user text cross into JavaScript as JSON values, never
  source fragments.
- A parked WebKit view stays in its window in a clipped host at its
  viewport, and a WebKitGTK view keeps one widget parent from its first
  park to its close. Do not hide with opacity, natural-size requests or a
  page resize.
- Keep platform differences explicit; an unsupported capability answers a
  clear refusal.

## Files, downloads and site data

- `browser_open` (`path`) authorization is `authorizeFile`. The download rows
  the Manager lists are the one record of which downloaded files are
  openable (`listedDownloadFile`); keep no second one. Use direct `file://`
  navigation.
- Where a download goes is the engine's `DownloadSaving`
  ([Browser hardening](../../docs/architecture/browser-tools.md#browser-hardening)).
  A WebKit engine writes only into the staging folder AO names
  (`download_staging.go`), and the Manager publishes the finished file
  without copying or replacing (`publishFinishedDownload`,
  `placeNoReplace`); WebView2 saves its own (`engineSavedDownload`).
  Quotas, the per-download cap and pruning apply only to headless
  Chromium's artifacts, written into a directory per browser process
  (`headless_download_room.go`).
- Change a page's download entry only in a hold of its log, through
  `noteDownloadStartLocked` or `noteDownloadLocked`, then
  `publishDownloads`. The page log and the thread list enforce their own
  bounds; nothing else trims them.
- An engine reports one terminal event per download, after its start, and
  names its cause (`downloadState`); the Manager never infers a cause from
  a pending cancel. Every removal of a live page goes through
  `pageLeftLocked`.
- Preserve the denial of sensitive website permissions.
- Site data is the engine's and isolated per jar. Persistent and ephemeral
  profiles stay distinct, and a private jar is always ephemeral. Clearing
  site data closes pages first, then clears every place site data lives; a
  silent per-platform no-op is a bug.

## Tests and references

Unit tests use `fake_engine.go` and fake CDP endpoints and must not start or
download a browser; keep `ManagerOptions.FakeEngine` the default for test
boots. Real engines run only in the manual gates: `AO_HEADLESS_CHROMIUM_SMOKE=1`
for Chromium, and `AO_NATIVE_BROWSER_SMOKE=1` for WebKitGTK and WKWebView
(`native_engine_smoke_test.go`), which opens a window under a Weston of its
own on Linux; a window on the session display, the only one macOS has, also
needs `AO_NATIVE_BROWSER_SMOKE_SESSION_DISPLAY=1`.

- [codex-browser-parity.md](../../docs/references/codex-browser-parity.md)
- [internal/cdprelay](../cdprelay/AGENTS.md)
- [internal/webview2host](../webview2host/AGENTS.md)
