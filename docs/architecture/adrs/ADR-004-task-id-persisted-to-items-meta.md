# ADR-004: `task_id` Persisted to `items.meta`

Status: accepted
Date: 2026-04-18

## Context

Claude backgrounded tools (`run_in_background` Bash, `Task` subagent)
correlate `task_started` → terminal `task_updated` (and any later
TaskOutput retrieval) via a `task_id`. Our `tool_call` row uses the
`tool_use.id` as its primary id. The mapping `task_id ↔ tool_use_id`
has to live somewhere so that when a terminal `task_updated` arrives,
we can find the right launch row and write the separate
`tool_completion` sibling.

Two options:

- **In-memory map on the adapter.** `parser.taskIDToToolUseID`
  populated on `task_started`, looked up on `task_updated`.
- **Persist on the `tool_call` row itself.** Store the `task_id` in
  `items.meta` on the row at `task_started` time.

## Decision

Persist `task_id` to `items.meta` at `task_started` time, and provide
a dedicated store lookup `FindToolCallItemByTaskID` backed by a
partial index on `json_extract(meta, '$.task_id')`
(migration v17).

The adapter also keeps its in-memory map for the common "same
session" fast path, but the persisted copy is the source of truth for
reconnect.

## Rationale

- **Crash recovery.** If the app restarts while a backgrounded tool
  is running, the in-memory map is gone. When the terminal
  `task_updated` eventually lands, we need to find the tool_call
  row by `task_id` alone. SQLite gives us that.
- **Session replay.** Claude's `--resume` path re-emits
  `task_started` events. The dedicated per-row copy means replay
  doesn't need to race against an in-memory map that may have lost
  entries.
- **Partial index keeps it cheap.** `idx_items_meta_task_id` uses
  `WHERE json_extract(meta, '$.task_id') IS NOT NULL`, so only
  backgrounded tool rows pay the index cost. Non-backgrounded tools
  are the overwhelming majority; they don't carry a task_id at all.

Considered alternatives:

- **Only in-memory.** Rejected: crash recovery breaks. A
  `run_in_background` Bash that completes while the app was closed
  would come back as a `completed` event with no way to find the
  launch row.
- **Separate `task_map` table.** Rejected: adds a second row write
  per backgrounded tool. The `items.meta` column was already
  generic JSON; adding a key is zero cost beyond the partial index.

## Consequences

- The `FindToolCallItemByTaskID` query is O(log N) on the partial
  index rather than O(N) on the thread's item list.
- The adapter owns the meta-key schema: today it's `{task_id: "..."}`;
  future keys (thinking signatures, receiverThreadIds) live
  alongside. See `internal/provider/claude/AGENTS.md` for the
  current set.
- Dedupe/coalescing lives at the stable `tool_completion` sibling id.
  `task_notification` is not a completion source; if surfaced, it
  must use a distinct notification path and must not participate in
  lifecycle dedupe.
