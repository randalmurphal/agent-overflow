# internal/provider/claude/

Wraps the Claude Code CLI. One process per active thread, NDJSON over
stdio both ways. The CLI owns OAuth; we never see credentials.

## Invocation

```
claude --output-format stream-json --input-format stream-json --verbose
```

Session resume uses `--resume <session-ref>`. Fork is replay-based: we
replay from the chosen turn against a fresh session.

## NDJSON Shapes We Handle

- `system` — `init` (startup metadata: model, cost, context window),
  `compact_boundary`. `tool_progress` is intentionally dropped by the
  chat rewrite; tool cards update through normal item upserts.
- `system.task_started` — fires for `run_in_background` Bash / `Task`
  launches. The parser records a `task_id ↔ tool_use_id` map entry and
  re-emits a meta-only `EventToolStart` (ItemID = tool_use_id,
  Meta = `{"task_id": "..."}`) so triage can persist the mapping into
  `items.meta` for reconnect recovery.
- `system.task_updated` — terminal status patch (`completed` / `failed`
  / `killed`). Resolves tool_use_id from the in-memory map; on a fresh
  adapter session where the map is empty, emits an EventToolComplete
  with empty ItemID and `Meta.task_id` so triage can look up the row
  by `items.meta.task_id`.
- `system.task_notification` — parallel completion trigger. Carries
  both `task_id` and `tool_use_id` inline so it is self-sufficient even
  without a prior task_started. Deduped against task_updated via the
  parser's `completedToolUseIDs` and `completedTasks` sets — whichever
  signal fires first emits the completion, the other is suppressed.
- `assistant` — text deltas, tool calls, thinking blocks.
- `stream_event` — streaming deltas (`text_delta`, `tool_use_start`,
  `tool_result`, …).
- `result` — final tool results.
- `control_request`:
  - `CanUseTool` — approval; may include `UpdatedInput` /
    `UpdatedPermissions` when the user edits before approving.
  - `exit_plan_mode` — plan-mode exit signal. Gated behind a cheap
    `bytes.HasPrefix` check in `session.readLoop` so streaming
    text/thinking lines don't pay a second `json.Unmarshal` on every
    delta.
- `rate_limit_event` — surfaces via the rate-limit normalizer.

`parent_tool_use_id` on tool events correlates subagent (`Task`) work
back to the parent tool call.

## Layout

The parser is split by wire-envelope type so each NDJSON shape has one
owner. The top-level `ParseLine` (in `parser.go`) reads the envelope's
`type` field and dispatches to the matching helper.

- `parser.go` — `Parser` struct, `ParseLine` dispatch, per-parser
  correlation state (background flag, task_id ↔ tool_use_id map,
  dedupe sets).
- `parse_system.go` — `system` envelopes (init metadata, compact_boundary,
  task_started / task_updated / task_notification).
- `parse_assistant.go` — `assistant` envelopes (text deltas, tool_use
  blocks, thinking blocks, usage). The top-level dispatch hands each
  content block to `appendTextEvent` / `appendToolUseEvent` /
  `appendThinkingEvent` / `appendExitPlanModeEvent` / `appendUsageEvent`.
- `parse_user.go` — `user` envelopes carrying `tool_result` blocks,
  split into `appendTaskOutputCompletion` (Task-tool background path)
  and `appendToolResultCompletion` (standard inline path).
- `parse_control.go` — `control_request` envelopes: CanUseTool
  approvals and the exit_plan_mode signal.
- `parse_stream.go` — `stream_event` envelopes (incremental deltas
  between assistant-message boundaries).
- `protocol_meta.go` — `compact_boundary` / context-window meta
  normalisation shared across envelopes.
- `approvals.go` — approval-response encoding for the SDK.
- `session.go` — process lifecycle + read loop that feeds ParseLine.
- `json_helpers.go` — tiny JSON-inspection utilities.
- `options.go` / `probe.go` — non-parser subsystems (session options,
  binary probe).

## Rules

- Every message received must either be handled or explicitly logged
  as "unknown type — ignored". Never silently drop.
- Parser errors do not kill the read loop. Log with enough context to
  reproduce, keep reading. There is a regression test for this — keep
  it passing.
- When Anthropic adds a new message type, add it to the parser *and* to
  the round-trip test before touching UI.

## Reference

- Forge's adapter: `apps/server/src/provider/Layers/ClaudeAdapter.ts` and
  `apps/server/src/provider/claude/*.ts` (full `CanUseTool`,
  subscription probe, Task subagent correlation).
- Upstream SDK: `@anthropic-ai/claude-agent-sdk`.

If behavior drifts, spike against the real CLI — see
`docs/references/spike-policy.md`.
