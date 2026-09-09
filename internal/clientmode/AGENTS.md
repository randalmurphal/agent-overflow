# internal/clientmode

Legacy single-upstream `agent-overflow --connect` relay. New paired desktop
frontends use [internal/frontendclient](../frontendclient/AGENTS.md). This
package serves the embedded SPA on loopback and carries its WebSocket and
attachment requests to one configured backend without booting a local app.

## Boundaries

- Validate the operator-supplied connect URL and bind the local server only to
  `127.0.0.1`.
- `internal/backendproxy` owns the shared upstream carrier. This package owns
  local page admission, bootstrap presentation, and relay configuration.
- `internal/deviceclient` owns pairing keys, session renewal, certificate
  pinning, and request authorization. `internal/transport` owns RPC dispatch,
  event replay, credentials, and origin policy.
- The page always connects to this process with the same SPA protocol used by
  embedded and remote-browser boots. Do not add another frontend transport.

## Credential handling

The upstream token or paired session remains in Go and must never appear in the
page URL, bootstrap body, script state, logs, or a local GET response. The host
delivers a single-use page ticket; bootstrap exchanges it for this relay's
HttpOnly cookie. Every WebSocket and attachment request authenticates that
cookie before being carried.

For token upstreams, the carrier replaces inbound credentials and session
headers with the configured upstream token and the session obtained through
`internal/relaysession`. For paired upstreams, each manifest probe receives a
request-bound proof and each WebSocket upgrade receives a single-use ticket
from `PairedUpstream`. Both use the pinned RoundTripper.

Do not forward the local Cookie, Origin, page marker, or arbitrary query
parameters upstream. Preserve only the bounded client identity fields defined by
the shared carrier. The attachment route preserves the upstream-minted path and
ticket and adds no upstream credential.

`/bootstrap.json` revalidates the upstream credential while returning this
relay's own manifest and WebSocket URL. Map a confirmed refusal to the terminal
404 shape and transient failures to 503 so the frontend continues its reconnect
policy.

Tests use local HTTP and WebSocket fixtures and injected paired clients. They
must not read a developer profile, connect off-host, or start a backend.

See [transport.md](../../docs/architecture/transport.md) and
[session-renewal.md](../../docs/architecture/session-renewal.md).
