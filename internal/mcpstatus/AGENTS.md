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
`internal/AGENTS.md`'s no-globals stance.

## Layout

- `doc.go`: package one-liner.
- `status.go`: wire shapes only (`Key`, `Provider`, `Status`,
  `Source`, `ServerStatus`). Provider-native → unified-status
  projectors live with each adapter:
  `internal/provider/claude/mcpstatus.go` (`MCPStatusFromRaw`,
  `MCPStatusFromListLine`) and
  `internal/provider/codex/mcpstatus.go` (`MCPStatusFromList`,
  `MCPStatusFromNotif`).
- `cache.go`: `Cache` + `Fetcher` interface, with TTL + per-key
  (`GetOrFetch`) and per-provider (`RefreshProvider`) single-flight
  gates. `NewCache` is the production constructor; `NewWith` injects
  a clock for tests. `SnapshotProviderWithFreshness` returns expired
  entries too (annotated `Fresh: false`). The config-fallback MCP
  listing overlays cached statuses onto config-derived membership and
  renders expired entries with their last-known status marked stale
  while a background refresh runs. The cache is status-only, never
  membership: a cached name the config can't derive is another
  workspace's server and must not create a row. `Put` stores the
  incoming status verbatim with one narrow exception (see the
  error-retention invariant below).
- `events.go`: `EventBus` seam so the App can hook the cache's
  Put/Invalidate emissions onto the `mcp:status` Wails channel without
  this package importing `*App`.
- `cache_test.go`: unit coverage including `-race` regression guards
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
  - SQLite writes. Status is in-memory only; the live notification
    + ephemeral-fetch paths are authoritative.
  - `*App` state or Wails-binding types. The App composes this
    package with its event bus.
  - Reactive UI projections. The composer popup derives everything
    it needs from the `mcp:status` event channel.

## Invariants

- **Stale entries survive a fetch failure.** `Get` returns `ok=false`
  for an expired entry but does not evict it, so a refetch that
  fails leaves the prior value in the cache rather than blanking the
  UI. `Invalidate` is the only way to remove an entry without
  putting a fresh one.
- **`RefreshProvider` always re-fetches.** Unlike `GetOrFetch`, it
  never reads the cache. Callers reach for it when they want a forced
  refresh (popup auto-fetch on open, explicit Refresh button). Each
  caller (including concurrent waiters that collapse onto one
  fetcher invocation) receives an independent slice clone so
  caller-side `sort.Slice` cannot race peers.
- **An error-less probe cannot erase a provider's explanation.**
  A status list answers what state a server is in, never why, so an
  ephemeral fetch always Puts an empty `Error`. When such a Put lands
  on an entry whose `Error` a notification or live session produced
  **and the `Status` is unchanged**, `Put` carries that error onto both
  the stored and the emitted status. Otherwise "failed: invalid_grant"
  would collapse into a bare "failed" the user cannot act on. Every
  other transition stores verbatim: a changed status, a non-empty
  incoming error, or an incoming source that is not a fetch. Provenance
  lives in `cacheEntry.errorFrom` (the Source that PRODUCED the error),
  which carry-forwards preserve, so the retention chains across any
  number of consecutive probes instead of evaporating after the first
  one relabels the entry. The retention is deliberately event-bounded,
  never time-bounded: it ends when a provider speaks again or the
  status changes. A probe that agrees the state is current is
  confirmation, not staleness. Aging the cause out by clock would
  reintroduce the bare unactionable "failed" this rule exists to
  prevent. `Put` returns the EFFECTIVE stored status, and
  `GetOrFetch`/`RefreshProvider` results are re-read through it, so
  fetch callers see the retained error too. `cache_test.go` covers this
  over ORDERED PAIRS (including probe chains), not states; a per-state
  test passes with the rule inverted.
- **Source stamping happens inside the cache.** Fetchers may omit
  `Source`; the cache stamps `SourceEphemeralFetch` in place so the
  slice returned to callers and the slice stored on the inflight both
  carry the stamp. Other Put paths (live session, notification) set
  their own Source before calling `Put`.

## Anti-patterns

- Do NOT add a `force` flag to `RefreshProvider`. It's already a
  forced refresh by contract. Adding the flag invites callers to
  pick the wrong default. Use `GetOrFetch(k, fetcher, force)` for
  single-key reads with cache-aware semantics.
- Do NOT hand the same `flight.results` slice to multiple callers.
  `RefreshProvider`'s defensive `cloneStatuses` is load-bearing for
  the `-race` regression. Removing it re-introduces the shared-slice
  data race documented in the cache tests.
- Do NOT cache fetcher errors. Only successful results populate the
  cache; errors fall through so a transient CLI failure does not pin
  a bad answer for the full TTL.
- Do NOT add provider-specific parsing or wire decoding here. New
  status sources go into the provider adapter and surface through the
  shared `Fetcher` interface.

## References

- `app_mcp_bindings.go`: `GetMcpServerStatus`,
  `ListMcpServerStatuses`, `RefreshMcpServerStatus`, plus the
  `handleCodexMCP*` notification handlers.
- `internal/provider/claude/mcpstatus.go`: Claude ephemeral fetcher
  (`MCPStatusFetcher`) + projectors (`MCPStatusFromRaw`,
  `MCPStatusFromListLine`) + `sanitizeChildStderr` for bounding
  child-process stderr in user-facing errors.
- `internal/provider/codex/mcpstatus.go`: Codex ephemeral fetcher
  (`MCPStatusFetcher`) + projectors (`MCPStatusFromList`,
  `MCPStatusFromNotif`) backed by an inline JSON-RPC client.
- `internal/provider/claude/parse_system.go`: `system/init`
  mcp_servers extraction that feeds the cache via App wiring.
- `internal/provider/codex/session_notifications.go`:
  `mcpServer/startupStatus/updated` + `mcpServer/oauthLogin/completed`
  dispatch.
- `frontend/src/lib/stores/mcpServers.svelte.ts`: the reactive
  consumer of the `mcp:status` event channel.
