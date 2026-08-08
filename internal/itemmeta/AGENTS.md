# internal/itemmeta/

Shaping helpers for the persisted `items.meta` JSON column, shared by
the triage write path and the store migration chain. Those two cannot
import each other (`triage` imports `store`), so the shared shaping
logic lives here. Stdlib-only.

## Layout

- `trim.go` — `TrimToolResultEcho(toolName, raw)` bounds the Claude
  completion echo (`tool_result` / `tool_use_result`) on persisted tool
  metas: dropped on success, tail-capped error excerpts
  (`ToolResultExcerptCap`) on failure, user-input tools exempt. Fixed
  point — safe to re-run on its own output. Callers: triage
  `shapeToolItemMeta` (every tool persist path) and the store v8
  data-fixup migration.
- `collab_states.go` — `TrimCollabAgentStateMessages(raw)` drops the
  per-agent `message` (the child's full final output, duplicated in
  the lazy tool_call_result payload) from `meta.input.agentsStates` on
  persisted Codex collab tool metas, keeping statuses and every other
  key. Event meta stays untouched — persist shaping rewrites only the
  store.Item's Meta copy, so triage's terminal-evidence readers
  (resolveSubagentsForWait, hasRunningChild), which decode evt.Meta,
  always see full messages. Fixed point. Callers: triage
  `shapeToolItemMeta` and the store v9 data-fixup migration.
- `collab_prompt.go` — `TrimEncryptedCollabPrompt(raw)` removes opaque
  MultiAgentV2 collaboration message ciphertext from persisted item
  metadata, keyed structurally by `input.activityKind`; legacy plaintext
  V1 prompts are preserved.
- `promoted_at_interrupt.go` — the truncation-relevant markers on a
  queued flush user row (`promoted_at_interrupt`,
  `promoted_echo_boundary`) plus `DecodePromotionState`, and the
  `mergeKey` primitive every marker writer in this package uses:
  decode → set one key → re-encode, erroring on malformed meta.
- `import.go` — the session-import markers: `ImportSourceUUIDKey`
  provenance via `MarkImported`, and `ImportUnavailableKey` +
  `MarkImportUnavailable` for an item whose payload the provider session
  no longer contains (`tool-output-gc`, `exec-detail`). The reason set is
  open; the frontend maps known reasons to copy and falls back to its
  existing empty-payload branch. Written by `internal/sessionimport`.

## Responsibility boundary

- What BELONGS here: pure `items.meta` JSON shaping that both the write
  path and migrations must agree on byte-for-byte.
- What does NOT belong here: SQLite access, provider event handling,
  per-tool input registries (`triage/tool_meta_rules.go` owns those —
  they never run from migrations).

## Anti-patterns

- Do NOT import non-stdlib packages or any `internal/` sibling. The
  no-cycle guarantee depends on this.
- Do NOT change trim semantics without checking the frontend consumers
  pinned in the tests (`commandDisplay.ts` error fallbacks,
  `AskUserQuestionCard` answers echo; for the collab-state trim, the
  labels-only receivers line and payload-only completionPreview pinned
  in `ToolCallCard.test.ts` / `WaitGroup.test.ts`).
