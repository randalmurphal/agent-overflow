# internal/tailnet

Runs this backend as a userspace `tailscale.com/tsnet` node. It owns node
lifecycle, status, bounded peer enumeration, listeners, and outbound dialing.
The application reconciles settings; `internal/transport` serves every accepted
connection.

## Lifecycle and state

- Call `envknob.SetNoLogsNoSupport()` before starting tsnet. Remote support-log
  upload is outside this feature's data path.
- A `Node` is single-use. `Close` is idempotent, checks whether start occurred,
  and bounds teardown. Restart by constructing a new node over the same state
  directory.
- The state directory contains node identity key material. Disabling keeps it;
  only explicit `Forget` removes it, and never while a node is live.
- `Start` must publish intermediate states and login URLs rather than blocking
  on `tsnet.Server.Up`. Refresh identity and certificate fields on state and
  self changes. Clear spent login URLs.
- `Events()` is a coalesced wake channel. Consumers reread the complete current
  status. Close it when the node stops.

`Listen` and `ListenTLS` require the running state. TLS additionally requires
MagicDNS and HTTPS certificate availability. A cleartext listener still travels
inside the authenticated tailnet.

Peer enumeration yields connection candidates, not trusted Agent Overflow
computers. Never infer authorization from a hostname or tailnet membership.
Callers still perform pairing, certificate checks, and backend identity
verification. Outbound tailnet dialing is injected into existing pinned clients
rather than creating another protocol.

Tests use the in-process Tailscale control and DERP rig and refuse ambient
Tailscale credentials or non-loopback control URLs. Production files must not
import `tailscale.com/tstest/integration`. Live sign-in, certificate issuance,
and cross-network DERP behavior remain manual integration boundaries.

See [remote-access.md](../../docs/specs/remote-access.md).
