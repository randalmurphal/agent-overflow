# internal/mcpstatus/

Per-App cache of MCP server status, shared by every surface that wants
to render "is this MCP server connected?" without re-running a probe.
Live provider sessions feed it for free (Claude `system/init.mcp_servers`,
Codex `mcpServer/startupStatus/updated` + `mcpServer/oauthLogin/completed`
notifications). For inactive threads (or first popup open of the day)
the cache falls back to an ephemeral fetcher: `claude mcp list` for
Claude, `mcpServerStatus/list` JSON-RPC against `codex app-server` for
Codex. Neither path mutates provider state or bills an API call.

The `Cache` is per-App (lazy-init through `(*App).mcpStatus()`); see
`internal/CLAUDE.md`'s no-globals stance.

## Layout

- `doc.go` — package one-liner.
- `status.go` — wire shapes only (`Key`, `Provider`, `Status`,
  `Source`, `ServerStatus`). Provider-native → unified-status
  projectors live with each adapter:
  `internal/provider/claude/mcpstatus.go` (`MCPStatusFromRaw`,
  `MCPStatusFromListLine`) and
  `internal/provider/codex/mcpstatus.go` (`MCPStatusFromList`,
  `MCPStatusFromNotif`).
- `cache.go` — `Cache` + `Fetcher` interface, with TTL + per-key
  (`GetOrFetch`) and per-provider (`RefreshProvider`) single-flight
  gates. `NewCache` is the production constructor; `NewWith` injects
  a clock for tests.
- `events.go` — `EventBus` seam so the App can hook the cache's
  Put/Invalidate emissions onto the `mcp:status` Wails channel without
  this package importing `*App`.
- `cache_test.go` — unit coverage including `-race` regression guards
  for the single-flight + defensive-copy invariants.

The ephemeral fetchers themselves (`claude mcp list` and the Codex
`mcpServerStatus/list` JSON-RPC handshake) live with their provider
adapters; this package only declares the `Fetcher` interface they
implement.

## Responsibility boundary

- What BELONGS here:
  - TTL + dual single-flight bookkeeping (per-key and per-provider
    inflight maps under their own mutex).
  - Defensive cloning so concurrent `RefreshProvider` callers cannot
    race on a shared backing slice.
  - The `Fetcher` interface. Concrete implementations live in each
    provider package (`internal/provider/claude/mcpstatus.go`,
    `internal/provider/codex/mcpstatus.go`).
  - Source-stamping on every Put so subscribers can disclose how
    fresh the data is.
- What does NOT belong here:
  - SQLite writes — status is in-memory only; the live notification
    + ephemeral-fetch paths are authoritative.
  - `*App` state or Wails-binding types — the App composes this
    package with its event bus.
  - Reactive UI projections — the popup / settings derive everything
    they need from the `mcp:status` event channel.

## Invariants

- **Stale entries survive a fetch failure.** `Get` returns `ok=false`
  for an expired entry but does not evict it, so a refetch that
  fails leaves the prior value in the cache rather than blanking the
  UI. `Invalidate` is the only way to remove an entry without
  putting a fresh one.
- **`RefreshProvider` always re-fetches.** Unlike `GetOrFetch`, it
  never reads the cache. Callers reach for it when they want a forced
  refresh (popup auto-fetch on open, explicit Refresh button). Each
  caller — including concurrent waiters that collapse onto one
  fetcher invocation — receives an independent slice clone so
  caller-side `sort.Slice` cannot race peers.
- **Source stamping happens inside the cache.** Fetchers may omit
  `Source`; the cache stamps `SourceEphemeralFetch` in place so the
  slice returned to callers and the slice stored on the inflight both
  carry the stamp. Other Put paths (live session, notification) set
  their own Source before calling `Put`.

## Anti-patterns

- Do NOT add a `force` flag to `RefreshProvider`. It's already a
  forced refresh by contract — adding the flag invites callers to
  pick the wrong default. Use `GetOrFetch(k, fetcher, force)` for
  single-key reads with cache-aware semantics.
- Do NOT hand the same `flight.results` slice to multiple callers.
  `RefreshProvider`'s defensive `cloneStatuses` is load-bearing for
  the `-race` regression — removing it re-introduces the shared-slice
  data race documented in the cache tests.
- Do NOT cache fetcher errors. Only successful results populate the
  cache; errors fall through so a transient CLI failure does not pin
  a bad answer for the full TTL.
- Do NOT add provider-specific parsing or wire decoding here. New
  status sources go into the provider adapter and surface through the
  shared `Fetcher` interface.

## References

- `app_mcp_bindings.go` — `GetMcpServerStatus`,
  `ListMcpServerStatuses`, `RefreshMcpServerStatus`, plus the
  `handleCodexMCP*` notification handlers.
- `internal/provider/claude/mcpstatus.go` — Claude ephemeral fetcher
  (`MCPStatusFetcher`) + projectors (`MCPStatusFromRaw`,
  `MCPStatusFromListLine`) + `sanitizeChildStderr` for bounding
  child-process stderr in user-facing errors.
- `internal/provider/codex/mcpstatus.go` — Codex ephemeral fetcher
  (`MCPStatusFetcher`) + projectors (`MCPStatusFromList`,
  `MCPStatusFromNotif`) backed by an inline JSON-RPC client.
- `internal/provider/claude/parse_system.go` — `system/init`
  mcp_servers extraction that feeds the cache via App wiring.
- `internal/provider/codex/session_notifications.go` —
  `mcpServer/startupStatus/updated` + `mcpServer/oauthLogin/completed`
  dispatch.
- `frontend/src/lib/stores/mcpServers.svelte.ts` — the reactive
  consumer of the `mcp:status` event channel.
