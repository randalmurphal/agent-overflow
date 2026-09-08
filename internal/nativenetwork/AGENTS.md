# internal/nativenetwork

Windows-native LAN ingress for the backend inside WSL, without changing WSL,
Windows Firewall, routing tables, credentials, or certificate ownership.

`Run` uses the launcher's existing authenticated owner RPC bridge to read
`Config` and report `State`. It reconciles at most every three seconds, closes
listeners on a failed bridge exchange, and reports bind/discovery errors for
the backend's normal network settings UI. Scan IDs associate one bounded
on-demand native discovery with the requesting frontend. This protocol is not
an independent HTTP/control surface.

Configuration generations fence stale reports after a listener change or a
replacement launcher connection; they are distinct from discovery scan IDs.
Echo the supplied generation even in error reports. The backend clears old
forwarding observations on owner loss and accepts only the current generation.
Each pending native scan owns its result and completion channel, so starting a
new scan cannot replace the results a prior caller is about to consume.

`Relay` binds individual Windows physical private IPv4 addresses at the backend
port. `GetIfEntry2Ex` identifies physical interfaces; do not guess from adapter
names. The upstream is a literal private IPv4 WSL address with an explicit port.
The backend checks its actual listener before enabling the relay: saved LAN
preferences cannot make an explicit loopback-only bind remotely reachable.
Loopback, hostnames, public addresses, credentials and URL paths are refused
inside the relay constructor, not just by its caller.

The non-loopback upstream is a security boundary. A LAN TCP connection forwarded
through localhost would appear to the backend as a local peer. Dialing the WSL
private interface makes the observed peer the Windows vEthernet gateway, which
is non-loopback. No launcher token, session credential, forwarded header or new
trust class is added. TLS terminates at the existing WSL backend and the client
checks its original certificate pin. Forwarded clients share the gateway's
per-peer rate limits and network address in the host's audit records.

Mirrored WSL already exposes the backend on a Windows interface: when its target
is a native interface address, publish that target instead of competing for its
port. NAT mode needs the relay. Windows Firewall still decides whether the
listener is reachable, including its normal Private networks consent prompt;
this code must not silently modify OS configuration or open public networks.

Pairing-window mDNS advertises only addresses whose native listeners bound.
Closing the pairing window stops advertising while established pairing routes
remain available. Name changes replace the advertisement. The bridge publishes
actual external endpoints so pairing QR codes and authenticated alternate-route
bootstrap do not advertise an unreachable WSL NAT address — including before
the first report: the backend's headless WSL boot marks native ingress as
expected (`app.ExpectNativeNetwork`) so its routes carry no LAN address until
`Run` has reported one, rather than the NAT address the desktop fallback would
discover.

Connection count is capped at 64; upstream dialing is bounded to five seconds.
The latest upstream dial failure
is reported through the existing host network state and clears after a successful
dial; accepting a Windows socket alone does not prove WSL is reachable.

Shutdown cancels both directions and waits for active copies. TCP half-close
must preserve a host response after the client finishes writing. Relay tests
use loopback fixtures only by bypassing constructor admission *inside the test*
(the production constructor still rejects loopback), and verify original TLS,
unchanged authority/headers, half-close, cancellation and unsafe-target refusal.
Windows builds compile the native adapter selection; real Windows firewall,
NAT and mirrored-network reach still require the physical-machine check.
