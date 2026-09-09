# internal/eventchan

Dependency-free typed constants for backend event-channel names.

A registered channel requires:

1. a `Channel` constant here;
2. a `ChannelPolicy` row in `internal/transport/event_channels.go` defining
   audience, scope, and retention; and
3. a constant reference in `cmd/ao-harness/channels.go` when the harness CLI
   may name it.

The registry and AST tests check these surfaces. Emit sites use constants rather
than string literals. Wire-selected subscription names remain strings and are
looked up in the registry; converting them to `Channel` does not register or
authorize them.

Keep this package free of imports so all emitting layers can depend on it. When
another package exports the same cross-process channel name, derive that value
from this constant rather than spelling it twice.

Choose retention from the event semantics. A directive whose waiter expires
should not replay; a time-series sample needed after reconnect should use the
bounded replay ring. Transport policy details are in
[transport.md](../../docs/architecture/transport.md).
