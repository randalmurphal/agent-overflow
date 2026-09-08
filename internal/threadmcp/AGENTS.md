# internal/threadmcp

Shared loopback HTTP MCP transport for browser and remote-command tools. Owns
capability URLs, request admission, JSON-RPC envelopes, per-thread toggles and
listener lifecycle; callers own tool definitions, arguments and authorization.

Every POST requires a loopback socket peer, no Origin and application/json.
OPTIONS receives no CORS permission. Bound bodies and reject trailing JSON.
Neither tokens nor endpoint URLs belong in errors or logs.

Decode tools/call envelopes with DecodeToolCall, which accepts MCP metadata
and protocol extensions. DecodeArgs is only for tool arguments: its closed
schema must not reject client-added envelope fields such as `_meta`. Exercise
realistic metadata in adapter wire tests, not just name/arguments envelopes.

Registering rotates the thread capability. RevokeThread compares the expected
access value so a late old-session teardown cannot revoke its replacement, and
retains the thread toggle across a session restart. UnregisterThread also
forgets that toggle. Close cannot be followed by another listener start.
Callbacks run outside the registry mutex; they must independently recheck live
execution authority and resource ownership. A tool-list omission is not an
execution permission check. Empty tool lists serialize as arrays, never null.

Browser protocol/security tests exercise this transport through its real
adapter. Remote application tests additionally cross it and a real paired TLS
connection, with isolated stores and injected command runners. No real providers.
