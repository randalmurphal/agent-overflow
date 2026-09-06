# Paired Go clients

`Client` owns one computer's key-bound session and pinned HTTP transport.
Production code imports neither identity nor transport; wire spellings are
pinned by tests. Credential requests never follow redirects.

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
even if that proxy still answers ordinary HTTP; 401/403 remain auth handling.

Renewal's shared contract is [session-renewal.md](../../docs/architecture/session-renewal.md).
Save the proposed successor before sending to `/auth/token/recover`. Never
fall back to the legacy endpoint with a pending operation. Transient HTTP or
proof failures and unknown future refusal codes preserve pairing. Session/key file locks cover short local
transactions; only the separate legacy-renewal lock spans bounded network
work. Reload profiles under their OS lock and compare the current pairing
and refresh generation after the response. A late reply/refusal cannot
replace/delete a newer generation, rename or re-pairing. Unknown JSON fields
survive every update. Retired clients never write again.

Tests use private profile directories and local fixture servers; never use a
real device profile or provider home. Cross-process and lost-response tests
must exercise persisted state, not just two references to one Client.
