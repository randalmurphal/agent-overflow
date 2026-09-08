# internal/nearby

Untrusted LAN DNS-SD hints, separate from pairing and all credentials.
`Start` owns per-interface mDNS responders on the interfaces present when
it runs; `Discover` owns a bounded two-second scan with no persistent cache.
Root composition starts responders when a pairing window opens on a host
sharing on the LAN and closes them when that window is closed, replaced,
loses its owner connection, or a listener-affecting setting changes. Nothing
observes interface changes: a network that appears later is advertised by
the next window. A start failure is the window's user-facing state
(`ComputerPairingView.DiscoveryError`), never only a log line. Name is a
getter so renames do not leave stale labels; the responder reads it only for
an answer that carries the TXT record, because the library asks the zone
about every question on the LAN and the getter stats a file.

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
