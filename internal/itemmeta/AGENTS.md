# internal/itemmeta/

Shaping helpers for the persisted `items.meta` JSON column, shared by
the triage write path and the store migration chain. Those two cannot
import each other (`triage` imports `store`), so the shared shaping
logic lives here. Stdlib-only.

## Helpers

- `trim.go`: `TrimToolResultEcho(toolName, raw)` bounds the Claude
  completion echo (`tool_result` / `tool_use_result`) on persisted tool
  metas. Dropped on success, tail-capped error excerpts
  (`ToolResultExcerptCap`) on failure, user-input tools exempt. Fixed
  point, so it is safe to re-run on its own output. Callers: triage
  `shapeToolItemMeta` (every tool persist path) and the store v8
  data-fixup migration.
- `collab_states.go`: `TrimCollabAgentStateMessages(raw)` drops the
  per-agent `message` (the child's full final output, duplicated in
  the lazy tool_call_result payload) from `meta.input.agentsStates` on
  persisted Codex collab tool metas, keeping statuses and every other
  key. Event meta stays untouched, because persist shaping rewrites only
  the store.Item's Meta copy, so triage's terminal-evidence readers
  (resolveSubagentsForWait, hasRunningChild), which decode evt.Meta,
  always see full messages. Fixed point. Callers: triage
  `shapeToolItemMeta` and the store v9 data-fixup migration.
- `collab_prompt.go`: `TrimEncryptedCollabPrompt(raw)` removes opaque
  MultiAgentV2 collaboration message ciphertext from persisted item
  metadata, keyed structurally by `input.activityKind`. Legacy plaintext
  V1 prompts are preserved.
- `promoted_at_interrupt.go`: the truncation-relevant markers on a
  queued flush user row (`promoted_at_interrupt`,
  `promoted_echo_boundary`) plus `DecodePromotionState`, and the
  `mergeKeys` primitive every marker writer here uses. A writer that
  sets two keys goes through the plural form, so both land in one
  decode/encode and no row can persist half-marked.
- `provider_queued.go`: `providerQueued` (this message left AO's queue
  for the Codex provider queue, `thread/queue/*`, codex >= 0.148) and
  `providerQueueHandoff` (that hand-off is still UNPROVEN). Both are set
  BEFORE the `thread/queue/add` write, because an add that lands and is
  never acked is exactly when this process may not come back to stamp
  anything, and only the second is cleared on confirmation. The split is
  what lets a later process tell a message the provider DISPATCHED
  (leave the row as history) from one it never took (hand it back to the
  composer). NOTHING writes them any more, but the legacy sunset in
  `app_codex_provider_queue.go` still reads the pair on rows an older
  build left in codex's queue.
- `import.go`: the session-import markers, `ImportSourceUUIDKey`
  provenance via `MarkImported` and `ImportUnavailableKey` +
  `MarkImportUnavailable` for an item whose payload the provider session
  no longer contains (`tool-output-gc`, `exec-detail`). The reason set is
  open. The frontend maps known reasons to copy and falls back to its
  existing empty-payload branch. Written by `internal/sessionimport`.

## Responsibility boundary

- What BELONGS here: pure `items.meta` JSON shaping that both the write
  path and migrations must agree on byte-for-byte.
- What does NOT belong here: SQLite access, provider event handling,
  per-tool input registries (`triage/tool_meta_rules.go` owns those, and
  they never run from migrations).

## Anti-patterns

- Do NOT import non-stdlib packages or any `internal/` sibling. The
  no-cycle guarantee depends on this.
- Do NOT change trim semantics without checking the frontend consumers
  pinned in the tests (`commandDisplay.ts` error fallbacks,
  `AskUserQuestionCard` answers echo; for the collab-state trim, the
  labels-only receivers line and payload-only completionPreview pinned
  in `ToolCallCard.test.ts` / `WaitGroup.test.ts`).
