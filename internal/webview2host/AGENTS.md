# Embedded WebView2 host

This Windows package hosts the browser pane's pages in the Windows
launcher: COM setup, child-window geometry, the pane's WebView2
environment, popups, downloads and the CDP relay. The backend owns page
intent and sends directives through the launcher
([internal/wsllauncher](../wsllauncher/AGENTS.md)). The package comment
says what builds off Windows. Behavior is in
[browser-tools.md](../../docs/architecture/browser-tools.md) and
[embedded-browser.md § 5](../../docs/specs/embedded-browser.md#5-windows-hosting-and-the-cdp-relay);
the contracts of the symbols named below are their doc comments.

## Where to look

| Change | Start here |
|---|---|
| Directives and reports on the wire | `directive.go`, `reportqueue.go`, `targetinfo.go` |
| Page creation, close, directive application | `host_create_windows.go`, `host_close_windows.go`, `host_apply_windows.go` |
| Environment and clear-data | `environment.go` (`paneEnvironment`), `host_windows.go`, `identifiers.go` |
| Geometry, presentation, focus | `geometry.go`, `presenter.go`, `focusintent.go`, `internal/paneplace` |
| Popups | `popup.go`, `popup_windows.go`, `popup_com_windows.go` |
| Downloads | `download.go`, `downloadoutcome.go`, `download_windows.go`, `download_reconcile_windows.go`, `download_dialog_windows.go` |
| COM objects and interfaces | `com_windows.go`, `comref_windows.go` |
| CDP tunnel | `cdptunnel.go`, `tunnelstream.go`, `cdpframe.go`; [internal/cdprelay](../cdprelay/AGENTS.md) |

## Rules

- Validate every directive before touching a window. Create and clear-data
  always report success or failure; any other operation targets an
  existing page and never fabricates one after a failed create.
- The pane has one user-data folder per app mode, apart from the SPA's,
  with each jar a named `CoreWebView2Profile` inside it. Never share a
  folder across modes. The launcher validates and creates it, refusing
  symlinked and reparse-point components (`prepareBrowserProfileStorage`);
  this package only takes it (`Config.UserDataDir`).
- Preserve the clear-data generation contract in `paneEnvironment`. While
  clear-data owns the folder the host refuses page admissions and further
  clears; a closed host refuses clear-data.
- The launcher is the sole CDP dialer. Bind the relay to loopback, pair
  the target with its page, close the relay with the controller, and never
  expose WebView2's debug endpoint to the backend or the LAN.
- The controller is a child of the launcher's HWND inside the clip
  container; preserve sibling z-order. Hide only the container: an
  invisible controller stops compositing, which freezes frames and hangs
  CDP captures. A page's size and scale come from the backend's
  device-metrics override, never from the controller's bounds, and its base
  color only from `OpBackground`.
- Only backend directives bind a page to a mount; the newer of the
  directive and the local report places it (`pagePresentation.view`).
  Moves go through `placePage`, and the host's shown or hidden state
  changes only after the native call succeeded (`nextPresentStep`).
- Keyboard focus enters a page only by `OpFocus`, under `focusDecision`;
  AO never takes the OS focus from another application. The intent
  protocol is in
  [keyboard and focus](../../docs/architecture/keyboard-focus.md#focus-intents).
- All COM and HWND mutations run on the launcher UI thread, which alone
  owns the page registry (`admit`, `closeTaken`) in `Config.OnMain` turns;
  later work goes through `Config.Post`. Never block on backend RPC there.
  Reports keep their order, created before any later state.
- A page whose required handlers (downloads, the download dialog, new
  windows) cannot be subscribed, or whose dialog state cannot be read, is
  not created.
- Keep handwritten COM interfaces in ABI order with explicit HRESULT,
  reference-count, UTF-16 ownership and callback-lifetime handling. A
  Go-backed COM object follows `comref_windows.go`. Close the host before
  its parent HWND and release in dependency order.
- Every window a page opens is a launcher page, never a window of
  WebView2's own; every `NewWindowRequested` is answered exactly once
  (`settlePopup`). A popup's reports wait for its announcement
  (`pageReport`).
- The launcher takes every `DownloadStarting`. Every read of a download's
  operation goes through `reconcileDownload`, and only its verdict
  (`downloadoutcome.go`) makes a terminal snapshot. What the real runtime
  does on a drop is recorded at `reconcileDownload`, `settleCancel` and in
  `download_smoke_windows_test.go`.

## Tests

Real WebView2 behavior is covered only by the `AO_NATIVE_BROWSER_SMOKE=1`
Windows tests (`*_smoke_windows_test.go`), which need an installed
runtime. From WSL, build them with `GOOS=windows go test -c` and run the
binary with `WSLENV=AO_NATIVE_BROWSER_SMOKE`. A smoke never shows anything
on the user's screen or takes their keyboard: the binary runs itself
again on a desktop of its own, never switched to
(`desktop_smoke_windows_test.go`), and refuses to create a window on the
input desktop or `Default` unless
`AO_NATIVE_BROWSER_SMOKE_ON_USER_DESKTOP=1`. The popup and policy smokes
allow only WebView2 windows owned by the smoke window
(`window_watch_smoke_windows_test.go`). That desktop is never the input
desktop, so the focus smoke stands in the host's foreground answer
(`Host.foreground`).
