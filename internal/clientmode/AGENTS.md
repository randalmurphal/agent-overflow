# internal/clientmode/

`agent-overflow --connect <url>` remote-client mode. The desktop binary
skips booting a local transport and points its Wails webview at a
remote-hosted backend over WebSocket.

## Layout

- `clientmode.go` is the public surface: `Config`, `ParseConnectURL`,
  `Serve`, `Server`. The stub HTTP server, the bootstrap injection,
  and the URL parser all live here.
- `clientmode_test.go`: URL-parsing tests, bootstrap-injection
  tests, static-asset serving tests, idempotent shutdown.

## Responsibility boundary

- What BELONGS here:
  - Validating the operator-supplied `--connect` URL.
  - Booting a tiny loopback HTTP server that serves the embedded SPA
    bundle with `window.__AO_BOOTSTRAP__` injected into index.html.
- What does NOT belong here:
  - The WebSocket client itself. That's `frontend/src/lib/transport/
    wsClient.ts`. This package only sets up the bootstrap so the
    wsClient finds it.
  - Authenticating the remote server's TLS certificate. The OS trust
    store handles that; clientmode does not pin or verify beyond
    what `wss://` already gives you.
  - Any RPC dispatch, event bus, or `/ws` endpoint. `internal/transport`
    owns those, and `clientmode` deliberately skips that whole stack
    so connecting clients don't ship a second backend.

## Bootstrap injection

The browser-side wsClient reads `window.__AO_BOOTSTRAP__` on first call
and uses it instead of fetching `/bootstrap.json`. We inject the
manifest into the SPA shell at server-boot time:

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
- Do NOT replace the index injection with a first-load
  `/bootstrap.json` fetch. The global keeps startup zero-round-trip
  and tolerant of a briefly unreachable upstream. The stub DOES serve
  `/bootstrap.json`, but only as the credential revalidation probe
  (`handleBootstrap`): the SPA refetches it mid-outage, the stub asks
  the upstream from Go (CORS-free), and a refused token maps to the
  same 404 a browser session observes, which is what lets the SPA's
  terminal 'unauthorized' state cover `--connect` clients. On success
  it answers with the stub's OWN manifest. The upstream's names wsUrl
  from the server's perspective (wrong through an SSH tunnel) and
  carries no `mode:"client"`.
- Do NOT bind to anything other than 127.0.0.1. The loopback server
  is the boot shim for the local Wails webview; nothing else should
  reach it.

## The injected manifest is the SPA's same-origin exemption

`validateWsUrl` (`frontend/src/lib/transport/bootstrap.ts`) refuses a
manifest whose `wsUrl` names an origin other than the page's. The
transport derives that field from the request's own `Host` header, so
anything else was tampered with in flight. Both halves of `--connect`
are legitimately cross-origin and are the only exemptions: the injected
`window.__AO_BOOTSTRAP__` at first load, and the same manifest served
again from this stub's `/bootstrap.json` for the reconnect revalidation.

The SPA opts out per CALL SITE, keyed on how the manifest reached the
page (an out-of-band injection by the shell that owns the process), and
deliberately NOT on the `mode:"client"` field inside the manifest. A
spoofed manifest would exempt itself for free. So: do not move the
exemption's trigger into the manifest body, and do not make the stub
serve a manifest from a path the SPA reaches without an injected global.

## References

- `frontend/src/lib/transport/wsClient.ts`: `defaultBootstrap` reads
  `window.__AO_BOOTSTRAP__` before falling back to `/bootstrap.json`.
- `internal/transport/server.go`: the in-process HTTP+WS server
  skipped on `--connect`.
