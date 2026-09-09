# Native network bridge

This package exposes LAN listeners from the native host when the backend runs
inside WSL. It relays raw TLS to a verified non-loopback WSL endpoint and reports
native endpoints and errors to the backend.

`StartRelay` validates its own target: an HTTPS URL containing one explicit
private, non-loopback WSL IPv4 address and port, with no credentials, query, or
non-root path. `LANAddresses` identifies physical Windows interfaces through the
native interface API rather than adapter names. Keep its address and connection bounds,
dial deadline, half-close behavior, and joined shutdown documented beside the
implementation in `relay.go`.

Bind only eligible physical interfaces and keep admission, pairing
advertisements, and mirrored mode tied to the backend's current configuration.
Never advertise the WSL NAT address, proxy remote clients through localhost, or
inject launcher credentials into relayed traffic. Before the first valid report,
the backend advertises no LAN endpoint.

Configuration changes and cancellation replace the complete listener set.
Close old listeners, wait for relays, and report partial bind failures visibly.
`Config.Generation` orders listener replacement; `ScanID` independently owns one
caller's discovery result. Preserve the type-level contract in `types.go` so a
stale report or later scan cannot overwrite current state.
Tests must not alter the developer firewall or network configuration; isolate
socket and interface discovery behind package seams.
