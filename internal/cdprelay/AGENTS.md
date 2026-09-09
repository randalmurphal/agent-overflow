# internal/cdprelay

WSL backend endpoint for the embedded browser's CDP tunnel. The Windows launcher
owns the frame protocol in `internal/webview2host`; the browser manager consumes
`BrowserWebSocketURL`. The architecture is described in
[embedded-browser.md](../../docs/specs/embedded-browser.md#5-windows-hosting-and-the-cdp-relay).

- The launcher initiates the authenticated `/browser-cdp` connection. This
  package listens only on an ephemeral WSL loopback address for chromedp.
- Never dial an address supplied by a tunnel frame or debugger response.
  `RewriteDebuggerURL` retains only the debugger path and substitutes the
  endpoint's own scheme and address. Keep `chromedp.NoModifyURL`.
- Bind a local connection to its stream before sending `open`, wait for
  `opened` before data, and split data at `TunnelChunkBytes`. Enforce
  `MaxTunnelFrameBytes` and `MaxTunnelStreams`.
- One launcher tunnel is current. Installing a new connection closes the old
  tunnel and its streams; an old connection's teardown must not close its
  replacement. Local connections are closed immediately when no tunnel exists.
- Preserve the bounded stream-open, write, and discovery timeouts. Discovery
  response bodies remain bounded.

Tests use loopback HTTP and WebSocket fixtures and do not start a launcher or
WebView2. Transport admission for `/browser-cdp` remains in
[transport.md](../../docs/architecture/transport.md).
