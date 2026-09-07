# Computer pairing

Desktop setup starts on the computer granting access: **Remote access →
Allow device access → Allow a device to connect → Another computer**. On the other desktop,
**Remote access → Connect to a computer** discovers that open window. A hostname or
HTTPS address is the fallback; no invitation needs to cross clipboards.
The owner compares the six digits on both screens before approving.
Phone QR codes and explicit invitation links retain their existing flow.

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
derivation. The ordinary invitation, including endpoint and certificate trust,
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

Owner connection loss, expiry, cancellation and network changes retire the
window and unfinished invitation. Startup retires unfinished pairings in one
store transaction because an ephemeral comparison cannot survive a restart.
Confirmed devices retain their existing sessions and renewal policy.

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
Physical Windows firewall/NAT behavior and actual remote Wi-Fi/cellular paths
still require device checks; a cross-build or loopback fixture cannot prove them.
