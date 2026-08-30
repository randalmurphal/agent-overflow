# ADR-005: Schema v15 Wipes `items` and `payloads`

Status: accepted
Date: 2026-04-18

## Context

The chat-rewrite introduced a fundamentally different item model: a
closed set of 7 kinds (user_text, assistant_text, thinking, tool_call,
tool_completion, error, compaction), deterministic id formats per
kind, `parent_id` as the single nesting column (replacing
`parent_tool_use_id`), `completion_of` (replacing
`completion_of_item_id`), and a new `meta` JSON column for
provider-specific per-item data.

The existing v14 `items` table used a different kind taxonomy, had
provider-specific column names, and stored data in shapes that didn't
map cleanly to the new model. Copying forward would require:

- Heuristic classification of old rows into new kinds.
- Invented `turn_index` values where the old schema didn't have them.
- Synthesized `segment_index` values for text rows.
- A lookup table of old-id → new-id to rewrite cross-references.

Every one of those is a source of subtle drift that would haunt later
migrations.

## Decision

Migration v15 (`chat_rewrite_unified_items`) drops and rebuilds both
`items` and `payloads` from scratch. Existing timeline data is
discarded. Thread rows, project rows, and provider session refs are
preserved. Only the timeline cache is wiped.

## Rationale

- **Item identity is the breaking change.** Every item's id format
  changed. There's no reversible mapping, so replay-style migration
  is impossible.
- **History is a cache, not source of truth.** Core principle 3 says
  the provider's session file is the authoritative conversation
  history. A wipe loses the cache, not the data. The user can
  replay from the provider session to rebuild (via `--resume` for
  Claude, `thread/resume` for Codex).
- **Users lose no active work.** Active threads keep their
  `session_ref`; on the first message after migration, resume
  re-hydrates the recent items under the new schema. Long-tail
  historical conversations are lost, but those weren't load-bearing.
- **Forward cleanliness.** The new schema starts clean: no
  compatibility shims, no "this column was called X in v14" case
  branches.

Considered alternatives:

- **Data migration.** Rejected: the heuristics to map old rows to
  new kinds would be wrong at least 5% of the time, producing
  corrupted history that's worse than no history.
- **Dual-read (read old schema as fallback).** Rejected: drags the
  old kinds into every query and every renderer. The whole point of
  the rewrite was to stop branching on legacy shapes.

## Consequences

- Users running v14 upgrade to v15 and lose historical timeline
  data. The release notes called this out.
- Subsequent migrations (v16, v17) are purely additive. They
  extend v15's shape without rebuilding. The wipe was a one-time
  cost for the chat-rewrite transition.
- Column renames (`parent_tool_use_id` → `parent_id`,
  `completion_of_item_id` → `completion_of`) are real renames in
  the new schema, not aliases. Callers reference the new names.
- This ADR sets a precedent: future wipes are only acceptable for
  fundamental shape changes. Additive migrations are the default.
