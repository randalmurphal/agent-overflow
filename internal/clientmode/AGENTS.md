# internal/clientmode/

`agent-overflow --connect <url>` remote-client mode. The desktop binary
skips booting a local transport and points its Wails webview at a
remote-hosted backend, reached through this stub.

## Layout

- `clientmode.go` is the public surface: `Config`, `ParseConnectURL`,
  `Serve`, `Server`. The stub HTTP server, the credential exchange, the
  WebSocket carrier, and the URL parser all live here.
- `clientmode_test.go`: URL-parsing tests, shell-serving and manifest
  tests, the credential and origin rules on `/ws`, the upstream
  revalidation verdicts, static-asset serving, idempotent shutdown.

## Responsibility boundary

- What BELONGS here:
  - Validating the operator-supplied `--connect` URL.
  - Booting a tiny loopback HTTP server that serves the embedded SPA
    bundle, answers `/bootstrap.json`, and carries `/ws` to the upstream.
  - Holding the upstream session token, which never leaves this process.
- What does NOT belong here:
  - The WebSocket client itself. That's `frontend/src/lib/transport/
    wsClient.ts`. This package only carries the socket it opens.
  - Authenticating the remote server's TLS certificate. The OS trust
    store handles that; clientmode does not pin or verify beyond
    what `wss://` already gives you.
  - Any RPC dispatch, event bus, replay ring, or upgrade of its own.
    `internal/transport` owns those, and `clientmode` deliberately skips
    that whole stack so connecting clients don't ship a second backend.

## The upstream token stays server-side

The page never receives the upstream credential, in any form a script
could read. The flow mirrors a local boot exactly:

1. `Serve` builds a `transport.Credential` around the configured upstream
   token — the same type, and therefore the same `Authenticate`, the
   transport server uses.
2. `AppURL` mints a one-time page ticket and stamps `?t=…&mode=client&cid=…`
   on the URL it hands the webview.
3. `handleBootstrap` calls `Credential.Exchange`, which validates the
   ticket and sets this stub's own HttpOnly cookie for this origin. The
   SPA scrubs the ticket from the URL, and every later request rides the
   cookie.
4. `handleWS` checks `transport.OriginAllowed` and `Credential.Authenticate`,
   then hands the request to a `httputil.ReverseProxy` that deletes the
   local `Cookie` and `Origin` headers and sets `Authorization: Bearer
   <upstream token>` for the hop.

So the SPA's socket is same-origin (it dials this stub), and the
cross-origin hop happens in Go where no page script can observe it.

**Carrying `/ws` rather than pointing the browser at the upstream is the
design choice this package is built around.** The alternative — hand the
page the upstream's `wsUrl` — cannot work under a cookie model: this stub
cannot set a cookie for another origin, so the page would need a
credential it could read, which is precisely what the boot flow removed.
Carrying it also means the SPA code is byte-identical across embedded,
`--connect`, and remote-browser boots: one origin, one cookie, one
`validateWsUrl` rule with no exemptions. The cost is one extra process
hop per frame on the `--connect` path, and the proxy is written to keep
that hop transparent: the handshake key, subprotocols, and the
compression extension all pass through, so the browser and the upstream
negotiate with each other and this process only splices bytes.

## Anti-patterns

- Do NOT reintroduce a `window.__AO_BOOTSTRAP__` injection, a
  `sessionStorage` stash, or any other route by which page script can
  read a credential. The shell is served verbatim; a first-load
  `/bootstrap.json` fetch is how the page learns its manifest, and the
  round trip it costs is what buys the HttpOnly cookie.
- Do NOT serve the upstream token to a local GET. Anything on this
  listener that would hand out the token is a way for any process on the
  host to become a client; the ticket exchange exists so it doesn't need
  to be.
- Do NOT add RPC dispatch, an event bus, or a real upgrade here. `/ws` is
  a byte carrier with no protocol knowledge; the whole point of
  `--connect` is to skip the local transport.
- Do NOT bind to anything other than 127.0.0.1. The loopback server
  is the boot shim for the local Wails webview; nothing else should
  reach it.

## `/bootstrap.json` is also the credential-revalidation probe

The SPA refetches the manifest mid-outage to learn whether its session is
still honoured. The stub answers by asking the upstream from Go
(CORS-free) with the bearer token, then maps the verdict onto the shapes
the SPA already knows (transport/AGENTS.md § "Credentials and refusal
shapes"): a refusal is 404, everything transient — network failure
included — is 503, so the reconnect ladder survives. This is what lets
the SPA's terminal `unauthorized` state cover `--connect` clients, whose
page cannot ask the upstream anything itself.

The body is the stub's OWN manifest, never the upstream's: `wsUrl` must
name THIS origin, since that is where the page's socket goes and the only
`wsUrl` `validateWsUrl` accepts. The manifest carries no credential. The
ticket exchange runs BEFORE the probe, so a page load during an upstream
outage still lands its cookie and its retry costs no ticket.

## References

- `frontend/src/lib/transport/bootstrap.ts`: `defaultBootstrap` fetches
  `/bootstrap.json`, forwarding the URL's ticket on first contact.
- `internal/transport/credential.go`: the credential type, the ticket
  exchange, and the origin rule this package reuses rather than restates.
- `internal/transport/server.go`: the in-process HTTP+WS server
  skipped on `--connect`.
