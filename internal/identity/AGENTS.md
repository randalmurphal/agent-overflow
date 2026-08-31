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
vocabulary, and RESTATED in `internal/transport/scopes.go` — which adds the
two values that are method PROPERTIES rather than grants (`host`, and
`session`, the floor any live session passes) plus the tier each name
resolves to — because transport must not import this package. `internal/app` imports both, so
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

### And "the row is live" is itself two rows

**Revocation is absolute** (spec §2, owner ruling 2026-08-31): a session is
live only while its own row AND its device's row are both unrevoked. Every
consult reads that conjunction — the per-RPC gate, `Verify`, refresh
rotation, `/auth/ticket`, the connection's interval re-check — because all
five reach it through `Sessions.Live`.

It costs nothing per call, and the shape is why. `internal/store`'s
`sessionSelect` JOINs the device onto every session read and
`store.Session.Live` folds `DeviceRevokedAt` in, so the fast path gained a
second integer comparison — no second lookup, no second round trip, no
device cache to keep coherent. A revoked-device SET consulted per call, or
a liveness bit resolved separately, would both have to be kept in sync with
rows they do not travel with.

The entry a fast-path HIT reads carries the device stamp from install time.
Three things keep that honest:

- a device revocation sweeps every un-revoked session the device holds and
  forgets each one, so no entry for it survives;
- a device revocation moves the generation **unconditionally**, so a slow
  path already in flight — one holding a joined row read before the
  revocation committed — declines to install it;
- a session row that appears AFTER the sweep has no entry at all, so its
  first consult is a slow path and the row it reads carries the revocation.

That last case is what the incident turned on, and it is why the
enforcement is at the consult rather than at the mint. Mint-time refusals
are real (below) but they are hygiene: a revocation can always land the
instant after one.

`TestEveryCredentialProducingCallGoesThroughAChokepoint` is the class gate.
Four calls bring a credential into existence or keep one alive —
`store.CreateSession`, `store.ActivateSession`, `store.ExtendSession`,
`signClaims` — and each carries the device gate at the point of the write,
so a mint path built from them inherits it. The test fails when one is
called from somewhere new, which is the moment to say what stops the new
caller producing a credential for a revoked device. Widening its list is
not the answer on its own.

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

`RevokeDevice` honors that same doctrine, and used not to. The store's
already-revoked early return meant a re-revoke touched no session, so a
session that slipped past the first sweep was unreachable through the
device surface forever — a paired browser kept full access and every later
revoke was a silent no-op (incident 2026-08-31). It now re-sweeps, and this
side forgets what came back, closes its sockets, and **moves the generation
even when nothing came back at all**: what changed is the device row, and a
`Live()` slow path in flight may be holding a joined copy of it. A loop of
per-session `forget` calls would leave exactly the zero-session case — the
straggler case — unguarded, which is why `forgetAll` exists and bumps once
for the whole set.

It reports an `identity.DeviceRevocation` rather than a bare id list, so a
surface can say "revoked, 2 sessions ended, 1 connection closed" and
"already revoked, nothing was live" as the different answers they are.

`RestoreDevice` and `ForgetDevice` are the two ways OUT of a revoked
device row, and they answer opposite questions. Restoring says "that is
still my device": it re-admits the KEY to pairing (the remedy the
revoked-key redemption refusal names) and moves no credential. Forgetting
says the device is nothing to this backend any more: it DELETES the row,
and the schema cascades its sessions and their refresh secrets with it.

Both refuse to hand back a credential — the way to one is still an
owner-minted link plus the verification number — and `ForgetDevice`
additionally refuses an un-revoked device (`ErrDeviceNotRevoked`).
Revoking is what ENDS access and what closes live sockets; deleting the
row first would remove the only handle on a device that still holds
credentials. Revoke, then forget.

Two consequences worth stating rather than discovering:

- **The key becomes free.** `idx_devices_key_thumbprint` is unique over
  the surviving rows, so a forgotten device's key may enroll again. That
  is intended: re-enrolment still costs a link the owner minted and a
  number the owner compared, so nothing returns unwatched.
- **The audit log outlives every row it names.** `auth_audit.device_id`
  is a plain column with no foreign key, so the revoke and the forget
  both stay in the log after the device is gone. That is the point of
  keeping attribution out of the cascade — the row being deleted is
  exactly when the record matters.

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

- **The redeeming device generates its keypair FIRST** and presents it as
  part of redemption. Proof-of-possession is universal — there is no path
  that mints a session for a device that proved nothing, so no later phase
  has to add one. What it presents decides its `ProofKind` for the life of
  the row: a SIGNED proof over the redemption enrolls it `key`, and the
  thumbprint recorded is derived from a key the device just demonstrated
  it holds; a bare identifier enrolls it `bearer`. A device may only make
  itself weaker by choosing the second, which is why sniffing the shape is
  safe at enrollment and refused everywhere else — there is no stronger row
  to downgrade from yet.
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
  with `key_mismatch` rather than re-admitted. Adoption may never change
  the row's `ProofKind`: this is the one path that writes device rows, so
  without that rule a key-bound device's requirement could be undone by
  redeeming a fresh link with its thumbprint as a bare string
  (`proof_downgraded`).
- **The grant set is decided at MINT and copied onto the session.**
  `PairingRequest.Scopes` is the link's set, `mintPendingSession` copies
  it onto the row, and nothing edits it afterwards — so how much a device
  may do is a property of the link somebody handed it. The access surface
  names its two choices through `PairingAccess`: `full` is `Scopes`
  entire, `view-only` is `ObserveScopes`, and an undeclared level errors
  rather than falling back to full, because a level nobody declared must
  never widen a link somebody meant to narrow. `ObserveScopes` is exactly
  transport's observe tier, which this package cannot check and
  `internal/app` does (`TestObserveScopesAreTheObserveTier`). Narrowing a
  device that is already paired is a new link, and its old session keeps
  what it holds until it is revoked.
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

It takes the DEVICE ROW rather than a policy, and derives the policy
itself. Every caller built the same `PolicyFor(device.Class,
session.BindingClass)` anyway, and taking the row means a caller cannot
pass a policy from one device with a session from another — nor issue
without having read a device at all, which is what lets the device half of
the conjunction be refused here (spec §2). `Mint` is the matching
chokepoint for the session ROW: it is the only caller of
`store.CreateSession`, and the device predicate lives inside that INSERT.

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

Phase 3 has since made naming a session REQUIRED where its absence
cannot be tolerated, and the boundary it drew is the PEER rather than
the route: a `/ws` upgrade from a peer that is not on this machine must
name a live session, because a connection with no session id is one
`CloseSession` has no id to reach and the per-RPC gate has no grant set
to read. The launch credential is unchanged and still authorizes every
request; what it no longer does is stand alone off-host. The rules and
the admission matrix live in `internal/transport/AGENTS.md`
(§ Credentials and refusal shapes).

`CheckDeviceProof` is exported for that hook and runs on every request
that names a session, not only on renewal. A session whose device enrolled
a key presents it everywhere, so no route added later can be a way around
it — including `/auth/ticket`, whose whole authentication is that hook.

## The device proof, and why the row decides

`deviceproof.go`. Phase 5 replaced the thumbprint STRING with an ES256
compact JWS signed over the request (spec §4). A string copied out of a
page's storage was as good as the key it named, so the old binding bought
attribution and nothing more; a proof is minted per call, so a copied
credential is no longer sufficient on any path that binds to a device key.

- **The row decides what a valid presentation IS, never the
  presentation.** `checkProofAgainstDevice` switches on the device's
  `ProofKind` before it looks at what arrived. That one sentence is the
  downgrade rule: a `key` device sending a bare thumbprint is
  `proof_downgraded`, not a fallback to the weaker branch. Sniffing the
  shape and taking whichever path it fits is exactly the hole this
  replaces.
- **`bearer` is not a weaker option, it is a different device.** A
  plain-HTTP LAN page is not a secure context, so `crypto.subtle` does not
  exist there at all and no key can be generated. Spec §15 constraint 6
  states there is deliberately no LAN-HTTP proof path; `ProofBearer` is how
  that is recorded rather than pretended away, and such a device keeps
  exactly the behavior it had before phase 5.
- **The refusal ordering is structural, the same way claims.go's is.**
  `verifiedDeviceProof` is constructed only by `checkProofSignature`'s
  success path, and `withinWindow` and `boundTo` are methods on it. So a
  proof that did not verify can never be reported as a clock problem —
  `outside_time_window` carries the "check automatic date & time" hint, and
  showing that for a proof this backend's device never signed sends a
  person to fix the wrong thing.
- **One ordering, two callers.** `admitProof` holds signature → binding →
  freshness → replay, because a request against an enrolled row
  (`verifyDeviceProof`) and an enrollment with no row yet (`enrollmentFor`)
  would otherwise be free to drift apart. What differs between them —
  whether a stored thumbprint exists to compare against — sits outside that
  function on both sides.
- **Replay is spent LAST**, so a presentation that could never be admitted
  cannot consume the identifier of one that would be.
- **Enrollment verifies before the link is spent.** A signing bug on the
  device must not cost the link somebody minted, or a spent link and a
  broken client would be indistinguishable to the person holding the phone.
- **`htp` is the PATH, not RFC 9449's full URI**, and `iatMs` is
  milliseconds rather than JWT seconds. One backend answers on loopback, a
  LAN address, the WSL relay and a `--connect` proxy at once, and a client
  cannot predict which authority its request is seen under. The field names
  and `typ` are ours precisely so a reader who knows DPoP does not read
  them as the RFC's and be wrong.
- **The replay guard is two rotating maps** (`proofreplay.go`), bounded by
  the window with a hard cap, per spec §14. An entry lives between one and
  two windows — always at least the window a proof is acceptable across,
  which is the only property correctness needs — and a rotation frees a
  whole generation with no scan. At the cap it rotates EARLY rather than
  refusing: refusing would turn a burst into a sign-out for every real
  device, and reaching the cap requires thousands of proofs that already
  verified under a device's private key. A restart clears it; the spec
  accepts that and says why.
- **Signature work is bounded to establishment.** Per HTTP request and per
  WS upgrade, never per frame — `Sessions.Live`, the per-RPC path, does no
  signature work at all.

`TestProofVectorFromRealWebCrypto` is the one test that would fail if Go
and WebCrypto disagreed: every other test signs with `crypto/ecdsa`, so
they would all still pass. It pins a proof a real Chromium minted,
covering the two things that are easy to get wrong — WebCrypto emits a
64-byte r‖s signature rather than ASN.1 (`ecdsa.VerifyASN1` answers a
silent false on those bytes), and its exported JWK carries `ext` and
`key_ops` members that the RFC 7638 thumbprint must not include.

## Binding class is compared at PRESENTATION, and only there

A session's `BindingClass` is a property of the CREDENTIAL, not of the
socket it arrives on, and `loopback-only` is the one class with a
listener restriction: it is the posture this backend mints for ITSELF
(`local.go`), so a copy of one must carry no reach at all. Wave 6d2
turned that from a recorded fact into an enforced one.

**The comparison lives in `internal/app`'s `SessionForRequest`
(`bindingAdmitsPeer`), and nowhere else.** That hook is the one place in
the tree holding both a session row and a peer address: this package
never sees a request, and `internal/transport` cannot name a binding
class without importing this package. Every presentation path already
runs through it — the `/ws` upgrade's non-ticket arm, the manifest's
session fallback, `/auth/ticket` — so a route added later inherits the
rule instead of having to restate it. A route that resolved a session
some other way would be the way around it; there is no second resolver.

- **Peer locality is `loopback.PeerAddress`**, the same kernel-reported
  predicate the transport judges every other locality question by, and it
  fails closed on an address it cannot read.
- **The refusal resolves NO SESSION rather than refusing the request.**
  The credential is genuine and live; what it is not is presentable on
  this listener. So the request carries no session and the sessionless
  rules decide — off-host that is the `/ws` upgrade's unfingerprintable
  404, and a 404 from `/auth/ticket` because there is nothing to bind a
  ticket to.
- **The other end is the bootstrap exchange**, which plants the local
  channel's cookie only for a loopback peer
  (`internal/transport/server.go`). A page is never handed a credential
  that would be refused the moment it used one. The LAN share URL still
  loads — it gets the page cookie and the SPA's pairing prompt — it just
  arrives with no local channel.
- **Consequence worth knowing:** an off-host relay can no longer borrow
  the backend's local channel. `internal/relaysession` fetches the
  session cookie out of an authenticated bootstrap exchange; from another
  host that exchange now carries none, so a cross-host `--connect` stub
  needs a paired device session rather than the backend's own.

A class added to `BindingClasses` needs an answer in `bindingAdmitsPeer`
in the same change. Everything that is not `loopback-only` is admitted
anywhere today, which is what pairing buys.

## Backend binding

Every MAC covers a domain separator, the backend id, and the payload. A
database restored under a re-minted backend id therefore refuses every
session it imported, which is the re-pairing recovery the spec already
states (§12) rather than a second mechanism. The backend id is captured
once at `NewSessions`, so a restore takes effect on the next boot.
