# Triage Routing

Every normalized `ProviderEvent` flows through `Router.Handle` in
`internal/triage/router.go`. This page is the routing table — one line per
`EventKind`, source of truth is `Router.Handle` plus
`internal/provider/types.go` `AllEventKinds`.

## Routing Table

| EventKind | Disposition |
|---|---|
| `init` | `handleInit` — upsert `session_ref` on `threads`. |
| `text_delta` | `handleTextDelta` — append streaming item via SQLite append, emit `provider:item_upsert`. |
| `tool_start` | `handleToolStart` — persist tool-use lifecycle row, capture pending inline diff, emit `provider:item_upsert`. |
| `tool_complete` | `handleToolComplete` — persist tool result (inc. inline diffs), flip status, emit `provider:item_upsert`. |
| `turn_start` | `handleTurnStart` — open turn span, capture git baseline. |
| `turn_complete` | `handleTurnComplete` — drain accumulators, persist text/thinking/plan, close span. |
| `approval_request` | `handleApprovalRequest` — record pending, emit `provider:approval` (request). |
| `approval_resolved` | `handleApprovalResolved` — fold decision onto the row, emit `provider:approval` (resolve). |
| `session_status` | `handleSessionStatus` — precise mapping to `ProviderStatusEventKind`, emit `provider:status` when persistent. |
| `token_usage` | `handleTokenUsage` — emit `provider:usage` (cost pre-computed in the provider adapter). |
| `error` | `handleError` — persist error row, mark turn items errored on fatal, emit `provider:item_upsert`. |
| `compact_boundary` | `handleCompaction` — persist compaction marker, emit `provider:usage` (reset). |
| `rate_limits` | `handleRateLimits` — emit `provider:usage` (rate_limits). |
| `content_block_start` / `content_block_stop` | Streaming text/thinking block markers; settle streaming rows on stop. |
| `model_rerouted` | `handleThreadModelUpdate` — persist new model, emit `thread:updated`. |
| `thread_renamed` | `handleThreadRename` — persist new title, emit `thread:updated`. |
| `diff` | `handleDiff` — persist payload + meta, upgrade summary-only tool results, emit `provider:item_upsert`. |
| `command_output` | `handleCommandOutput` — persist command_output payload, emit `provider:item_upsert`. |
| `thinking` | `handleThinking` — persist + append streaming thinking block, emit `provider:item_upsert`. |
| `proposed_plan` | `handleProposedPlan` — persist plan payload, emit `provider:item_upsert`. |

Routing lands on typed channels: `provider:item_upsert` (every
timeline state change), `provider:approval`, `provider:usage`, and
`provider:status`. There is no generic `provider:event` passthrough —
the router exposes a `SetEventHook` test-only observer so Go tests can
synchronize on the routing pipeline without a wire channel.

## The Sentinel

The `default` branch in `Handle` returns
`fmt.Errorf("%w: %s", ErrUnhandledEventKind, evt.Kind)` and emits on no
channel. The sentinel exists so `TestHandleEveryEventKindCovered` (in
`internal/triage/router_test.go`) can loop `provider.AllEventKinds`
and fail loudly if any kind falls through.
`TestAllEventKindsListIsComplete` (same file) guards the complementary
drift: a new const in `types.go` that isn't added to `AllEventKinds`.

## Adding a New EventKind

When a provider surfaces a new event type:

1. Add the constant to the `const` block in `internal/provider/types.go`.
2. Append it to `provider.AllEventKinds` in the same file.
3. Add a `case` in `Router.Handle` (in `internal/triage/router.go`).
4. Add the matching case in the frontend switch in
   `frontend/src/lib/stores/events.ts` — the `never` guard at the
   default branch will fail the TypeScript build if you forget.
5. Update the reference map in `TestAllEventKindsListIsComplete` so the
   new kind is expected there too.

Skipping any step produces either a silent drop (no case in router) or
a CI failure (missing from `AllEventKinds` or the frontend `never`
guard). Both are intentional — a kind the UI doesn't render is a dead
end users can't see.
