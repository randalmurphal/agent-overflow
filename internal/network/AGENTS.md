# internal/network

Settings and pure helpers for listener exposure: bind selection, exact origin
patterns, share URLs, local-address discovery, pairing URLs, and advertised
computer routes. Application code owns persistence and listener rebind rollback.

## Listener and URL contract

- Build origin patterns after the listener binds and include its exact port.
  Cookies are host-scoped, so accepting another port on the same host would
  admit the wrong origin. Include HTTP and HTTPS spellings because the shared
  listener supports both on one port.
- Publish an HTTPS browser URL only when the active listener serves the named
  canonical domain with a certificate a browser can verify. A self-signed
  certificate remains available to pinned clients but does not make the browser
  URL HTTPS.
- Use the listener's observed address and served domain rather than saved
  settings when formatting current state. Invalid or unresolved ports produce no
  speculative origin entry.
- Share URLs provide reachability and a one-time page ticket. Pairing and
  transport authorization still control admission.

Tailnet URLs use the live node status: HTTPS when its MagicDNS certificate is
available, otherwise cleartext HTTP over the authenticated tailnet path. Publish
one only while the node is running. Keep Tailscale state names unchanged.

## Pairing and route advertisements

Pairing URLs choose a listener the joining device can reach. Loopback is valid
only for a same-machine flow; LAN, canonical-domain, and tailnet candidates must
reflect live listener state. Do not let a request parameter assert locality.

`ComputerRoutes` advertises bounded, credential-free HTTPS alternatives. Each
address retains its own TLS trust. This package formats observations;
`internal/computerroute` validates them and clients verify backend identity
before sending credentials.

Address discovery must be deterministic, bounded, and side-effect free. Ignore
loopback, unspecified, multicast, and unusable interface addresses. Prefer
stable private addresses without assuming interface names or enumeration order.

Tests inject interfaces and listener observations. They must not depend on the
developer machine's current network.

See [computer-routes.md](../../docs/architecture/computer-routes.md),
[transport.md](../../docs/architecture/transport.md), and
[remote-access.md](../../docs/specs/remote-access.md).
