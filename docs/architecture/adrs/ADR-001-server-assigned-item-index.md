# ADR-001: Server-Assigned `item_index`

Status: accepted
Date: 2026-04-18

## Context

Timeline items need a monotonic ordering key. Two candidate sources:
(a) the order the provider emits events on the wire, or (b) the order
the store assigns when the row is first inserted.

Wire order is deceptive. Claude emits text deltas, tool_use, thinking
blocks, and `system.task_notification` events interleaved. Codex emits
`turn/*` and `tool/*` notifications out of their final rendering order
— a backgrounded tool completion can arrive mid-stream, before the
streaming text it follows has settled. If we used wire order, the
frontend would have to re-sort on every upsert, and "new row shows up
above an in-flight row" bugs would recur.

## Decision

The store assigns `item_index` at first INSERT. Updates preserve it.
The value is a per-(thread, turn) monotonic int, maintained by the
store's `UpsertItem` path.

## Rationale

- The store already serializes writes (`SetMaxOpenConns(1)`), so the
  assignment is race-free.
- The `UNIQUE INDEX idx_items_thread_turn_item_unique` guarantees no
  two rows share an index within the same turn.
- The frontend reads `turn_index, item_index` as the ordering key
  and never re-sorts on upsert.
- Invariant #1 ("`item_index` immutable after first upsert") flows
  directly from this decision.

Considered alternatives:

- **Wire-order index.** Rejected: interrupt-queue deferrals break
  wire order. A mid-stream backgrounded tool completion must
  render AFTER the streaming text, not wherever it happened to
  arrive.
- **Client-side sort.** Rejected: forces the frontend to hold
  ordering logic that duplicates server state. Breaks the
  "frontend memory is bounded" principle.

## Consequences

- The streaming-phase interrupt queue (see ADR-003) is mandatory —
  without it, the server-assigned index would put backgrounded
  completions in the wrong visual position.
- `item_index` must never be rewritten. Any future "move this item
  up" feature needs a different mechanism (a separate sort-key
  column, for instance).
- Upsert replay on `--resume` works naturally: the same id finds the
  same row, and the existing `item_index` is preserved.
