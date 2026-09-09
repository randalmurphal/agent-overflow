# identity/

This package owns durable device identity, pairing, access and refresh
credentials, device proof verification, passkeys, recovery codes, and
revocation. The protocol contract is
[remote-access.md](../../docs/specs/remote-access.md); recoverable refresh is
described in
[session-renewal.md](../../docs/architecture/session-renewal.md).

## Boundaries

The dependency direction is `transport -> narrow interfaces -> identity ->
store`. Do not import `internal/transport` here. Transport authenticates and
classifies requests, this package decides whether credentials and durable rows
are valid, and `internal/app` enforces authorities that depend on RPC arguments.

Scope names are persistence and audit vocabulary here and transport policy in
`internal/transport/scopes.go`. Keep both sets aligned through
`TestScopeVocabularyMatchesIdentity` and the schema value-set tests. A shipped
`Reason.Code()` is stable wire vocabulary; update the frontend mirror and its
cross-language test when adding one.

## Session validity

Admission requires both a verified credential and a live durable session.
`Verify` handles each credential presentation. `Sessions.Live` is the cheap
per-RPC check and must be consulted rather than caching authorization from the
WebSocket upgrade. A session is live only while both its session row and device
row remain unrevoked.

Preserve these concurrency rules:

- Write revocation durably before invalidating the live-session cache, then
  close registered connections.
- Move the cache generation on every device revocation, including an empty
  session sweep, so a concurrent slow read cannot reinstall stale state.
- Close connections again on repeated revocation. The durable row and the
  connection registry can have changed independently.
- Keep signature verification structurally ahead of time-window diagnosis.
  `verifiedClaims` and `verifiedDeviceProof` encode this ordering.

Credential-producing store calls are guarded by
`TestEveryCredentialProducingCallGoesThroughAChokepoint`. A new caller needs a
device-revocation gate at the write boundary, not only an update to the test's
allow-list.

## Pairing and local sessions

Pairing is owner-confirmed proof of possession. The redeeming device supplies
its key before a pending session is created. Pending credentials admit no
request until confirmation. A refused redemption consumes its link. Re-pairing
a known key may adopt the existing device row, but cannot change its
`ProofKind`, restore a revoked device, or cross users.

Grant sets are fixed when a pairing link is minted and copied to the session.
Reject unknown access levels. Certificate fingerprints are carried from
the serving listener; identity does not choose or verify the certificate.

Boot removes every unconfirmed invitation and pending session before publishing
the session service. Confirmed sessions and refresh credentials survive.
`ReasonPendingConfirmation` does not consume the refresh secret or create an
audit refusal, so polling can continue until the owner decides.

The page session is a loopback-only, non-renewable credential minted for the
current backend launch. It does not replace the launch credential or permit an
off-host request.

## Refresh, proof, and recovery

Recoverable refresh rotates a client-chosen successor atomically. A retry may
recover only from the spent predecessor's matching receipt while that successor
is still live. A different successor is reuse and revokes the session family;
an already-used proposed successor is terminal. Check confirmation, revocation,
and device possession before either recovery or reuse handling.

Device proof is a compact ES256 JWS bound to HTTP method and path. The durable
device row selects signed-key or bearer verification; never infer the accepted
proof kind from the submitted value. Keep parsing limits, fixed-width P-256
coordinates, freshness checks, and the bounded replay guard intact. The
transport/app presentation hook must apply proof checks to every session-bearing
HTTP route, including ticket minting.

Binding class is enforced at presentation time against the actual peer. It is
not a grant and must not be inferred from listener mode or request headers.
Backend identity in credentials prevents a valid credential from one backend
being accepted by another.

Passkeys add proof of device possession to the existing pairing and session model.
Challenges are single-use, purpose-bound, backend-bound, and short-lived.
Recovery codes are stored as digests, returned only at creation, and consumed
atomically. Recovery may restore access through the documented recovery flow;
it must not silently weaken device proof or session binding.

Personal-device invitations and ordinary sharing are separate protocols.
Authorize from the stored invitation purpose and recipient-key restriction,
not the display hint supplied by a client.

## Validation

Exercise state transitions and races, not only steady states: pending to
confirmed, live to revoked, revoke during cache fill, repeated revocation,
refresh retry after a lost response, competing successors, replayed proofs,
restore then re-pair, and process restart. Use temporary stores and deterministic
clocks; repository tests must never use real provider homes or binaries.
