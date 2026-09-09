# internal/browser

Built-in browser MCP and companion-pane manager. `driver.go` isolates engine
implementations: launcher-hosted WebView2 on Windows/WSL, WebKitGTK on native
Linux, WKWebView on macOS, and explicitly enabled headless Chromium in serve
mode. Product behavior and authority are defined in
[browser-tools.md](../../docs/architecture/browser-tools.md).

## Ownership and lifecycle

- Register the thread before spawning its provider. First registration starts
  the loopback MCP listener because the provider receives its per-thread
  capability URL in argv; starting it on first tool use is too late.
  Unregistering revokes that capability and closes only that thread's pages.
- One manager owns one engine. Profiles are per workspace, pages are per thread,
  and every operation rechecks thread ownership. Closing the last page disposes
  its profile; manager shutdown joins all engine work.
- Pages start hidden. Present only the selected page for a visible companion
  pane. The pane uses a native view positioned over the SPA; pixels do not cross
  the transport.
- An omitted `page_id` may resolve only when the thread owns at most one page.
  With multiple pages, require an explicit handle. Never infer a caller or use
  MRU selection.
- Keep the existing resource bounds on profiles, pages, viewport size,
  operations, snapshots, screenshots, downloads, assets, console data, and MCP
  bodies. Do not start an engine per tool call.
- Engine callbacks must not synchronously perform teardown that waits on the
  callback's own event loop or UI thread.

## Engine rules

The CDP engines use `chromedp` through the shared driver seam. The hosted
engine reaches the Windows controller only through `internal/cdprelay`; the
headless engine starts one Chromium process per profile with an isolated user
data directory. Headless mode is selected explicitly by serve boot and must not
become a fallback for an ordinary windowless process.

All GTK/WebKit calls go through `gtkDo`; all AppKit/WKWebView calls go through
`wkDo`. Do not call either while holding a lock reachable from a native
callback. Native callbacks carry integer IDs, never Go pointers. Keep shared
page-operation JavaScript in `webkitjs.go` and `pagejs.go`; selectors and user
text cross as JSON values rather than source fragments.

Hidden native WebKit pages must remain attached to their window in a clipped
1x1 host so layout, animation, and snapshots remain live. Pane geometry uses
per-page clipping and the full page rect. Do not use opacity or natural-size
requests as substitutes.

Platform differences remain explicit. WebKit input is programmatic and reports
`isTrusted=false`; CDP native input must assert resulting DOM state. WKWebView
site data uses identified WebKit stores where available and has no AO-owned
profile directory. Unsupported platform capabilities return a clear refusal.

## Files, downloads, and site data

`browser_open_file` resolves symlinks and accepts regular files within the
thread workspace or project root unless the explicit outside-workspace setting
widens access. Use direct `file://` navigation.

Downloads use sanitized unique names under AO-owned artifact directories and
never the user's Downloads directory. Preserve quota reservation, per-download
bounds, page cancellation, and denial of sensitive website permissions.

Site data belongs to the engine and is isolated per workspace. Persistent and
ephemeral profiles must remain distinct. Clearing site data first closes pages,
then removes AO-owned profile trees and asks engines with externally managed
stores to clear themselves. A platform-specific silent no-op is incorrect.

## Tests and references

Unit tests use `fake_engine.go` and fake CDP endpoints. They must not start or
download a browser. Keep `ManagerOptions.FakeEngine` as the default for test
boots. Real Chromium launch compatibility is covered only by the documented
`AO_HEADLESS_CHROMIUM_SMOKE=1` manual gate.

- [embedded-browser.md](../../docs/specs/embedded-browser.md)
- [codex-browser-parity.md](../../docs/references/codex-browser-parity.md)
- [in-app-browser-spike.md](../../docs/architecture/in-app-browser-spike.md)
- [internal/cdprelay](../cdprelay/AGENTS.md)
- [internal/webview2host](../webview2host/AGENTS.md)
