# Claude Code CLI — NDJSON wire reference

Authoritative reference for the JSON shapes the Claude Code CLI emits over
stdio in `stream-json` mode. Consulted by `internal/provider/claude/`
parser code. Do not rely on memory; if a shape is not listed here,
confirm against the Python SDK or captured samples before coding.

## Sources

**Shape-of-truth, in priority order:**

1. **Captured wire samples** (real NDJSON from live sessions) in
   `docs/references/fixtures/claude/` — see
   [§Captured samples](#captured-samples).
2. **Python SDK** at `/Users/randy/repos/claude-agent-sdk-python`.
   The dataclasses in `src/claude_agent_sdk/types.py` and the parser at
   `src/claude_agent_sdk/_internal/message_parser.py` describe every
   envelope the SDK models. Shapes NOT modeled there fall through to
   `SystemMessage(subtype, data)` and must be read raw (see
   [§task_updated](#systemtask_updated)).
3. **forge's adapter** at
   `/Users/randy/repos/forge/apps/server/src/provider/Layers/ClaudeAdapter.ts`
   and `claude/streamHandlers.ts` / `claude/sdkMessageParsing.ts` —
   authoritative for interpreting shapes the SDK leaves underspecified
   (`task_updated.patch`, TaskOutput `tool_use_result.task`).

**Capturing fresh samples**: set `AGENT_OVERFLOW_DEBUG=provider` before
launching the app. Raw stdio lines (pre-parse) land in
`<dbDir>/logs/provider-events-YYYY-MM-DD.ndjson`. See
`internal/logging/process.go:273-286`.

## Envelope types

The CLI emits newline-delimited JSON. Every line has a top-level
`type` field dispatched in `parser.go:131-152`:

| `type` | Dispatch | Purpose |
|---|---|---|
| `system` | `parseSystem` | Init, task lifecycle, compact boundary, session status. |
| `assistant` | `parseAssistant` | Text / thinking / tool_use blocks, token usage. |
| `user` | `parseUser` | `tool_result` blocks echoed back after tool execution. |
| `stream_event` | `parseStreamEvent` | Incremental deltas (requires `include_partial_messages:true`). |
| `result` | `parseResult` | **Turn-complete signal.** One per CLI turn. |
| `control_request` | `parseControlRequest` | Bidirectional. Inbound: `can_use_tool`, `exit_plan_mode`. Outbound (client → CLI): `interrupt`, `stop_task`. |
| `rate_limit_event` | `parseRateLimitEvent` | Rate limit state changes. |

Unknown `type` values are dropped silently by the dispatcher, logged
with the raw line for diagnosability. Every envelope carries
`session_id` and `uuid` at the top level (for observability / replay
correlation); those are not reproduced in the shape examples below.

---

## `result` — turn-complete signal

**Fires**: exactly once per CLI turn, after all tool round-trips and
the final assistant message have settled.
**Authoritative for**: the `EventTurnComplete` signal.

```json
{
  "type": "result",
  "subtype": "success",
  "is_error": false,
  "duration_ms": 31469,
  "duration_api_ms": 26359,
  "num_turns": 4,
  "stop_reason": "end_turn",
  "result": "The background command's output file contained exactly: `BGDONE-BASH`...",
  "total_cost_usd": 0.1103,
  "usage": {
    "input_tokens": 26,
    "cache_creation_input_tokens": 20835,
    "cache_read_input_tokens": 39828,
    "output_tokens": 1310,
    "server_tool_use": {"web_search_requests": 0, "web_fetch_requests": 0},
    "service_tier": "standard"
  },
  "modelUsage": {
    "claude-sonnet-4-6": {
      "inputTokens": 26, "outputTokens": 1310,
      "cacheReadInputTokens": 39828, "cacheCreationInputTokens": 20835,
      "costUSD": 0.10980765,
      "contextWindow": 200000, "maxOutputTokens": 32000
    }
  },
  "permission_denials": [],
  "terminal_reason": "completed",
  "api_error_status": null
}
```

### `subtype` values
- `"success"` — turn ended normally
- `"error_during_execution"` — runtime error (see interrupted note)
- `"error_max_turns"` — auto-turn cap reached
- `"error_max_budget_usd"` — cost cap reached

### `stop_reason` values

Mirrors the Anthropic API. Observed: `"end_turn"`, `"max_tokens"`,
`"tool_use"`, `"stop_sequence"`, `"refusal"`, `"pause_turn"`.
**`"interrupted"` is NOT a value** — interruption surfaces as
`subtype: "error_during_execution"` + `is_error: false` +
`errors[]` containing `"aborted"` / `"interrupted"`
(forge `sdkMessageParsing.ts:112-125`).

### Fields the SDK exposes that we should capture
- `total_cost_usd` (NOT `cost_usd`)
- `usage` — cumulative turn token/cost accounting; do not use its
  aggregate token counts as current context-window occupancy
- `modelUsage[model].contextWindow` — authoritative max context, use
  this instead of assuming `200_000`
- `permission_denials: []` — list of declined tool calls
- `errors: []` — present when `is_error` or on interrupt

### Context-window usage

Claude exposes two distinct context-related surfaces:

- Passive stream usage on top-level assistant API responses:
  `assistant.message.usage` and, with partial messages enabled,
  `stream_event.event.type == "message_delta"` `usage`.
- Active `/context` parity via an inbound `control_request` with
  `request.subtype == "get_context_usage"`.

For the chat context meter, the verified Claude Code 2.1.118 signal is
the latest **top-level** `message_delta.usage` with no
`parent_tool_use_id`, using:

```text
input_tokens + cache_creation_input_tokens + cache_read_input_tokens
```

Do not include `output_tokens` for current context occupancy. In the
2026-04-29 spike, this value was `15167`, exactly matching
`get_context_usage.totalTokens`. Adding output produced `15197`, while
`result.usage` input/cache totals produced `29888` because `result.usage`
accumulated multiple API calls in the turn.

Do not update the parent chat meter from Agent/Task side signals:
`system.task_notification.usage`, `user.tool_use_result.usage`, or any
assistant/stream event carrying `parent_tool_use_id`. Those belong to
the subagent's private context/cost accounting.

`get_context_usage` is the canonical `/context` breakdown and returns
`totalTokens`, `maxTokens`, `rawMaxTokens`, categories, and `apiUsage`.
Use it when exact category parity is needed; otherwise the passive
top-level `message_delta.usage` is enough for the live meter.

Captured reference:
`fixtures/claude/context_usage_spike_20260429.summary.json`.

Other captured usage-adjacent signals worth preserving for future UI:

| Signal | Future use | Context-meter rule |
| --- | --- | --- |
| `assistant.message.usage` | Fallback if partial `message_delta` events are unavailable; useful for showing per-response usage once an assistant envelope arrives. | Top-level only, and prefer `message_delta` because assistant envelopes can be earlier snapshots. |
| `stream_event.event.type == "message_start"` `message.usage` | Early API-response usage snapshot, useful for diagnostics or "request started" telemetry. | Do not treat as settled context usage. |
| `stream_event.event.type == "message_delta"` `usage` | Best passive live/settled top-level context signal; use latest top-level delta for the chat meter. | Use input + cache creation + cache read, excluding output. |
| `result.usage.iterations[-1]` | Historical diagnostic only. It mirrored the final top-level `message_delta.usage` in one spike, but Claude documents `result.usage` as SDK cost/usage accounting, not the statusline context signal. | Do not drive the context meter from this. |
| `result.usage` | Per-turn API-call/cost accounting. Good for "tokens spent this turn" or billing diagnostics. | Never use aggregate totals for current context occupancy. |
| `result.modelUsage[model]` | Per-model accounting across top-level and subagent/internal calls; `contextWindow` is a useful max-window hint. | Token totals are spend/accounting, not used context. |
| `system.task_notification.usage` | Subagent/background-task progress or row-level token display. | Subagent-private accounting; do not update parent meter. |
| `user.tool_use_result.usage` and `tool_use_result.totalTokens` | Completed Agent/Task details and subagent cost display. | Subagent-private accounting; do not update parent meter. |
| `control_response` for `get_context_usage` | Canonical `/context` parity: exact `totalTokens`, `maxTokens`, category breakdown, and `apiUsage`; useful on resume/start or for audits. | Use `totalTokens` directly when actively requested. |
| `context_management.applied_edits` | Potential future compaction/context-edit visualization; observed as empty in this spike. | Bookkeeping only. |

### ⚠ No `assistant_message_id` on this envelope

The `result` envelope **does not carry** the final assistant message's
id. Track it from the last `assistant` envelope's `message.id` seen
in-stream; the final text content is mirrored into `result.result` as
a convenience string only.

---

## `system/init`

**Fires**: once at session start, and again on model switch.

```json
{"type": "system", "subtype": "init",
 "cwd": "/private/tmp/...",
 "session_id": "ef86676d-...",
 "model": "claude-sonnet-4-6",
 "tools": ["Task", "Bash", "Edit", ...],
 "mcp_servers": [{"name": "playwright", "status": "connected"}],
 "permissionMode": "bypassPermissions",
 "slash_commands": [...],
 "claude_code_version": "2.1.112",
 "output_style": "daily-driver",
 "apiKeySource": "none"}
```

Emits `EventInit`. Parser extracts model id for usage pricing.

---

## `system/task_started`

**Fires**: for **every** Bash and Task invocation, backgrounded or
foreground. It's NOT a "backgrounded-only" signal.

```json
{"type": "system", "subtype": "task_started",
 "task_id": "bslbv9989",
 "tool_use_id": "toolu_015s9XtK1RXLBS1AtHF79Dyy",
 "description": "Background: sleep 4s then echo sentinel",
 "task_type": "local_bash"}
```

### `task_type` values (observed, open-ended)
- `"local_bash"` — Bash tool (backgrounded or foreground)
- `"local_agent"` — Task subagent tool
- `"background"` — generic (from SDK test fixture)

Don't branch on exact values; treat as a classification hint.

### Subagent extension

For `task_type: "local_agent"`, the envelope also carries:
- `prompt` — the subagent's initial prompt (non-SDK, observed in forge)

### Parser action
The adapter emits a meta-update `EventToolStart` carrying
`task_id` in meta so triage can persist the
`task_id ↔ tool_use_id` mapping onto the existing `tool_call` row —
needed for reconnect correlation.

---

## `system/task_updated`

**Fires**: when a task's status transitions, including terminal.
**Authoritative for**: `EventBackgroundTaskTerminal` (when status is
terminal).

⚠ This shape is **NOT modeled in the Python SDK** — it falls through
to the generic `SystemMessage(subtype, data)` path
(`message_parser.py:195-199`). Read `data["patch"]` yourself.

```json
{"type": "system", "subtype": "task_updated",
 "task_id": "bslbv9989",
 "patch": {
   "status": "completed",
   "end_time": 1776577311261
 }}
```

### Terminal `patch.status` values
`"completed"`, `"failed"`, `"killed"`. Map all three to terminal;
non-terminal statuses (`"pending"`, `"running"`) may appear in
intermediate patches and should be treated as no-ops.

### Optional `patch` fields
- `end_time` (ms epoch) — completion timestamp
- `total_paused_ms` — total time the task was paused
- `is_backgrounded` (snake_case on wire) — whether the task ran in
  background mode; forge maps to `isBackgrounded`
- `error` — failure message when `status == "failed"`
- `description` — human-readable description, may change

### What `task_updated` does NOT carry

Fresh app-style spikes in `docs/references/fixtures/claude/`
confirmed that terminal `task_updated` is only a lifecycle update for
ordinary background Bash tasks:

- successful background Bash: `patch.status="completed"`,
  `patch.end_time`, no `exitCode`, no `output`, no `output_file`
- failed background Bash: `patch.status="failed"`, `patch.end_time`,
  no exit code, no output text, no output file path
- `TaskOutput(block=true)` while the task is still running does NOT
  replace this signal; the observed order was `task_updated` first,
  then the TaskOutput `tool_result`

Use `task_updated` to decide that a background task reached terminal
state. Do not expect it to contain the command/subagent result body.

### `tool_use_id` resolution

The envelope **does not reliably carry `tool_use_id`** — resolve via
the per-parser `taskToolUses[task_id]` map populated from
`task_started`. On a fresh parser (reconnect), the map is empty; fall
back to `items.meta.task_id` lookup at the triage layer.

### Timing vs turn-complete

`task_updated` can arrive BEFORE, CONCURRENT WITH, or AFTER the
owning turn's `result` envelope. A backgrounded task that runs longer
than its launching turn will emit `task_updated` after `result`.
Triage writes the `tool_completion` sibling row whenever it arrives,
at the current thread write head when one exists; it is never blocked
on the launch turn's state. See
[`turn-lifecycle.md`](../architecture/turn-lifecycle.md) for rules.

---

## `system/task_notification`

**Fires**: for every Bash/Task terminal (foreground AND background).
Fires even for trivial foreground commands with `output_file: ""`.

```json
{"type": "system", "subtype": "task_notification",
 "task_id": "bwh4ptwpo",
 "tool_use_id": "toolu_01NoZSorBGb7jSQMhNrs6qZj",
 "status": "completed",
 "output_file": "/private/tmp/claude-502/.../tasks/bwh4ptwpo.output",
 "summary": "Background command \"...\" completed (exit code 0)"}
```

### `status` values
`"completed"`, `"failed"`, `"stopped"` (per Python SDK
`TaskNotificationMessage`).

### ⚠ NOT a completion signal

Do not route `task_notification` through the task lifecycle.

**Rationale**: `task_updated` carries the authoritative task-terminal
signal (with richer `patch` content). `task_notification` is an
"attention signal" intended to nudge the next user turn's prompt with
a human-readable summary. Treating it as a completion source
introduces race conditions (dedupe against `task_updated`) and adds
state to the parser for no correctness benefit.

`task_notification` DOES carry useful notification material:

- `summary` — user-friendly completion text, often including exit code
- `output_file` — path to the full task output file
- `tool_use_id` — usually present inline

If the UI surfaces this, emit a distinct notification event/row. Do
not conflate it with `EventBackgroundTaskTerminal`, do not use it to
set completion state, and do not dedupe task lifecycle against it.

Fresh app-style spikes also showed that, when the CLI process stays
alive after the turn, `task_notification` can be followed by an
assistant message/result generated for the agent, e.g. "Background
task ... completed (exit code 0)." That follow-up text is agent-visible
conversation, not a replacement for the task lifecycle.

---

## `user` message — `tool_result` blocks

`user`-type envelopes echo `tool_result` blocks keyed by the original
`tool_use_id`. Three distinct shapes.

### ⚠ Universal rule: always emit `EventToolComplete` for `tool_use_id`

Every `tool_result` on the wire must emit exactly one
`EventToolComplete` keyed by its own `tool_use_id`. This is the
universal **tool lifecycle invariant** (see
[`invariants.md`](../architecture/invariants.md)). Task-related
enrichments (below) layer on top — they do NOT replace the completion.

### E1 — Standard inline tool (Read, Grep, etc.)

```json
{"type": "user",
 "message": {
   "role": "user",
   "content": [{
     "type": "tool_result",
     "tool_use_id": "toolu_01LCMPzpxsdsVULRDTztqNn4",
     "content": "1\tBGDONE-BASH\n2\t"
   }]
 },
 "parent_tool_use_id": null,
 "tool_use_result": {
   "type": "text",
   "file": {
     "filePath": "/.../bslbv9989.output",
     "content": "BGDONE-BASH\n",
     "numLines": 2, "startLine": 1, "totalLines": 2
   }
 }}
```

- `content` is `str | list[{type: "text", text} | {type: "image", ...}] | None`.
- `is_error: true` signals tool failure; absence means not an error.
- `tool_use_result` is an optional top-level sibling with richer
  structured data. Per-tool shape (Bash: `exit_code/stdout/stderr`,
  Edit: `filePath/oldString/newString/structuredPatch`). Can arrive as
  single object, object keyed by tool_use_id, or array — see
  `indexToolUseResults` in `parse_user.go` (see the function defined
  at line 382+).

### E2 — Backgrounded Bash placeholder

**Fires ~instantly** after the tool_use with
`input.run_in_background: true`.

```json
{"type": "user",
 "message": {
   "role": "user",
   "content": [{
     "tool_use_id": "toolu_015s9XtK1RXLBS1AtHF79Dyy",
     "type": "tool_result",
     "content": "Command running in background with ID: bslbv9989. Output is being written to: /.../bslbv9989.output",
     "is_error": false
   }]
 },
 "tool_use_result": {
   "stdout": "", "stderr": "",
   "interrupted": false, "isImage": false,
   "noOutputExpected": false,
   "backgroundTaskId": "bslbv9989"
 }}
```

Marker: `tool_use_result.backgroundTaskId` present.

**Parser behavior**: DOES emit `EventToolComplete` for the tool's
own id (universal invariant). Per agent-overflow spec, triage keeps
the `tool_call` row at `status='running'` for backgrounded tools —
the sibling `tool_completion` row comes later via the task lifecycle.
See [`turn-lifecycle.md`](../architecture/turn-lifecycle.md).

### E3 — TaskOutput `tool_result`

TaskOutput is a regular tool. Its invocation emits a normal
`tool_use`, and its result arrives as a `tool_result` block keyed by
TaskOutput's `tool_use_id`. The `tool_use_result` sibling carries
terminal info about the task being polled.

```json
{"type": "user",
 "message": {"role": "user", "content": [{
   "tool_use_id": "<TaskOutput_tool_use_id>",
   "type": "tool_result",
   "content": "Task completed"
 }]},
 "tool_use_result": {
   "retrieval_status": "success",
   "task": {
     "task_id": "bazncp4aq",
     "task_type": "local_bash",
     "status": "completed",
     "description": "Sleep 20 seconds then print done",
     "output": "done\n",
     "exitCode": 0
   }
 }}
```

### ⚠ `exitCode` vs `exit_code`

TaskOutput's `task.exitCode` is **camelCase**. Regular Bash's
`tool_use_result.exit_code` is snake_case. `readIntAtAnyKey` in
`internal/provider/claude/json_helpers.go` already handles both.

### Parser behavior

1. **Always** emit `EventToolComplete` for TaskOutput's own
   `tool_use_id` (universal invariant).
2. **Additionally**, if `tool_use_result.task.status` is terminal,
   emit `EventBackgroundTaskTerminal` for the backgrounded task's
   original `tool_use_id` (resolved via
   `taskToolUses[task.task_id]`). Carries exit_code, output_file,
   output payload — this is the **richer enrichment** signal on top of
   whatever `task_updated` wrote.

Triage's sibling-row upsert is idempotent; task_updated + TaskOutput
for the same task is expected and handled by the store layer.

### Retention and ordering

TaskOutput is an explicit tool call made by the agent. It is not a
durable task-history lookup.

Fresh app-style spikes in `docs/references/fixtures/claude/`
confirmed:

- If the agent calls `TaskOutput(block=true)` while the background task
  is still running, Claude emits terminal `task_updated` and then the
  TaskOutput `tool_result`.
- In those immediate TaskOutput cases, no `task_notification` was
  observed in the captured window; the TaskOutput result carried the
  output and exit code.
- If the task has already completed and its `task_notification` has
  been delivered, a later `TaskOutput` call can return
  `<tool_use_error>No task found with ID: ...</tool_use_error>` with
  `tool_use_result` as an error string.

So the lifecycle rule is:

- `task_updated` is enough to mark the background task done/failed.
- `TaskOutput` may add rich details because the agent explicitly asked
  for them.
- `task_notification.output_file` is the durable handle after task
  cleanup; TaskOutput cannot be assumed available later.

### E4 — Orphan `tool_result`

A `tool_result` block whose `tool_use_id` has no corresponding
`tool_use` earlier in the stream. **Drop it silently** — we do not
fabricate a ghost tool_call row. In practice this only happens in
malformed replays.

---

## `assistant` envelope

### Top-level fields (outside `message`)

```json
{"type": "assistant",
 "message": { /* Anthropic Messages API message shape */ },
 "parent_tool_use_id": null,
 "error": null,
 "session_id": "...", "uuid": "..."}
```

### `parent_tool_use_id` — subagent streams

When a `Task` subagent is running, its assistant messages land on
the **parent session's** NDJSON stream. They're distinguished by
`parent_tool_use_id` (top-level) pointing at the parent Task's
`tool_use_id`. `session_id` stays the PARENT's id — there is no
per-subagent session multiplexing at the wire layer.

This applies to `user` (subagent tool_results) and `stream_event`
envelopes as well — `parent_tool_use_id` is always top-level.

### `message` sub-object

Standard Anthropic Messages API shape:
- `id` — message id. **Track this** to resolve
  `latest_turn.assistant_message_id` in the `turns` row (see
  [`turn-lifecycle.md`](../architecture/turn-lifecycle.md)).
- `role: "assistant"`, `model`, `type: "message"`
- `content: []` — array of blocks:
  - `{type: "text", text}`
  - `{type: "thinking", thinking, signature}`
  - `{type: "tool_use", id, name, input, caller}`
  - `{type: "exit_plan_mode", ...}` — plan tool invocation
- `stop_reason`, `stop_sequence`, `stop_details` — per-assistant-message
  (distinct from turn-level `result.stop_reason`)
- `usage: {input_tokens, output_tokens, cache_*_tokens, ...}`
- `error: AssistantMessageError | null` — top-level envelope field, NOT
  inside `message`. Literal set:
  `authentication_failed | billing_error | rate_limit | invalid_request | server_error | unknown`

### Input streaming (`run_in_background: true` hint)

`input` on a tool_use block is the final assembled JSON. To see the
input being streamed token-by-token, enable
`CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING` — causes
`stream_event` envelopes with `input_json_delta` deltas. Not
currently handled in our parser.

---

## `stream_event` — streaming deltas

**Only emitted when** `include_partial_messages: true` is set on the
session options. Passes through the Anthropic Messages API SSE payload
verbatim.

```json
{"type": "stream_event",
 "parent_tool_use_id": null,
 "event": {
   "type": "content_block_delta",
   "index": 0,
   "delta": {"type": "text_delta", "text": "hello"}
 }}
```

### Inner `event.type` values
- `message_start` — opens an assistant message
- `content_block_start` — `{index, content_block: {type: "text"|"thinking"|"tool_use", ...}}`
- `content_block_delta` — `{index, delta: {type: ..., ...}}`
- `content_block_stop` — `{index}`
- `message_delta` — mid-message `stop_reason` / `usage` updates
- `message_stop` — closes an assistant message

### Delta types
- `text_delta: {text}` — ✅ handled
- `thinking_delta: {thinking}` — ✅ handled
- `signature_delta: {signature}` — ❌ unhandled (thinking signature)
- `input_json_delta: {partial_json}` — ❌ unhandled (fine-grained tool streaming)

### `message_stop` vs `result`

`message_stop` is per-assistant-message (one per assistant turn in
the API stream). `result` is per-CLI-turn (fires once, after the
final `message_stop` and any trailing tool round-trips settle).
**`result` is authoritative for turn end, not `message_stop`.**

---

## `control_request`

Bidirectional control channel shared by the CLI and the client. The CLI
emits inbound control_requests for approval (`CanUseTool`) and
`exit_plan_mode`; the client can emit outbound control_requests to
interrupt a turn or stop a backgrounded task. Responses use
`control_response` envelopes keyed by `request_id`.

### Inbound (CLI → client)

Handled by `parse_control.go`, gated behind a `bytes.HasPrefix` check in
`session.readLoop` so streaming text/thinking lines don't pay a second
`json.Unmarshal`.

- `subtype: "can_use_tool"` — approval for a tool invocation. Includes
  `tool_name`, `input`, optional `permission_suggestions`,
  `updatedInput` / `updatedPermissions` for the approval round-trip.
- `subtype: "exit_plan_mode"` — plan-mode exit signal with proposed
  plan markdown.

### Outbound (client → CLI)

The CLI accepts several control_request subtypes on the stdio
`--input-format stream-json` channel. The full schema list lives in the
CLI binary; the subtypes we use or plan to use:

- `subtype: "interrupt"` — abort the current turn. No additional params.
- `subtype: "stop_task"` — kill a specific backgrounded task (Bash with
  `run_in_background:true` OR Task subagent). Takes `task_id` (the id
  from `system/task_started`). See [§stop_task](#stop_task) below.

### Response envelope

Both directions share the same response shape:

```json
{
  "type": "control_response",
  "response": {
    "subtype": "success",
    "request_id": "<id from the request>",
    "response": { ... optional subtype-specific payload ... }
  }
}
```

Error form:

```json
{
  "type": "control_response",
  "response": {
    "subtype": "error",
    "request_id": "<id>",
    "error": "<human message>"
  }
}
```

### stop_task

**Wire shape (request):**

```json
{
  "type": "control_request",
  "request_id": "caller-unique-id",
  "request": {
    "subtype": "stop_task",
    "task_id": "<id from system/task_started>"
  }
}
```

**Wire shape (response):**

```json
{"type":"control_response",
 "response":{"subtype":"success","request_id":"caller-unique-id","response":{}}}
```

**Follow-up notification:** the CLI fires `system/task_updated` with
`patch.status: "killed"` for that `task_id`:

```json
{"type":"system","subtype":"task_updated",
 "task_id":"<same id>",
 "patch":{"status":"killed","end_time":<unix ms>}}
```

Unifies across task types — `task_started.task_type` is `local_bash`
for a backgrounded Bash, `local_agent` for a Task subagent, but
`stop_task` accepts any of them because they share the same task
registry. The deprecated `shell_id` parameter is aliased to `task_id`
in the CLI; use `task_id` only.

**Verified via spike on Claude CLI 2.1.112** — spawn with
`--print --input-format stream-json --output-format stream-json
--permission-mode bypassPermissions`, send a user message prompting a
backgrounded `sleep`, capture the `task_started.task_id`, write the
request above to stdin. Response lands immediately; `task_updated`
with `status:killed` follows within a few ms. This is the primitive
that powers per-item stop and "Stop all" for Claude background
tasks in the [BackgroundTaskTray](../architecture/chat-rewrite.md).

---

## `rate_limit_event`

```json
{"type": "rate_limit_event",
 "rate_limit_info": {
   "status": "allowed_warning",
   "resetsAt": 1776981600,
   "rateLimitType": "seven_day",
   "utilization": 0.51,
   "isUsingOverage": false
 }}
```

camelCase wire fields (`resetsAt`, `rateLimitType`, etc.). See
`parse_rate_limit.go`.

---

## Captured samples

All three captures are real wire output from claude-code CLI version
2.1.112 (Sonnet 4.6) with `stream-json` mode. They ARE the
authoritative test fixtures for parser refactor work — do not
fabricate shapes when these samples cover the scenario.

### `docs/references/fixtures/claude/ndjson_bash.log`
**Scenario**: one `run_in_background:true` Bash + one foreground
`sleep 6` + one `Read` of the output file.

**Shapes covered**: `system/init`, `assistant` (thinking + tool_use
with `run_in_background`), `system/task_started` (backgrounded +
foreground), `user` backgrounded placeholder (E2), `system/task_updated`
terminal, `system/task_notification`, `user` inline tool_result (E1),
`result` envelope.

### `docs/references/fixtures/claude/ndjson_task.log`
**Scenario**: `Task` subagent with `run_in_background: true` + `TaskOutput`
retrieval.

**Shapes covered**: `system/task_started` with `task_type: "local_agent"`
and `prompt`, `user` TaskOutput tool_result (E3),
`assistant` streams with `parent_tool_use_id` for the subagent.

### `docs/references/fixtures/claude/ndjson_outlives.log` + `ndjson_outlives_turn2.log`
**Scenario**: backgrounded Bash that outlives its launching turn —
first turn's `result` arrives BEFORE the task's `task_updated`.

**Critical for**: the "turn completes with tasks still running"
invariant. The turn closes normally; terminal signals for the
backgrounded task land afterward on the stream (they address the same
`thread_id` via `session_id` correlation).

### `docs/references/fixtures/claude/taskoutput_multi.ndjson`
**Scenario**: two parallel `run_in_background:true` Bashes + one
blocking `TaskOutput` on the longer one.

**Shapes covered**: two independent `task_started` envelopes,
two `task_updated` terminals for different `task_id`s, single
TaskOutput `tool_result` enrichment for one of them.

### `docs/references/fixtures/claude/interactive_outlives_taskoutput_monitor.ndjson`
**Scenario**: app-style long-lived CLI process. Turn 1 launches a
background Bash and ends immediately. The process stays alive long
enough to receive `task_updated` + `task_notification`. A later turn
tries `TaskOutput` for the completed task. Another turn launches a
second background Bash, then a later turn uses `Monitor` against the
notification output file.

**Shapes covered**: `task_updated` after `result`,
`task_notification` carrying `output_file`, later `TaskOutput` failure
for an already-cleaned task, and `Monitor` as a separate background
task that reads the output file.

**Critical for**: TaskOutput retention. Once the completed task has
been notified/cleaned, TaskOutput can return "No task found"; do not
rely on TaskOutput as durable history.

### `docs/references/fixtures/claude/blocking_taskoutput.ndjson`
**Scenario**: background Bash sleeps for 10 seconds, then the agent
immediately calls `TaskOutput(block=true)` while it is still running.

**Shapes covered**: `task_updated` fires before the TaskOutput
`tool_result`; TaskOutput carries `task.output` and `task.exitCode`.

### `docs/references/fixtures/claude/blocking_taskoutput_failure.ndjson`
**Scenario**: same as above, but the background Bash exits with code 7.

**Shapes covered**: failed `task_updated` has only
`patch.status="failed"` + `end_time`; TaskOutput carries `exitCode: 7`
and merged stdout/stderr in `task.output`.

### `docs/references/fixtures/claude/plain_failure_outlives.ndjson`
**Scenario**: failed background Bash without TaskOutput retrieval.

**Shapes covered**: `task_updated` terminal plus
`task_notification.summary` containing exit code and
`task_notification.output_file` containing the durable output path.

### Using samples in tests

```go
// Suggested helper — each fixture line becomes one ParseLine call
func loadCapturedFixture(t *testing.T, path string) []string {
    data, err := os.ReadFile(path)
    if err != nil { t.Fatalf("fixture: %v", err) }
    return strings.Split(strings.TrimSpace(string(data)), "\n")
}
```

Replay the lines through `(*Parser).ParseLine(threadID, line)` and
assert the emitted `ProviderEvent` sequence. These fixtures live in the
repo because the parser/tests/docs depend on them. Refresh them from a
new `AGENT_OVERFLOW_DEBUG=provider` capture when upstream wire behavior
changes, then update the checked-in fixture and this doc together.

---

## Contradictions and ambiguities

**Tracked for future resolution. Don't code against these assumptions
without a fresh spike.**

1. `task_updated.patch.status` closed set — forge treats
   `{completed, failed, killed}` as terminal; `stopped`, `pending`,
   `running` only seen on `task_notification` or intermediate patches.
   Our adapter should keep non-terminal as no-op.
2. `task_type` is open-ended. `local_bash`, `local_agent`,
   `background` confirmed; don't branch on exact values.
3. TaskOutput `task.output_file` is speculative — fresh and forge
   fixtures show durable output paths on `task_notification`, not
   reliably on TaskOutput. Reading it from both is harmless.
4. `exitCode` vs `exit_code` inconsistency — TaskOutput uses
   camelCase on `task`, Bash uses snake_case on `tool_use_result`.
5. `interrupted` is not a `stop_reason` — detect via
   `subtype == "error_during_execution"` + `is_error == false` +
   `errors[]` containing aborted/interrupted.
6. `assistant_message_id` must be tracked from the last `assistant`
   envelope's `message.id`. Not carried on `result`.

---

## When this doc is wrong

Capture fresh NDJSON (`AGENT_OVERFLOW_DEBUG=provider`), compare
against these shapes, and update this file before writing parser
code against a new assumption. This doc is the single source of
truth for parser behavior; if it's stale, code written against it
will be too.
