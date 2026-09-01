# internal/network/

Wire shape + helpers behind the LAN-bind toggle in Settings. Owns the
public `Settings` struct, the bind-host / origin-allow-list / share-
URL formatters, and the deterministic local-IP discovery. The App-
side orchestration (settings persist + transport rebind with
rollback) stays in `internal/app` so the failure-path rollback can
touch `*App.settings` directly.

What the share URL this package formats actually delivers is decided
elsewhere and worth knowing here: it loads the SPA, and the page then
needs a paired session before the backend will open its socket
(`internal/transport/AGENTS.md`, the launch credential and the `/ws`
upgrade). So the URL is how a device REACHES this backend, not how it
gets in — which is also why the one-time ticket on it buys less than
its shape suggests: it loads the page, and nothing else.

**Whether this URL says `https://` is decided by what a BROWSER can
verify, never by whether a certificate exists.** The transport terminates
TLS on the same port whenever it holds any certificate
(`internal/transport/AGENTS.md`, § Same-port TLS), and the self-signed one
is always there — but a browser cannot pin it and would meet a trust
warning, so for a self-signed-only backend the URL stays `http://` and
`Insecure` stays true on a LAN bind. That certificate is for clients that
own their own TLS configuration and pin the fingerprint the pairing
payload carried (`internal/deviceclient`).

The one case that flips is a CANONICAL DOMAIN with a certificate actually
loaded for it — an ACME issuance or the user's own file pair. Then the
URL is `https://<domain>[:port]/?t=…` (the port is dropped when it is 443)
and `Insecure` is false, because a trust store accepts those bytes under
that name.

`AppURLWithLAN` decides that by asking the LISTENER
(`transport.Server.ServesDomain(name)`), never the settings and never the
`TLSStatus` beside them. The name is part of the question: a user who just
changed their domain has a settings record naming the new one while the
old certificate is still what is loaded, and both a bare "a certificate
exists" test and the observed `Serving` string answer yes there — which
publishes an https URL nothing can complete a handshake on. A canonical
domain with no hook and no file pair is a legitimate third state — somebody
else's proxy terminates TLS in front — and it publishes the http URL,
because from here nothing can tell what that proxy does.

## The tailnet URL is a third answer, on its own terms

`TailnetStatus` is the node's observed state as `internal/app`'s reconciler
last read it, and `tailnetURL` mints the reachable address beside it —
`https://<magicdns-name>` when the tailnet issued a certificate, else
`http://<magicdns-name>:<port>`. Two things about it differ from everything
above, and both are deliberate:

- **There is no `Insecure` flag beside it.** That flag answers "would a
  browser warn", and it is `Settings.Insecure`'s job for the LAN URL. A
  tailnet address is reached over WireGuard, which authenticates and
  encrypts the hop whether or not TLS sits on top; marking the cleartext
  case insecure would tell the truth about the scheme and a lie about the
  connection.
- **It exists only while the node is RUNNING.** The URL carries a
  single-use page ticket like every other share URL, so producing one for
  a node nothing can reach would spend a ticket from a bounded book for a
  link nobody can open. An install with the feature off spends none.

The Tailscale STATE VOCABULARY is carried verbatim (`NeedsLogin`,
`Starting`, `Running`, …) rather than mapped onto our own words. It is what
the tailnet's own admin panel and CLI say, so a person comparing the two
screens is comparing the same words, and a state we have not seen before
still renders as itself instead of collapsing into "unknown".

## Layout

- `network.go`: `Settings`, `TailnetStatus`, `BindHost`, `OriginPatterns`,
  `AppURLWithLAN`, `FromServer` / `FromServerWithLAN`,
  `DiscoverLocalLANIP`. `Interfaces` and `InterfaceAddrs` are
  exported `var` hooks so tests can stub the iface enumeration
  without depending on the host's real network configuration.
  `AppURL` is NOT defined here: it is a method on `*transport.Server`
  that `AppURLWithLAN` calls for the loopback answer and every fallback.

## Responsibility boundary

- What BELONGS here: pure formatters (bind host, origin patterns,
  share URL), the iface discovery walk + Tailscale fallback, the
  `Settings` projection helper that turns `(server, bindAll, lanIP)`
  into a Settings record.
- What does NOT belong here: settings persistence (lives in
  `internal/settings`), transport rebind (lives in
  `internal/transport`), or the orchestration that combines the
  three (lives in `internal/app/app_network.go` so the rollback path can touch
  `*App.settings` on failure).

## Anti-patterns

- Do NOT return a public IPv4 from `DiscoverLocalLANIP`. A cloud VM
  flipping LAN-bind shouldn't auto-publish its public address.
  Without TLS, that invites a user to publish a cleartext endpoint
  on an open port. RFC1918 / link-local / Tailscale CGNAT only.
- Do NOT call `DiscoverLocalLANIP` more than once per Set flow. The
  origin allow-list and the URL must use the *same* discovered IP
  or the user can see a URL their browser can't reach without an
  origin failure. Use `OriginPatterns(bindAll, lanIP, domain)` +
  `AppURLWithLAN(srv, settings, lanIP)`.
- Do NOT change the JSON tags on `Settings` without a coordinated
  frontend change. The SPA's hand-maintained TS mirror plus the
  Wails-generated bindings both rely on the shape.
- Do NOT derive `URL` or `Insecure` from the `TLS` status field. It is
  what the settings screen DISPLAYS; the listener is what a browser
  connects to, and `Server.ServesDomain` is the only question worth
  asking. Two sources for one fact disagree exactly during the seconds a
  domain is being changed.
