# Recoverable session renewal

Session renewal remains recoverable when a successful response is lost. The
same operation can be retried after a connection loss or client/backend restart
without pairing again.

## Rotation protocol

Before sending a renewal, the client chooses and durably saves the next 32-byte
refresh secret beside its current secret. Every retry uses that same pair and a
fresh device proof.

The server atomically:

1. verifies and spends the current secret;
2. records the proposed successor's digest;
3. creates the successor refresh row; and
4. extends the session.

The server stores digests, not bearer secrets. Recovery is authorized by the
spent predecessor's recorded receipt. Possession of a live successor alone is
insufficient.

A repeated predecessor succeeds only when its recorded successor matches the
proposed successor and that successor remains unspent. The response returns the
known successor and a new access credential without creating another refresh
generation.

A different successor proves competing use of the predecessor and revokes the
session family. A request whose proposed successor already identifies another
refresh row is terminally refused as `malformed_proof`. A recognized operation
whose successor has already been spent is superseded and must not revoke newer
legitimate state. An unknown presented predecessor is `unknown_credential`
regardless of the proposed successor.

Confirmation, session and device revocation, binding, and device possession are
checked before recovery or reuse handling. The durable rotation transaction
checks session and device state again so concurrent revocation cannot be undone.
Expired spent predecessors remain stored while their successor is unspent,
which preserves the receipt needed for recovery.

## Capability negotiation

Current hosts advertise `X-AO-Refresh-Recovery: 1` on authentication responses.
A Go profile with unknown capability probes `/healthz` without credentials and
verifies backend identity. Browser and native clients issue GET to the POST-only
`/auth/token` route; a current host advertises the capability on its 405
response and the request cannot spend a secret.

Recoverable renewal uses `POST /auth/token/recover`, and the device proof binds
that exact path. Legacy renewal remains on `POST /auth/token`. Clients must not
send recovery fields to the legacy path or silently fall back after an
uncertain recoverable operation, because an older server may ignore unknown
JSON fields and consume the predecessor.

## Client persistence and concurrency

Go profiles use OS locks for short file transactions. Legacy renewal also holds
a per-computer lock for the bounded network request. Browser clients use the
existing Web Lock or storage lease and verify that the pending successor reached
storage before sending.

Clients compare the persisted credential generation before applying or clearing
a response. A late response cannot overwrite a newer renewal, re-pairing, or
removal. Redirects are rejected. Storage and transport failures leave the
pending operation intact so a later retry uses the same successor with a fresh
proof. Profile writes preserve unknown fields, and unknown refusal codes retain
the pairing because an older client cannot safely classify them as permanent.

Credential storage remains per attached computer. One host's refusal cannot
clear another host's state.

## Validation

Cover dropped successful responses, restart after server acceptance, identical
and competing retries, an already-spent successor, unknown predecessors,
revoked and unconfirmed devices, invalid proofs, persistence failure before
send, late responses after a newer generation, mixed client/server versions,
and the Go, browser, and native HTTP paths.
