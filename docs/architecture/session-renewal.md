# Recoverable session renewal

A lost successful renewal response must not require pairing again. The same
operation is recoverable across connection loss and client/backend restart.

The client chooses the next 32-byte refresh secret and saves it beside its
current secret **before** sending the renewal. Every retry of that operation
uses the same pair and a fresh device proof. The server atomically spends the
old secret, records only the next secret's digest, creates its successor, and
extends the session. Neither bearer secret is stored on the server.

A repeated old secret is recoverable only when its recorded successor matches
the proposed successor and that successor is still live. Device possession,
confirmation and revocation are checked before granting recovery or declaring
reuse. A different successor is reuse evidence and revokes the family. A
recognized operation whose successor has already been spent must not revoke a
newer legitimate session state. A still-live proposed successor can also
recover an operation after its predecessor has aged out of retention.

Recovery returns the known successor and a usable access credential. It does
not create another refresh generation. Access to the session and device is
checked again inside the durable transaction, so concurrent revocation cannot
be undone by renewal.

Clients learn `X-AO-Refresh-Recovery: 1` from their trusted host's auth
responses. A Go profile with unknown capability probes `/healthz` without
credentials and checks the backend identity. Browser/native clients instead
GET the POST-only `/auth/token` route: its existing shell CORS works on older
hosts too, and a GET cannot spend a secret. The 405 response advertises support
on current hosts. The recoverable
exchange uses **`/auth/token/recover`**, and its device proof binds that exact
path. A separate path is essential: old servers may ignore unknown JSON
fields on `/auth/token`. They cannot consume a recovery request sent to the
new path. Legacy clients and hosts keep `/auth/token`; an uncertain recoverable operation must
never silently fall back to legacy rotation. Profile writes preserve unknown
fields. Cross-context/profile locking protects choosing the successor, and
response application compares the current saved generation before writing or
clearing anything. A late reply cannot overwrite a later renewal or re-pairing.

Go profiles use OS locks for short file transactions. Legacy exchanges hold
a separate per-computer lock while waiting on the bounded network request;
renaming and removal remain available. Browser clients use the existing Web
Lock/storage lease and verify that a pending successor really reached storage.
Both clients reject auth redirects and compare the saved generation again
after a response. Retries preserve the same successor and mint a fresh proof.
A storage or HTTP failure leaves the pending operation intact. A newer
pairing/removal fences the old owner. Unknown profile fields survive writes.
Unknown future refusal codes preserve the pairing too; an older client cannot
infer permanent revocation from a reason it does not understand.

Validation covers dropped replies, process restart after acceptance,
parallel identical/different operations, revoked or unconfirmed devices, invalid
proofs, storage failure before send, late responses after a newer generation,
mixed versions, and the actual Go/browser/native HTTP seams. Native and browser
storage remain per computer; one host's refusal cannot clear another's state.
