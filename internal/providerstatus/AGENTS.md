# Provider status

This package defines the `provider:status` wire shape and pure mapping helpers
for provider installation, version, and authentication health. Timeline events
remain on `provider:item_event`; application detection and emission remain in
`internal/app`.

Keep `Event` JSON fields synchronized with the frontend type and router. A new
`Kind` needs a frontend branch in the same change because unknown kinds are
dropped. A thread-scoped status raise may use a kind; withdrawal uses
`status:"ready"` without a kind.

`VersionToken` normalizes provider version strings. `BinaryStale` compares
only when both versions are known and treats trailing zero segments as equal.
`ActionURL` is the sole table of provider status links.

`ClaudeUnauthenticated` is the shared Claude-only heuristic. No identity
evidence means logged out; token source, email, display name, or a non-first
party API provider is evidence of authentication. Subscription type alone is
not. Codex account fields do not support this heuristic.
