# Paired Go clients

`Client` owns one computer's key-bound session and pinned HTTP transport.
Production code imports neither identity nor transport; wire spellings are
pinned by tests. Credential requests never follow redirects.

Confirmation waits poll renewal, not socket tickets. Pending confirmation is
read-only on the server and preserves the saved renewal operation; approval
performs one successful rotation. Terminal refusal ends promptly and retires
the session, while transport outages retry until cancellation or the bounded
confirmation deadline. No unused socket tickets or audit errors per pending poll.

`routes.go` learns bounded alternatives from the authenticated bootstrap. Its
RoundTripper keeps the caller's original target stable, chooses one immutable
address/verifier before sending, and never replays a failed request. Alternative
selection verifies credential-free health against the paired backend ID. Probe
work is coalesced, bounded and independent of any one waiting request. Route
failure never removes a pairing. A trusted pin update invalidates new requests
on the old route without interrupting an already established socket. Keep the
same contract in the native transport; [computer-routes.md](../../docs/architecture/computer-routes.md)
owns the cross-platform design.

`RepairAddress` probes an explicitly entered address without credentials, then
rechecks the pairing and its current trust inside the profile transaction. A
delayed check must not restore a certificate replaced by a newer bootstrap.
Retain a pending renewal unchanged. Failed socket upgrades invalidate a route
even if that proxy still answers ordinary HTTP; 401/403 remain auth handling,
and so does 404, the upgrade's own answer for a spent ticket or a dead session.

Renewal's shared contract is [session-renewal.md](../../docs/architecture/session-renewal.md).
Save the proposed successor before sending to `/auth/token/recover`. Never
fall back to the legacy endpoint with a pending operation. Transient HTTP or
proof failures and unknown future refusal codes preserve pairing. The exchange
runs detached from the caller that started it: a cancelled caller returns at
once while the rotation completes for every waiter, bounded by the HTTP
timeout and the lock waits. So a rotation can outlive the test that cancelled
it, still touching the profile directory's lock files: `openAgainst` registers
a cleanup that waits for it before the TempDir is removed, and a fixture that
builds a client another way owes the same wait. Session/key file locks cover short local
transactions and their wait is bounded by `profileWriteTimeout` even for a
caller with no deadline; only the separate legacy-renewal lock spans bounded
network work. Reload profiles under their OS lock and compare the current pairing
and refresh generation after the response. A late reply/refusal cannot
replace/delete a newer generation, rename or re-pairing. Unknown JSON fields
survive every update. Retired clients never write again.

Tests use private profile directories and local fixture servers; never use a
real device profile or provider home. Cross-process and lost-response tests
must exercise persisted state, not just two references to one Client.

`WithDialContext` selects a network path at construction for pairing, reopened
clients, learned alternatives and address-repair probes alike. It bypasses
environment proxies but never changes TLS pinning, destination authority or
credential rules. The owner can route tailnet destinations through its tsnet
node and ordinary LAN destinations through the OS without a global dialer.

`Session.OwnDevice` records the invitation's intent so a confirmed own-device
introduction can replace an older limited profile without repeatedly replacing
an existing group session. It is not authorization: every membership RPC checks
its durable session admission on the destination. `KeyThumbprint` reads the same
RFC 7638 key identity used in proofs and never generates a key. Public catalog
transactions sharing a profile use `WithProfileLock`; keep network work outside
that short cross-process lock.

Automatic introductions select among the target member's bounded trusted routes
before redemption. Credential-free health probes verify both TLS trust and
backend identity; the first verified route wins without waiting for a dead LAN
or cold VPN alternative. Selection never sends the invitation token or a device
proof. Pair then performs one redemption only: a lost reply cannot safely retry
an invitation that may already have been spent.


`ObserveComputerRoutes` is shared by authenticated bootstrap and verified hello
snapshots. Identity must match this client's backend, and the profile transaction
fences retired/replaced pairings before saving trust. A live route invalidation
causes a bootstrap refresh through the existing desktop proxy, which feeds this
same owner; do not parse opaque WebSocket bytes in the reverse proxy.
