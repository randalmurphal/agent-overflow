# internal/triage/

Classifies provider events and decides what ships to the frontend vs
what writes to SQLite. The single most important rule is that triage
has **no derived state** — it is a pure function of the current event
plus a narrow, bounded set of per-thread correlation state.

## Layout

The package is split by concern so each file owns a narrow slice of the
routing pipeline. New routing logic belongs in whichever file most
closely matches its concern; create a new file (and list it here) if
none fits.

- `router.go` — entry point. `Router` struct, `Handle` dispatch switch,
  `persistItem` / `emitThreadUpdated` shared helpers, and the top-level
  error / session-status / token-usage / rate-limit routers.
- `session_status.go` — `EventSessionStatus` classifier:
  `classifySessionStatus` maps content + meta → `ProviderStatusEventKind`
  (rate-limit / unauthenticated / transient retry / ok), plus the
  `logUnknownSessionStatusOnce` capped log throttle that keeps novel
  status strings from polluting steady-state logs.
- `approvals.go` — approval-request lifecycle: pending-approval map,
  approval-resolved fan-out, decision → item projection.
- `turn_lifecycle.go` — per-turn and per-thread correlation state
  (open turns, interrupt queue, captured-turn guard, stopped-thread
  markers, turn span bookkeeping, cleanup paths).
- `tool_lifecycle.go` — tool-call launch/completion rows,
  background-task pairing, summary/status derivation.
- `tool_result_file_change.go` — `file_change` tool-result normalisation
  (inline diff projection, unified patch assembly).
- `tool_result_diff_upgrade.go` — late-arriving diff upgrades that
  attach a richer payload onto a previously persisted tool result.
- `command_inline_diff_capture.go` / `command_inline_diff_parser.go` /
  `command_inline_diff_runtime.go` / `command_inline_diff_persist.go` —
  command-execution inline-diff pipeline, split by phase
  (capture → parse → runtime match → persist).
- `payload_items.go` — diff / command output / thinking / plan payload
  writers.
- `stream_items.go` / `stream_state.go` / `block_events.go` —
  streaming text / thinking block lifecycle and the content-block
  index bookkeeping they depend on.
- `usage_compaction.go` — token-usage normalisation and compaction
  boundary persistence.
- `turn_events.go` — frontend-facing payload shapes for
  `provider:turn_started` / `provider:turn_completed` /
  `provider:subagent_notification`, plus the canonical stop-reason
  normaliser.
- `meta.go` — shared JSON-inspection helpers.
- `maps.go` — generic map utilities (currently just `deleteByPrefix`).

## Routing table

| Event kind | Destination |
|---|---|
| Text delta | Frontend (passthrough). |
| Tool-use start/complete | Frontend event + item in SQLite on completion. |
| Approval request | Frontend event with `request_id` preserved. |
| Diff | SQLite payload + meta to frontend. Full diff is on-demand. |
| Command output | SQLite payload + meta to frontend. |
| Thinking block | SQLite payload + preview to frontend. |
| Turn metadata (cost, tokens, context) | Inline to frontend, persist on `threads`. |
| Background task terminal (Claude) | `tool_completion` sibling row upsert (idempotent). See `turn-lifecycle.md`. |
| Turn start/complete | Write `turns` row; emit `provider:turn_*` to frontend; force-close orphan tool_calls on complete. |
| Error | Distinct event kind; frontend renders as status/alert. |
| Unknown | Log with full context, do not drop silently. |

## Lifecycles we route

Authoritative mental model:
[`turn-lifecycle.md`](../../docs/architecture/turn-lifecycle.md).

- **Tool lifecycle** — `EventToolStart`/`EventToolComplete` keyed by
  tool_use_id. Triage upserts `tool_call` rows. Per-spec backgrounded
  launches stay `status=running` (the `tool_completion` sibling is
  written by the task lifecycle).
- **Task lifecycle (Claude only)** —
  `EventBackgroundTaskTerminal` → idempotent sibling row upsert via
  `persistItem` (stable `complete:<launchID>` id + `UpsertItem`'s
  INSERT-OR-UPDATE semantics). Both `task_updated` terminal and
  TaskOutput enrichment arrive via this event; the upsert coalesces
  them in place, with the richer payload winning.
- **Turn lifecycle** — `EventTurnStart` writes a `turns` row with
  `completed_at=null`; `EventTurnComplete` updates it and emits
  `provider:turn_completed` to the frontend. Triage force-closes any
  `tool_call` row with `status='running' && !is_background &&
  turn_index=currentTurn` as a safety net.

⚠ **Load-bearing invariants** (see
[`invariants.md`](../../docs/architecture/invariants.md)):

- `task_notification` is NOT a completion source; drop parser
  emission.
- Turn activity on the frontend is wire-pushed only — never derived
  from item state.
- No session-liveness probing for turn state inference.

## Responsibility boundary

- What BELONGS here:
  - Classify a single event → zero or more (persist + emit) decisions.
  - Bounded per-thread transient correlation state with explicit
    cleanup paths (see below).
  - Shared helpers for `persistItem` and `emitThreadUpdated`.
- What does NOT belong here:
  - Cross-turn derivations — do them in the frontend or as a persisted
    projection, not as an in-memory map here.
  - Provider-specific types. Provider packages normalize before handing
    events to triage.
  - Business decisions about when to fork/resume a thread; that's
    `app.go`.

## Correlation state (bounded, not derived)

The Router carries a narrow set of per-thread maps (interrupt queue,
open turn index, content-block counters, active streaming block flags,
pending approvals / approval decisions, pending command inline diffs,
captured-turn guard, turn spans, stopped-thread markers, streaming
render throttle) that exist purely to correlate one event to the next
within a turn — not to duplicate the store or the provider session.
All of these are bounded and have an explicit cleanup path:

- Per-turn state clears on `EventTurnComplete` (and on a matching
  error branch for errored turns).
- Per-thread state clears on `CleanupThread`.
- Approval and interrupt-queue entries clear when their correlated
  event resolves.

The one deliberate exception is the interrupt queue, which can span a
turn boundary because its contract is "persist queued events once the
interrupt lifts."

## Server-rendered display HTML

Every `persistItem` call (and the streaming two-phase append) runs raw
text through `internal/highlight/Renderer` so every row hitting SQLite
carries its own `highlighted_content` — the frontend paints it via
`{@html}` with no per-render cost on the UI thread. Constructors take a
non-nil `*highlight.Renderer`; the same instance is shared across the
Router and the `ChannelService`.

Streaming uses the split `AppendItemSummary` + `UpdateItemHighlight`
pair in `internal/store`, with the render running between them so the
SQLite writer lock is never held while goldmark/chroma/terminal-to-html
parses. Render frequency is throttled per-item via
`Router.nextHighlightAt`: one render per
`streamingHighlightIntervalMs` (50 ms) per streaming item id.

`persistItem` renders `HighlightedContent` unconditionally against the
current `Summary`. This is a defensive contract: a caller that loads a
row from the store (its `HighlightedContent` already populated), mutates
`Summary`, and calls `persistItem` would otherwise leave stale HTML on
the row. Streaming hot paths bypass `persistItem` — they go through
`AppendItemSummary` + `UpdateItemHighlight` directly — so the
unconditional render only runs at event-boundary rate and its cost is
noise. Settle paths (`settleStreamingText`, `settleStreamingThinking`,
`flipTurnItemsErrored`) still clear `HighlightedContent` defensively,
but the contract no longer depends on it.

`internal/highlight/dispatch.go` is the single source of truth for
which kinds are server-rendered. Do NOT build a parallel dispatch
table here; call `RenderForKind` and let the highlight package own the
list.

## Extension points

- To add routing for a new event kind: pick or create the matching
  `*_lifecycle.go` / `*_items.go` file, add a `Handle` switch case in
  `router.go`, write the routing-decision test FIRST. See
  `docs/architecture/how-to.md#add-a-new-event-kind`.
- To add a new persisted payload kind: extend `payload_items.go`,
  update `docs/architecture/schema.md`.

## Anti-patterns

- Do NOT cache store data here. No caching of store data. Transient
  correlation state only. Cross-turn derivation forbidden beyond the
  interrupt queue.
- Do NOT put preview content in the payload data blob. Meta is cheap,
  data is heavy — preview/stats in `meta`, full content in `data`.
- Do NOT combine or split events across boundaries. One event in, zero
  or more routing decisions out.
- Do NOT reach back into provider-specific types. If you need a detail
  the normalized event doesn't carry, fix the normalization upstream.

## Testing

- Every routing decision has a unit test with a representative event.
- When a new provider event type is added upstream, the routing
  decision is the first test — not the last.

## References

- `docs/architecture/data-flow.md` — end-to-end pipeline diagram.
- `docs/architecture/triage-routing.md` — detail on per-kind decisions.
- `docs/architecture/schema.md` — payload / item column reference.
