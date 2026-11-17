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
generation contract in `paneEnvironment` (`environment.go`, cross-platform and
unit-tested): clearing advances the generation, wakes old waiters, skips a
start queued for the old generation, and forces a late completion to release
its own COM references instead of adopting an environment rooted in the
deleted profile.

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
controller's bounds. A page's base color arrives on its own directive
(`OpBackground`), with or without bounds, and holds shown or hidden; bounds
carry no color.

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
shows the page, the launcher window is the foreground window, and the SPA
has not withdrawn the directive's focus intent (`focusDecision`,
`focusintent.go`); AO never takes the OS focus from another application.
The SPA posts each intent's arm and withdrawal to the launcher directly, so
a withdrawal wins over a directive still crossing WSL. Delivery is not
application: a directive for a page the launcher's own presentation hides
stays pending, and the page's next show applies it once (`present`).

All COM controller and HWND mutations run on the launcher UI thread. Completion
handlers may arrive there, so never block on backend RPC inline. Serialize
reports to preserve created before later states such as closed.

Maintain handwritten COM interfaces in ABI order with explicit HRESULT,
reference-count, UTF-16 ownership, and callback-lifetime handling. Close the
host before its parent HWND and release controllers, handlers, relay,
environment, and COM references in dependency order. A Go object handed to
WebView2 as a COM pointer counts its references and is rooted while any
remain (`comref_windows.go`). Its creator releases the constructor's
reference after the hand-off call, success or failure; a page detaches its
callbacks when it goes, except controller-completed, whose completion closes
a controller that outlived its page.

A page's downloads are the launcher's (`download_windows.go`): it handles
each `DownloadStarting`, so WebView2's dialog never opens, keeps the
`DownloadOperation` and reports every change as the download's whole
snapshot (`ReportDownload`, `download.go`), progress throttled, each
numbered (`Seq`) in the order taken so the backend applies only newer
ones. A directive naming a download (`OpCancelDownload`,
`OpDownloadStatus`) is answered with a snapshot marked `Answer`, which the
backend never creates a row from: a running one's, an ended one's kept
terminal snapshot under its own number (up to `MaxEndedDownloads` per page
while the page lives), or `DownloadUnknown` for an id with no record. A
report is bounded: the address is shortened, then the path left out,
never the start or end. WebView2 resumes a
dropped download as a new `DownloadStarting` whose operation carries the
interruption's reason; it is adopted into the page's running download from
that address (the one saving to its file, else the oldest), set back to
that download's file, and the operations it replaced are kept, detached,
until the download ends, because releasing them stops the new one's
reports. A download ends, its handlers detached and every operation
released, at its terminal state (an interrupted operation canceled first)
or when its page goes (`retireDownloads`), which cancels it and reports
nothing. What the real runtime does on a drop is recorded at
`downloadStateChanged` and in `download_smoke_windows_test.go`.

Real WebView2 behavior is covered only by the `AO_NATIVE_BROWSER_SMOKE=1`
Windows tests (`find_smoke_windows_test.go`, `focus_smoke_windows_test.go`,
`process_smoke_windows_test.go`, `download_smoke_windows_test.go`,
`crash_smoke_windows_test.go`, `title_smoke_windows_test.go`,
`background_smoke_windows_test.go`),
which need an installed runtime. From WSL, build them with
`GOOS=windows go test -c` and run the binary with
`WSLENV=AO_NATIVE_BROWSER_SMOKE`. A smoke never shows anything on the
user's screen or takes their keyboard: the binary runs itself again on a
desktop of its own, never switched to (`desktop_smoke_windows_test.go`),
refuses to run the smokes when it cannot, and a smoke refuses to open a
window on any other desktop, unless
`AO_NATIVE_BROWSER_SMOKE_ON_USER_DESKTOP=1`. That desktop is never the
input desktop, so the focus smoke stands in the host's foreground answer
(`Host.foreground`).

See [embedded browser specification](../../docs/specs/embedded-browser.md).
