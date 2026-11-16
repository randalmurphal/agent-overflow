# Embedded WebView2 host

This Windows package owns the secondary WebView2 used by the embedded browser
pane: COM setup, child-window geometry, profile environments, and the CDP relay.
The backend owns page intent and sends directives through the Windows launcher.

## Contracts

Validate every directive before touching a window. Result-bearing create and
clear-data operations always report success or failure. Other operations target
an existing page and must not fabricate one after construction failure.

A browser profile is an isolated WebView2 environment. Never share a user-data
folder concurrently across runtime modes or workspaces. Clear-data closes the
environment, removes the entire owned profile tree, recreates it, and reports
only after completion. Validate every path component against symlinks and
Windows reparse points before deletion.

`clear-data` may race an asynchronous environment create. Preserve the
generation contract documented on `ensureEnvironment`, `environmentCompleted`,
and `releaseEnvironment`: clearing advances `envGen`, wakes old waiters, and
forces a late completion to release its own COM references instead of adopting
an environment rooted in the deleted profile.

The launcher is the sole CDP dialer. Bind the relay to loopback, pair the target
identifier with its page, and close the relay with the controller. Never expose
WebView2's debug endpoint to the backend or LAN.

The browser controller is a child of the launcher's HWND. Use the clip container
and preserve sibling z-order. Convert frontend CSS pixels with the report's
`devicePixelRatio`, falling back to the window's client size over the SPA
viewport. Hide during invalid or zero-sized geometry. Hiding hides only the
container and keeps the controller `IsVisible`; an invisible controller stops
compositing, which freezes frames and hangs CDP captures. The page's size and
scale come from the backend's device-metrics override, never from the
controller's bounds.

The SPA also posts each pane geometry report to the launcher
(`geometry.go`); `presenter.go` decides where a page shows. A page bound by
its bounds directive to a mount with a local report is placed from that
report with `paneplace.Fit` and shows only while the backend shows it and
the report is visible, unless the directive's `Seq` (the SPA's number for
the report the backend placed from) is newer than the report's; then, and
without a local report, the directive's rect stands. Only backend
directives bind a page to a mount. Raw reports have their own validation:
an empty clip on either axis is a hidden pane, not an error. The mailbox
keeps the newest report per mount, drops older sequences, and has at most
one UI-thread drain queued.

Keyboard focus moves into a page only by `OpFocus`, and only while the host
shows the page and the launcher window is the foreground window
(`focusDecision`); AO never takes the OS focus from another application.

All COM controller and HWND mutations run on the launcher UI thread. Completion
handlers may arrive there, so never block on backend RPC inline. Serialize
reports to preserve created before later states such as closed.

Maintain handwritten COM interfaces in ABI order with explicit HRESULT,
reference-count, UTF-16 ownership, and callback-lifetime handling. Close the
host before its parent HWND and release controllers, handlers, relay,
environment, and COM references in dependency order. A Go object handed to
WebView2 as a COM pointer counts no references, so its constructor roots it
for the process (`keepForWebView2`); never make its lifetime depend on a page
or a clear.

Real WebView2 behavior is covered only by the `AO_NATIVE_BROWSER_SMOKE=1`
Windows tests (`find_smoke_windows_test.go`, `focus_smoke_windows_test.go`,
`process_smoke_windows_test.go`),
which need an installed runtime and open a window. From WSL, build them with
`GOOS=windows go test -c` and run the binary with
`WSLENV=AO_NATIVE_BROWSER_SMOKE`.

See [embedded browser specification](../../docs/specs/embedded-browser.md).
