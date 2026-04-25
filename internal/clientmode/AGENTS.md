# internal/clientmode/

Phase F `agent-overflow --connect <url>` remote-client mode. The
desktop binary skips booting a local transport and points its Wails
webview at a remote-hosted backend over WebSocket.

## Layout

- `clientmode.go` — public surface: `Config`, `ParseConnectURL`,
  `Serve`, `Server`. The stub HTTP server, the bootstrap injection,
  and the URL parser all live here.
- `clientmode_test.go` — URL-parsing tests, bootstrap-injection
  tests, static-asset serving tests, idempotent shutdown.

## Responsibility boundary

- What BELONGS here:
  - Validating the operator-supplied `--connect` URL.
  - Booting a tiny loopback HTTP server that serves the embedded SPA
    bundle with `window.__AO_BOOTSTRAP__` injected into index.html.
- What does NOT belong here:
  - The WebSocket client itself — that's `frontend/src/lib/transport/
    wsClient.ts`. This package only sets up the bootstrap so the
    wsClient finds it.
  - Authenticating the remote server's TLS certificate. The OS trust
    store handles that; clientmode does not pin or verify beyond
    what `wss://` already gives you.
  - Any RPC dispatch, event bus, or `/ws` endpoint. Phase A's
    `internal/transport` owns those — and `clientmode` deliberately
    skips that whole stack so connecting clients don't ship a
    second backend.

## Bootstrap injection

The browser-side wsClient (Phase B) reads `window.__AO_BOOTSTRAP__`
on first call and uses it instead of fetching `/bootstrap.json`. We
inject the manifest into the SPA shell at server-boot time:

1. Read `index.html` from the embedded asset fs.
2. Find the literal `<head>` opener.
3. Insert `<script>window.__AO_BOOTSTRAP__ = {...};</script>` right
   after it so the global is set before any module preload completes.

Why a string-replace over `<head>` rather than a template parser:
the SPA's index.html is fully under our control (it ships from
frontend/dist as part of the same build), so the literal `<head>`
tag is stable. A full HTML parser would add dependency weight for
zero correctness gain.

The token is JSON-encoded and then `</` is replaced with `<\/` so a
hostile token literal can't break out of the inline `<script>` tag.

## Anti-patterns

- Do NOT add a WebSocket upgrade or RPC dispatch here. The whole
  point of `--connect` is to skip the local transport; reintroducing
  it would defeat the design.
- Do NOT pipe the bootstrap through `/bootstrap.json` instead of
  injecting into the page. The manifest fetch path is for the
  embedded-webview deployment; the `--connect` path uses the
  global to avoid an extra round-trip and to keep the loopback
  HTTP server side-effect free.
- Do NOT bind to anything other than 127.0.0.1. The loopback server
  is the boot shim for the local Wails webview; nothing else should
  reach it.

## References

- Phase B: `frontend/src/lib/transport/wsClient.ts` —
  `defaultBootstrap` reads `window.__AO_BOOTSTRAP__` before falling
  back to `/bootstrap.json`.
- Phase A: `internal/transport/server.go` — the in-process
  HTTP+WS server skipped on `--connect`.
