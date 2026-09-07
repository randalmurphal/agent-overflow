# internal/nearby

Untrusted LAN DNS-SD hints, separate from pairing and all credentials.
`Start` owns per-interface mDNS responders while LAN sharing is enabled;
`Discover` owns a bounded two-second scan with no persistent cache. Root
composition closes/recreates responders on listener or network changes.
Name is a getter and is read per query so renames do not leave stale labels.

Only protocol version, backend ID, name, port and private IPv4 addresses are
advertised. Hints confer no identity, reachability or authorization: callers
probe the actual AO endpoint and perform owner-confirmed pairing. Never add
invitation tokens, certificate trust, project names or credentials to TXT.

The hashicorp mDNS library handles responder records. The browser uses bounded
DNS-SD unicast-response queries instead of the library's unbounded accumulated
name map. Packets, record counts, interfaces, scan time and returned hosts are
bounded. Unrelated services, malformed metadata and non-private addresses are
rejected before any connection attempt. Parsing tests use real library records
without touching a developer's LAN.

Discovery currently uses IPv4 multicast, available on ordinary LANs, and may
be blocked by guest/client isolation or firewall policy. WSL mirrored networking
supports multicast; WSL NAT does not make its multicast visible on the physical
LAN. The native launcher advertises its externally reachable listener through
`nativenetwork`, restricting `Advertisement.Addresses` to interfaces that bound.
Typed-address pairing must remain available; discovery failure must not disable
ordinary known-address connections. Tailnet peer enumeration belongs to tailnet,
not multicast: Tailscale does not forward LAN broadcast/multicast.
