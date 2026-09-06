# Routes to a connected computer

Implementation contract for route selection in
[connected-computers.md](../specs/connected-computers.md). Go carriers and native
Android share this policy; older hosts and APKs keep their original endpoint.
The offline computer controls also support verified address repair.

A computer's backend ID names its projects, threads, settings and device session.
An endpoint is only a way to reach that computer. Selecting a different endpoint
must never create another sidebar computer, pair another device, clear a replica,
change a thread's owner, or mint a different request ID.

## Trust and discovery

A reachable, trusted backend advertises its available endpoints in its bootstrap
manifest. Advertisements contain normalized HTTPS origins and, for its private
listener, the self-signed certificate fingerprint. They contain no page ticket,
credential, refresh secret, or device proof. LAN addresses are advertised only
when the main listener accepts LAN connections; tailnet HTTPS is advertised only
when that listener is available. Never publish the backend's loopback address as
a route for other computers.

The client keeps a bounded set per computer. The original paired endpoint is a
route too. New addresses learned through a trusted connection remain candidates
until a credential-free health request verifies BOTH their TLS trust and backend
ID. An address alone, including a familiar private IP, never buys a credential.
Health requests reject redirects and are bounded in time and response size.

A host without route advertisements keeps working through its paired endpoint.
Missing advertisements from an older build must not erase already trusted
addresses. A changed certificate is not accepted through the failed route; it
requires another trusted route or explicit renewed trust from the owner.

A manual address repair uses the same verification before persisting or dialing
with credentials. It must preserve the existing pairing. Address changes cannot
be discovered from a completely unreachable host without a discovery mechanism
or another working route; never disguise that case as a revoked device.

`RepairCandidates` and its TS counterpart reuse a saved private certificate pin
at a newly entered address, or WebPKI for the same saved hostname at a different
port. They do not trust a new public domain solely because its health response
claims a known backend ID. A replacement pin advertised for the original origin
supersedes its old pin during repair too. Recheck current trust and the pairing
after verification, then persist before reporting success. No health request
presents a credential, and no repair changes the original pairing endpoint.

## Selection and failure

Keep the last working route while it works. Do not poll healthy computers or
switch a live socket just because another route becomes available. After a
connection failure, coalesce the computer's next selection attempt. Candidate
health checks may race with bounded concurrency and an overall deadline; cancel
losers and keep only the successful route. This work is independent per computer.

Each computer retains at most four advertised alternatives plus its original
pairing address. A selection has a two-second deadline and a short retry floor;
health bodies are capped at 64 KiB. Native checks also share eight bridge slots
and a queue capped at 32, so simultaneous reconnects leave room for app traffic.
The last-working address is a hint that must be verified again after reopening.

Selection is separate from the request. A request that might have crossed the
wire is never automatically replayed on another route. A failed request reports
its outcome through the existing transport; the next connection selects a route
and presents fresh proofs/tickets. Recoverable renewal retries the SAME saved
operation on the newly verified route. Command execution, message sending and
transfer acceptance retain their existing durable request identities and outcome
recovery contracts. Route changes do not authorize retrying an arbitrary RPC.

Every HTTP operation and socket upgrade takes one route snapshot. Its URL and
TLS verifier come from that same snapshot. Concurrent switching cannot combine
one address with another address's pin, or send a credential to the computer
previously assigned a reused LAN address. Route-selection health checks never
call credential renewal or acquire a profile transaction around network work.

## Frontend and platform boundaries

Native Android has a stable app origin and its own verified HTTP/socket bridge;
its per-computer credential, endpoints and certificate trust remain together.
Its route metadata is stored separately from credential renewal records, bound
to the pairing generation; writing an address can never overwrite a pending
refresh. Feature detection asks the APK's Network capability rather than guessing
from a bundle version. TLS work stays native; connection policy stays in TS.
Desktop and `--connect` carriers present a stable frontend origin and select the
remote route behind their existing HTTP/WebSocket transport boundary. Keep
route selection out of components and provider code.

An ordinary browser page is scoped to its serving origin by storage and browser
security policy. It cannot silently carry its origin-bound credentials to a new
origin. Do not weaken CSP, put credentials in navigation URLs, or confuse a page
navigation with native/desktop route selection. Browser access keeps its existing
same-origin behavior; the installed clients provide independent route selection.

The Computers surface represents one computer. Its connection details can show
the address in use and allow a verified address repair. Choosing a conversation
or execution host remains separate from choosing a network route.

## Validation

Cover LAN-only and tailnet-only reachability, blocked routes, address reuse by a
different backend, wrong certificates, redirects, concurrent selection, and a
network change during renewal/command acceptance. Check bounded selection and
cancellation with a healthy route beside a stalled one. Prove unrelated hosts,
frontend preferences, drafts, pairing state and request IDs survive route changes.
Run actual two-listener flows through the desktop carrier and Android bridge;
a successful health mock alone does not prove either transport switches correctly.
