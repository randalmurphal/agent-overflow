# Computer pairing

Desktop setup starts on the computer granting access: **Remote access →
Allow device access → Allow a device to connect → Another computer**. On the other desktop,
**Remote access → Connect to a computer** discovers that open window. A hostname or
HTTPS address is the fallback; no invitation needs to cross clipboards.
The owner compares the six digits on both screens before approving.
Phone QR codes and explicit invitation links use the same approval steps.
The default personal-device flow joins your devices as described below.

## Discovery is a suggestion

`internal/nearby` advertises display name, backend ID and HTTPS address only
while the owner has opened a two-minute window. Its private IPv4 DNS-SD scan
is bounded. `internal/tailnet` adds online peer candidates from AO's own
Tailscale node; it does not assume a device-name prefix. The attached-backend
manager probes candidates without credentials, hides self/already saved
computers, and deduplicates reachable addresses by backend ID, preferring LAN.
None of those public fields authorizes access or supplies trusted certificate
information.

AO's node handles outgoing tailnet connections as well as incoming ones.
`deviceclient.WithDialContext` carries that path through pairing, saved
connections, alternate routes and address repair, with TLS verification
unchanged. Other destinations use the OS network. A standalone `--frontend`
controller discovers LAN devices; it can use a typed tailnet address through
OS Tailscale but has no embedded node to enumerate tailnet peers.

Discovery is optional: multicast may be filtered, or tailnet policy may hide
peers. The address fallback uses the identical approval exchange. Reachability
still depends on the selected network and its firewall/ACLs.

## Trust before enrollment

The existing invitation digits are returned by the host, so repeating them
through an initially untrusted connection would not authenticate that
connection. `internal/pairbootstrap` instead implements a committed P-256
Diffie-Hellman exchange with fresh nonces and independently derived comparison
digits. The client commits before the host chooses its response, freezes that
response before revealing, and includes the full versioned transcript in key
derivation. The device invitation, including endpoint and certificate trust,
is AES-GCM encrypted under the exchange key. Its bytes never appear in
discovery, a redirect, or an unauthenticated plaintext response.

The credential-free `/auth/pair/nearby` endpoint permits just public info and
that bounded exchange, behind the existing host/origin/body/rate guards.
Its client accepts an initially untrusted TLS certificate only for this
bootstrap; normal invitation redemption then uses the invitation's real pin.
The resulting device session remains inert until the owner approves the
independently matching digits. There is no new session type or alternate
authorization path. One requester may occupy a window; replacing it requires
an explicit owner action, preventing silent replacement or unlimited guesses.

Owner connection loss, cancellation and listener-affecting network settings
changes retire the window and its unfinished invitation. Expiry is the
bootstrap book's own timer, which cancels the invitation; the LAN
advertisement lingers only until the dialog closes, and an expired window
answers `open:false`, which discovery discards. Startup retires unfinished pairings in one
store transaction because an ephemeral comparison cannot survive a restart.
Confirmed devices retain their existing sessions and renewal policy.

## Own devices

Personal pairing grants full app access across the joined devices. Existing
personal groups merge; each execution host and frontend establishes its own
direct connections. This is access to each host's projects and conversations,
not replication of databases, files, accounts, or frontend preferences. No
initial computer, phone, or window must remain online to carry other connections.
Agent command access remains a separate, directional opt-in.

An ordinary invitation, including an older full-access pairing or a limited
share, does not become personal membership. Joining those installations requires
an explicit personal pairing. `own-devices.v1` advertises the new protocol;
legacy hosts retain ordinary connections without participating in introductions.

The durable catalog contains public device-key thumbprints, serving backend IDs,
names, device classes, verified routes, and membership generations. It is bounded
to 128 records including removal records. Credentials and private keys never
travel in it. Each installation retains its own device key; every destination
issues an independent session tied to that key and its admitted generation.
Forwarded metadata cannot replace an established host's endpoint trust; direct
self-registration updates that host's current metadata.

After approval, members exchange catalogs and issue single-use introductions
restricted to the recipient's key. The destination requires an active sponsor
and recipient in the admitted generation. Reciprocal invitations travel over
the already authenticated connection. A phone connected to two hosts can also
relay those hosts' key-restricted invitations; the recipient checks the target
against its active catalog and exact endpoint/certificate trust. The phone never
copies its own credentials to a host or remains a required relay.

Joining an execution host enables its LAN listener through the normal settings
and Windows relay lifecycle. Existing Tailscale settings are preserved; remote
return paths still need that host's Tailscale node configured. Introductions
use actual advertised routes and never fall back to a loopback address. Hosts
reconcile without a window; frontend-only desktops keep a public catalog and
direct sessions without starting an execution backend. Offline connections retry
with bounded work, and enrollment failures appear on the affected connection.

Removing a saved connection is a local exclusion: automatic introductions cannot
restore it until explicit pairing clears the exclusion. Revoking a personal
device removes its group membership. Removal wins over an equally old active
record and survives restart; only a new explicit approval can admit a later
generation. Reachable members propagate removal and invalidate affected group
sessions. An offline host cannot enforce a removal it has not yet received.
Ordinary independent shares are not absorbed into these group rules.

## Windows boundary

The native launcher reports reachable physical Windows LAN endpoints over its
existing authenticated owner RPC bridge. For default WSL NAT it forwards raw
TLS to the backend's **non-loopback** WSL interface, so clients remain remote
at the authorization boundary. Mirrored networking uses its exposed address
directly. QR addresses, discovery and authenticated alternate routes use these
published endpoints, never the inaccessible WSL NAT address once the native
bridge is known. Owner/configuration generations discard stale reports.
See [the native network guide](../../internal/nativenetwork/AGENTS.md) for the
relay bounds and lifecycle. Windows Firewall remains under the user's control.

## Evidence

The composed browser case in `e2e/tests/computer-address-pairing.spec.ts` runs
separate real host/client instances: address entry, independent matching digits,
rejection, retry, approval, history access, and self/duplicate refusal. Package
tests cover transcript tampering, cancellation races, restart retirement,
untrusted discovery responses, real pinned TLS through forwarding and dial
selection, and native owner/configuration/scan lifetimes. A local Tailscale
control/DERP fixture verifies discovery and dialing without OS Tailscale.
Own-device tests compose real TLS listeners, device proofs, sessions and RPCs
across three execution hosts and a frontend-only controller, including loss of
the original host. Additional cases cover merging two existing groups, a phone
introducing two hosts, offline enrollment retry, upgrading an ordinary profile
only through approved membership, local exclusions across restart, stale removal
records, and headless worker shutdown. Identity tests cover key restrictions,
lost-reply replacement, generation changes, and revocation. LAN advertisement
uses deterministic private addresses translated to local test listeners; it
exercises listener rebinds without depending on the developer's network.

Physical Windows firewall/NAT behavior and actual remote Wi-Fi/cellular paths
still require device checks; a cross-build or loopback fixture cannot prove them.
