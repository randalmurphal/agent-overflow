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

## The origin allow-list names EXACT PORTS

`OriginPatterns(bindAll, lanIP, canonicalDomain, port)` takes the port the
listener actually bound, and every pattern it emits carries it. That is a
wave-9 correction, not a refinement: the LAN entries were
`http://127.0.0.1:*`, `http://localhost:*` and `http://<lanIP>:*` from the
day the transport landed, so a document served by ANY port on this machine
named an origin the WS upgrade accepted — with this backend's page cookie
attached to the handshake, because cookies are scoped by host and not by
port. Nothing ever needed the wildcard; it was written when the list went
straight to the WebSocket library's matcher and the bound port was not
threaded down here.

Two consequences for callers:

- **The list cannot be built before the bind.** `main.go` installs it
  after `Server.Start` (an ephemeral bind has no port until then) and
  `internal/app/app_network.go` builds it per branch: the rebind branch
  from the port it asked for, re-set from `Addr()` when the listener
  landed somewhere else, and the no-move branch from the current port.
- **Each host appears under both schemes.** The listener classifies a
  connection by its first byte and answers TLS and cleartext on the one
  port (`internal/transport/AGENTS.md`, § Same-port TLS), so
  `http://<host>:<port>` and `https://<host>:<port>` are two spellings of
  the same listener.

An unresolved port (0, or out of range) drops every port-bearing pattern
rather than guessing one. The request's own authority is still admitted by
`transport.OriginAllowed` and is exact by construction, so the failure
mode is a refusal.

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

## Pairing chooses the reachable listener

`PairingURL` prefers the live tailnet, then the canonical domain, LAN,
and loopback. It returns the certificate pin with the URL: tailnet and
canonical HTTPS use WebPKI, and only the main address uses its self-signed
pin. Never copy that pin onto a tailnet invitation. Mint only the ticket
being returned; unused fallback tickets evict links still being scanned.
`TestPairingURLUsesReachableListenerAndMatchingTrust` enforces both rules.

## Two records, and what the second leaves out

`Settings` is read by two kinds of caller and only one of them is at the
machine. `GetNetworkSettings` answers `access:admin` — managing how a
backend is exposed is what a paired admin device is FOR, and gating the
read on host presence made Settings → Remote access unreachable from every
device the owner had paired. So there are two builders, and which one runs
is `internal/app`'s pick from the per-call proof:

- `FromServer` / `FromServerWithLAN` — the caller is at the machine.
- `FromServerRedacted` — everybody else. Four fields are left empty:
  `Token` (this LAUNCH's credential, which would let its holder attach as
  the backend's own local channel), `URL` and `Tailnet.URL` (each carrying
  a one-time page ticket), and `Insecure`, which describes a URL that is
  not there.

**Withholding is done by never MINTING, and the function takes no
`*transport.Server` so it cannot.** Building the full record and clearing
it afterwards would spend the same tickets out of a book of sixteen, so
each remote read of the screen would evict the URL the owner had just
copied at their own. `FromServerWithLAN` is written the other way round —
it STARTS from the redacted record and fills in — so the withheld set is
declared once and a fifth server-derived field cannot be added on one path
only.

**`Tailnet.AuthURL` deliberately travels.** It looks like the two URLs
that do not: single use, and a link. It is the tailscale sign-in link the
node publishes while it waits to be approved, which is exactly what a
remote owner needs in order to approve this machine — withholding it would
leave them able to enable the feature and unable to finish it. It is not a
page ticket and it is not ours to mint.

## Layout

- `network.go`: `Settings`, `TailnetStatus`, `BindHost`, `OriginPatterns`,
  `AppURLWithLAN`, `FromServer` / `FromServerWithLAN` /
  `FromServerRedacted`,
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
  origin failure. Use `OriginPatterns(bindAll, lanIP, domain, port)` +
  `AppURLWithLAN(srv, settings, lanIP)`.
- Do NOT reintroduce a wildcard port in an origin pattern. This machine
  serves agent-authored bytes on other ports of these same hosts (the
  dev-server preview listeners, `docs/specs/remote-access.md` §7), and the
  port is the only thing separating them from the SPA's own origin.
- Do NOT change the JSON tags on `Settings` without a coordinated
  frontend change. The SPA's hand-maintained TS mirror plus the
  Wails-generated bindings both rely on the shape.
- Do NOT derive `URL` or `Insecure` from the `TLS` status field. It is
  what the settings screen DISPLAYS; the listener is what a browser
  connects to, and `Server.ServesDomain` is the only question worth
  asking. Two sources for one fact disagree exactly during the seconds a
  domain is being changed.
