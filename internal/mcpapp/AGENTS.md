# internal/mcpapp/

Owns the application-level MCP surface across Claude and Codex: provider-native
config projection, cached status, thread and workspace toggles, OAuth flow
lifecycle, and live-session reconnect/reload coordination.

The named `main.App` wrapper remains the Wails service.
`internal/app/app_mcp_bindings.go` owns the exact bound method signatures, wire
DTOs, shutdown sentinel, and adapters into
the session manager, provider credential store, event bus, and triage. Do not
move those wire shapes or method receivers into this package.

The service owns every MCP mutex, timer, poll, temporary auth process, and
reload coalescer. Dependencies arrive through `Deps`; package code must not
resolve global homes, sessions, lifecycle state, or event transport through a
back-channel.

Tests must inject path-scoped Claude/Codex config stores and fake provider
sessions or binaries. They must never read the developer's real provider homes
or spawn a real provider CLI. Coordination state-machine tests belong here;
`internal/app` tests are reserved for App session/lifecycle and wire integration.
