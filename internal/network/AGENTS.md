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
gets in — which is also why the token in it buys less than its shape
suggests.

## Layout

- `network.go`: `Settings`, `BindHost`, `OriginPatterns`,
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
  Without TLS, that invites a user to share a token over an open
  port. RFC1918 / link-local / Tailscale CGNAT only.
- Do NOT call `DiscoverLocalLANIP` more than once per Set flow. The
  origin allow-list and the URL must use the *same* discovered IP
  or the user can see a URL their browser can't reach without an
  origin failure. Use `OriginPatterns(bindAll, lanIP)` +
  `AppURLWithLAN(srv, bindAll, lanIP)`.
- Do NOT change the JSON tags on `Settings` without a coordinated
  frontend change. The SPA's hand-maintained TS mirror plus the
  Wails-generated bindings both rely on the shape.
