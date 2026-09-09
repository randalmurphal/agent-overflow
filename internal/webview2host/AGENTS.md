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
and preserve sibling z-order. Convert frontend CSS pixels using the owning
window's current scale. Hide during invalid or zero-sized geometry.

All COM controller and HWND mutations run on the launcher UI thread. Completion
handlers may arrive there, so never block on backend RPC inline. Serialize
reports to preserve created before later states such as closed.

Maintain handwritten COM interfaces in ABI order with explicit HRESULT,
reference-count, UTF-16 ownership, and callback-lifetime handling. Close the
host before its parent HWND and release controllers, handlers, relay,
environment, and COM references in dependency order.

See [embedded browser specification](../../docs/specs/embedded-browser.md).
