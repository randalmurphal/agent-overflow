# identity/

The session core: mints session credentials, verifies a presentation,
answers the per-RPC liveness question, and revokes. Spec:
[docs/specs/remote-access.md](../../docs/specs/remote-access.md) §3 and §4.

Rows live in `internal/store` (migrations v75 and v76). Enforcement of what
a scope PERMITS is not here: `internal/transport` gates every RPC and every
event channel, and `internal/app` rechecks the authorities that depend on a
call's ARGUMENTS. All three read the grant set through one hook,
`app.SessionScopes`, which goes through `Sessions.Live` — so a revoked
session refuses on its next call rather than on a watchdog tick.

The grantable scope names are declared here as the audit and persistence
vocabulary, and RESTATED in `internal/transport/scopes.go` — which adds
`host` and the observe/execute/host tier each resolves to — because
transport must not import this package. `internal/app` imports both, so
`TestScopeVocabularyMatchesIdentity` there is what keeps one spelling,
failing in either direction.

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

## Pairing: the confirmation is the gate, and it is a predicate

`pairing.go` runs the seven steps of spec §4 in one place. The shape worth
keeping:

- **The redeeming device generates its keypair FIRST** and presents the
  thumbprint as part of redemption. Proof-of-possession is universal —
  there is no path that mints a session for a device that proved nothing,
  so no later phase has to add one.
- **Redemption returns the real credential immediately, and it admits
  nothing.** The session row is minted with `activated_at` unset, and
  `store.Session.Live` requires it. So the pending state costs no poll
  route, no poll secret, and no second credential: the device holds what
  it will use, retries until the owner confirms, and every presentation
  path refuses it in the meantime through the predicate every one of them
  already runs.
- **The verification number is derived, never stored.** HMAC over the
  active signing secret of (domain ‖ backend id ‖ link id ‖ key
  thumbprint), reduced to six digits. It is therefore a function of the
  key the device actually presented: a device that redeemed with a
  different key cannot display a number the owner's screen will match.
  Leading zeros are preserved, because a five-digit number on one screen
  and six on the other is a confirmation nobody completes.
- **A refused redemption cancels the link it spent.** `RedeemPairing`
  never releases a token back for a second attempt; the owner mints
  another. A link that could be retried is a link a second reader can
  race for.
- **Re-pairing a known key ADOPTS its device row** (`key_thumbprint` is
  uniquely indexed), so a device that pairs twice does not accumulate
  rows. A revoked device, or one belonging to another user, is refused
  with `key_mismatch` rather than re-admitted.
- **`PairingPayload.CertFingerprint` is reserved and unread.** Phase 5
  fills it when TLS exists. It is in the shape now so the QR a device
  scanned before that phase is not a payload version older clients cannot
  parse.

## Rotating refresh, and what "reuse" costs

`refresh.go`. Each renewal issues a new refresh secret and spends its
predecessor; presenting a SPENT secret revokes the whole family — the
session, every socket carrying it, and every outstanding secret in the
chain — and writes `refresh-reuse-detected`. That is the leaked-copy
detector, and it is deliberately unable to tell the copy from the
original, which is why BOTH stop.

The ordering inside `Refresh` is the contract:

1. resolve the secret (unknown / spent / lapsed each answer differently);
2. judge session liveness and the device-key proof;
3. only then CAS-consume, and treat a lost CAS as reuse.

Checks before consumption, so a client that presents the right secret with
a wrong or missing proof can correct itself. Consuming first would sign a
device out for one mistyped header. The cost is that a client must not
retry a renewal whose response it never read: it cannot distinguish that
from a copy, and neither can this package.

Refresh binds to the device key on EVERY listener. A bare bearer refresh
is `missing_proof` even on loopback, because a credential that could
self-renew from possession alone makes rotation bookkeeping rather than a
control.

`policy.go` holds the windows in one table. Binding class decides before
device class: every `loopback-only` session gets a short access window and
NO refresh secret at all, because it is re-minted at boot and one that
renewed itself would outlive the process it was minted to serve. The
browser class gets the shortest renewable pair of the rest — it is the one
class with a script-execution surface. Passkey re-auth on renewal is phase
5; rotation is now.

`issueFor` is the only function in this package that builds a `TokenSet`.
Every issuance path — pairing, renewal, the local channel — goes through
it, so a policy change cannot reach some callers and miss others.

## The local page channel

`local.go`. At boot the backend mints a `loopback-only` session for
ITSELF, and the bootstrap exchange hands it to the embedded webview, the
`--connect` client, and the WSL launcher relay alike.

It exists so that "the request arrived over loopback" stops being a trust
basis: a same-host relay carrying a remote peer's traffic is identical at
the socket, and a credential this backend minted and the relay forwards is
not. Nothing here removes the launch credential — it still authorizes
every request — but a local connection now also NAMES a session, which is
what gives revocation and attribution something to reach.

Idempotent in both halves: the DEVICE is resolved by `devices.channel`
(one row forever, `EnsureChannelDevice`), and the SESSION is extended and
re-signed while it is live, re-minted when it is not. A revoked channel
device is an error rather than a re-mint — re-minting around it would make
the one revocation a host-local surface can perform unenforceable.

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
untouched by this package and **still authorizes every request today**.

What changed in wave 5b is that a request may now ALSO name a session.
`internal/app/app_identity.go` supplies the transport's hooks: a request
carrying no session credential proceeds and names none (every
launch-credential client — the harness CLI, the e2e rig, a `--connect`
stub), and one carrying a credential this package refuses is refused
outright, rather than silently downgrading to an unattributed connection.
Phase 3 is what makes a session credential REQUIRED and enforces scopes
per method; until then the launch credential's rules
(`internal/transport/AGENTS.md` § Credentials and refusal shapes) are
unchanged.

`CheckDeviceProof` is exported for that hook and runs on every request
that names a session, not only on renewal. A session whose device enrolled
a key presents it everywhere, so no route added later can be a way around
it — including `/auth/ticket`, whose whole authentication is that hook.

## Backend binding

Every MAC covers a domain separator, the backend id, and the payload. A
database restored under a re-minted backend id therefore refuses every
session it imported, which is the re-pairing recovery the spec already
states (§12) rather than a second mechanism. The backend id is captured
once at `NewSessions`, so a restore takes effect on the next boot.
