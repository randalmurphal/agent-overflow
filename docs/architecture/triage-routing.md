# Triage Routing

Every normalized `ProviderEvent` flows through `Router.Handle` in
`internal/triage/router.go`. This page is the routing table, one line per
`EventKind`. The source of truth is `Router.Handle` plus
`internal/provider/types.go` `AllEventKinds`.

## Routing Table

| EventKind | Disposition |
|---|---|
| `init` | `handleInit`: upsert `session_ref` on `threads`. |
| `text_delta` | `handleTextDelta`: create the streaming row on first content, emit ordered `provider:item_event` deltas for follow-up text, flush raw text to SQLite from the stream buffer. |
| `tool_start` | `handleToolStart`: persist tool-use lifecycle row, capture pending inline diff, emit `provider:item_event` upsert. |
| `tool_complete` | `handleToolComplete`: persist tool result (inc. inline diffs), flip status, emit `provider:item_event` upsert. |
| `turn_start` | `handleTurnStart`: open turn state/span and persist the `turns` row. Message anchors are written at send time in `app_send.go`. |
| `turn_complete` | `handleTurnComplete`: drain accumulators, persist text/thinking/plan, close span. |
| `approval_request` | `handleApprovalRequest`: record pending, emit `provider:approval` (request). |
| `approval_resolved` | `handleApprovalResolved`: fold decision onto the row, emit `provider:approval` (resolve). |
| `session_status` | `handleSessionStatus`: precise mapping to `ProviderStatusEventKind`, emit `provider:status` when persistent. |
| `token_usage` | `handleTokenUsage`: persist and emit a provider-normalized context-window snapshot. Generic token-spend totals are ignored here. |
| `error` | `handleError`: persist error row, mark turn items errored on fatal, emit `provider:item_event` upsert + `thread:error_notice` (the sidebar's Failed badge, on a wildcard channel — see below). |
| `compact_boundary` | `handleCompaction`: persist compaction marker; emit an included context-window snapshot when present, otherwise emit `provider:usage` reset. |
| `rate_limits` | `handleRateLimits`: emit `provider:usage` (rate_limits). |
| `content_block_start` / `content_block_stop` | Streaming text/thinking block markers; settle streaming rows on stop. |
| `model_rerouted` | `handleThreadModelUpdate`: persist new model, emit `thread:updated`. |
| `model_fallback` | `handleModelFallback`: persist the provider's warning as a notification, retain the requested model, and emit/hydrate the session-scoped effective model. |
| `thread_renamed` | `handleThreadRename`: persist new title, emit `thread:updated`. |
| `diff` | `handleDiff`: persist payload + meta, upgrade summary-only tool results, emit `provider:item_event` upsert. |
| `command_output` | `handleCommandOutput`: streaming deltas accumulate in the stream-persist buffer and land as one payload append + one `provider:item_event` upsert per flush window (100ms / 64KB / lifecycle boundary); a `Replace` snapshot (Codex aggregatedOutput) discards the pending buffer and rewrites the payload. |
| `thinking` | `handleThinking`: create the thinking row/payload on first content, emit ordered `provider:item_event` deltas for follow-up reasoning, flush summary preview + payload data from the stream buffer. |
| `proposed_plan` | `handleProposedPlan`: persist plan payload, emit `provider:item_event` upsert + a `thread:updated` `full` row (the Plan ready badge is a derived column of that row). |

Routing lands on typed channels. Timeline mutations use
`provider:item_event` (ordered upserts and live text/thinking deltas);
approvals, usage/status, turn lifecycle, subagent notifications, and
background-task changes each use their own typed channel.
There is no generic `provider:event` passthrough. The router exposes a
`SetEventHook` test-only observer so Go tests can synchronize on the
routing pipeline without a wire channel.

`provider:item_event` is **entity-filtered**: the backend withholds
frames for threads a client is not watching
(`internal/transport/event_channels.go`). Anything the sidebar or a
global store must know about a thread nobody has open therefore cannot
be derived from an item row, and rides a wildcard carrier instead —
`thread:error_notice` for the Failed badge, `thread:updated` for the
Plan ready badge (a `full` row) and the reader's own message
(a `patch` carrying only `updatedAt`), and
`provider:background_tasks_changed` for the workspace-change lock. Emit
sites for the first three live beside the persists that cause them
(`emitErrorNotice`, `emitThreadRow`, `bumpThreadActivityForUserText` in
`router.go`). Adding a new global consumer of an item row is the thing
this split exists to prevent.

## Pre-dispatch rewrites

`Handle` performs exactly two checks before the switch. The stopped-thread
gate (invariant 29) drops every wire event for a thread `CleanupThread`
marked stopped. The carrier rewrite then replaces a `ParentToolUseID`
naming a known §E6 resume CARRIER with that agent's transcript ROOT
(`internal/triage/transcript_root.go`), so no handler can write a row
parented to a lifecycle row. Both are cheap by construction — the rewrite
short-circuits on an empty parent before taking the lock — and both apply
to every kind, which is why they live above the switch rather than in a
handler.

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
   `frontend/src/lib/stores/events.ts`. The `never` guard at the
   default branch will fail the TypeScript build if you forget.
5. Update the reference map in `TestAllEventKindsListIsComplete` so the
   new kind is expected there too.

Skipping any step produces either a silent drop (no case in router) or
a CI failure (missing from `AllEventKinds` or the frontend `never`
guard). Both are intentional: a kind the UI doesn't render is a dead
end users can't see.
