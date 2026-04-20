# internal/triage/

Classifies provider events and decides what ships to the frontend vs
what writes to SQLite. The single most important rule is that triage
has **no derived state** — it is a pure function of the current event.

## Routing Table

| Event kind | Destination |
|---|---|
| Text delta | Frontend (passthrough). |
| Tool-use start/complete | Frontend event + item in SQLite on completion. |
| Approval request | Frontend event with `request_id` preserved. |
| Diff | SQLite payload + meta to frontend. Full diff is on-demand. |
| Command output | SQLite payload + meta to frontend. |
| Thinking block | SQLite payload + preview to frontend. |
| Turn metadata (cost, tokens, context) | Inline to frontend, persist on `threads`. |
| Background task start/complete | See data-flow.md; two distinct items. |
| Error | Distinct event kind; frontend renders as status/alert. |
| Unknown | Log with full context, do not drop silently. |

See `/docs/architecture/data-flow.md` for the full pipeline diagram.

## Rules

- **No data caching, no cross-turn derived read models.** Triage
  persists to SQLite and emits to the frontend; it does NOT
  maintain in-memory views of timeline content or compute
  aggregates the renderer could derive on its own. If you need to
  derive something across events, do it in the frontend or in a
  persisted projection — not here.
- **Per-thread transient correlation state is allowed.** The Router
  carries a narrow set of per-thread maps (interrupt queue, open
  turn index, content-block counters, active streaming block flags,
  pending approvals / approval decisions, pending command inline
  diffs, captured-turn guard, turn spans, stopped-thread markers)
  that exist purely to correlate one event to the next within a
  turn — not to duplicate the store or the provider session. All of
  these are bounded and have an explicit cleanup path:
  - Per-turn state clears on `EventTurnComplete` (and on a matching
    error branch for errored turns).
  - Per-thread state clears on `CleanupThread`.
  - Approval and interrupt-queue entries clear when their
    correlated event resolves (approval resolved, interrupt drained).
  The one deliberate exception is the interrupt queue, which can
  span a turn boundary because its contract is "persist queued
  events once the interrupt lifts." Any other cross-turn derivation
  is forbidden — if you need one, add it as a store query or a
  frontend derivation, not a new map here.
- **No provider-specific types.** Provider packages normalize before
  handing events to triage.
- **Meta is cheap, data is heavy.** When in doubt, put preview/stats
  in `meta` and the full content in `data`.
- **One event in, zero or more routing decisions out.** Don't combine
  or split events across boundaries.

## Testing

- Every routing decision has a unit test with a representative event.
- When a new provider event type is added upstream, the routing
  decision is the first test — not the last.
