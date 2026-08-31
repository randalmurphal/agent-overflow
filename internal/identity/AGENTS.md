# identity/

The session core: mints session credentials, verifies a presentation,
answers the per-RPC liveness question, and revokes. Spec:
[docs/specs/remote-access.md](../../docs/specs/remote-access.md) §3 and §4.

Rows live in `internal/store` (migration v75). Enforcement of what a scope
PERMITS is phase 3 and is not here.

## Layering, and why it is one-directional

```
internal/transport  →  (narrow interfaces)  →  internal/identity  →  internal/store
```

`internal/transport` must NOT import this package, and this package must
NOT import transport. Transport is deliberately store-free (it takes the
backend id as an injected `func`, never a `*store.Store`), and importing
this package would pull the store in behind it.

The two meet through interfaces each side declares for itself:
`LiveConns` here, satisfied structurally by the transport's connection
registry; the session hook on the transport `Config`, satisfied by a
closure over a `*Sessions`. Neither file names the other's type.

That direction is also what makes the value-set cross-check possible: this
package can import `internal/store`, so
`TestDeclaredValueSetsMatchTheSchemaChecks` lives here and pins the Go
constants against the real CHECK constraints in both directions. The
reverse arrangement would cycle.

## Both halves, always

A presentation is admitted only when **the signed claims verify AND the
database row is live**. Neither alone:

- a valid signature over a revoked session admits nothing, because the row
  is authoritative;
- a live row nobody can produce a signature for admits nobody, because
  the MAC is what makes a session id unforgeable and what makes every
  credential minted under a dropped key uniformly dead.

`Verify` is the per-PRESENTATION path (an HTTP request, a WS upgrade, a
ticket redemption): parse, resolve key, check MAC, check window, check
row. `Live` is the per-RPC path: one read-locked map lookup and an integer
comparison, no signature work, no round trip. **No call may authorize from
state captured at upgrade time** — that is the whole reason `Live` exists
instead of a flag on the connection.

## The refusal ordering is structural, not documented

`verifiedClaims` (claims.go) is an unexported type whose only constructor
is the success path of the MAC check, and `withinWindow` is a method ON
that type. So there is no arrangement of calls — including one a later
edit introduces — in which a proof that failed to verify is reported as a
clock problem.

That specific misreport is the failure the shape exists to prevent:
"check automatic date & time on both devices" is the wrong instruction for
a credential this backend did not sign, and it is the instruction the
`outside_time_window` code carries. Do not replace the type with a boolean
and a comment.

`Reason` (reason.go) is a closed typed set with stable wire codes,
contiguous ordinals, and a gate test that fails on a constant with no code
or a code with no constant. A code is stable forever once shipped: an
older client bundle may still be mapping it to a hint. The frontend mirror
is `frontend/src/lib/transport/authReason.ts`, and the two are pinned
against each other by `authReason.test.ts`.

## Revocation order

`RevokeSession` does three things and the order is the mechanism:

1. write the database row, so every later read — including one already in
   flight — sees it dead;
2. drop the fast-path entry and bump the generation, at the same instant;
3. force-close the live connections, synchronously.

Reversing 1 and 2 lets a concurrent `Live()` miss re-populate the fast
path from a row that had not been written yet. Doing 3 first closes a
socket that can immediately reconnect on a credential still valid.

**The generation counter is not decoration.** A `Live()` miss captures the
generation before its row read and installs its result only if the
generation has not moved. Without it, a read that started before a
revocation could finish after it and put the dead session back in the
fast path, where it would stay until expiry.

A second revocation reports `moved == false` and **still closes
connections**: a connection that survived the first revocation is exactly
the case worth closing again.

## Bounded by construction

- The fast path holds one entry per live session, bounded by the devices a
  person actually paired. Expired entries are swept when the map crosses
  `liveSweepThreshold`, so they cannot accumulate behind live ones.
- The signing-key cache is bounded by rows in `signing_keys`, which only
  this package inserts. A presentation naming an unknown key is refused
  and caches nothing, so an unknown id cannot grow the map.
- `auth_audit` is bounded by the store's insert-order prune.

## Recovery codes

- 100 bits from a 32-character alphabet with the ambiguous letters
  removed, hashed with a plain SHA-256. A slow KDF would buy nothing (the
  input is not human-chosen) and would cost the single indexed lookup that
  makes consumption atomic.
- `newRecoveryCode` returns BOTH forms — the dashed one a person reads and
  the normalized one that gets hashed — because a single return value
  would let a caller hash the dashed string and mint codes that verify
  against nothing, with no path that would notice.
- A replayed code and a code that never existed both answer
  `ErrRecoveryCodeRefused`. The backend genuinely cannot tell them apart.
- `Bootstrap` mints codes only when the account has NO recovery-code rows
  at all, spent ones included. Keying on "no unspent rows" would silently
  hand someone who used their last code a set they were never shown.

## The credential this does NOT replace (yet)

`internal/transport`'s `Credential` is the per-launch page token: one per
process, not persisted, no device, no scopes, no revocation. It is
untouched by this package and still authorizes every request today. Phase
3 migrates the wire onto this core; until then nothing routes through
`Sessions`, and the launch credential's rules
(`internal/transport/AGENTS.md` § Credentials and refusal shapes) are
unchanged.

## Backend binding

Every MAC covers a domain separator, the backend id, and the payload. A
database restored under a re-minted backend id therefore refuses every
session it imported, which is the re-pairing recovery the spec already
states (§12) rather than a second mechanism. The backend id is captured
once at `NewSessions`, so a restore takes effect on the next boot.
