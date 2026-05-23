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

**Capturing fresh samples**: run `make dev PROVIDER_DEBUG=1` (or
`make dev-wsl PROVIDER_DEBUG=1` on the WSL launcher path), or set
`AGENT_OVERFLOW_DEBUG=provider` directly before launching the app. Raw
stdio lines (pre-parse) land in
`<dbDir>/logs/provider-events-YYYY-MM-DD.ndjson` with RFC3339Nano
timestamps. See `internal/provider/process.go:303-316`.

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
| `control_request` | `parseControlRequest` | Bidirectional. Inbound: `can_use_tool`, `exit_plan_mode`. Outbound (client → CLI): `interrupt`, `stop_task`, `set_permission_mode`, `mcp_set_servers`, `mcp_authenticate`, `mcp_oauth_callback_url`, `mcp_status`. |
| `rate_limit_event` | `parseRateLimitEvent` | Rate limit state changes. |

Unknown `type` values are dropped silently by the dispatcher, logged
with the raw line for diagnosability. Every envelope carries
`session_id` and `uuid` at the top level (for observability / replay
correlation); those are not reproduced in the shape examples below.

---

## `result` — turn-complete signal

**Fires**: exactly once per CLI turn, after all tool round-trips and
the final assistant message have settled.
**Authoritative for**: turn-level token/cost accounting and
`terminal_reason`. The final `assistant_message_id` is derived from
the last in-stream assistant `message.id`; it is not carried on
`result`.

⚠ **Not the only turn-complete signal.** As of Claude Code 2.1.118, when
the parent assistant ends its message with `stop_reason="end_turn"`
while a `local_agent` (Task) subagent is still running in the
background, the CLI **withholds the `result` envelope** until the
subagent completes. The wire-typed parent-ended signal is therefore
`stream_event.message_delta.delta.stop_reason` (with
`parent_tool_use_id == null`) — see
[§stream_event soft-round-close](#stream_event--streaming-deltas)
below. We treat that signal as authoritative for the round-end UI
(working indicator, Stop button, composer block) and absorb the late
`result` envelope as a token-usage / cost / `assistant_message_id`
fold-in. Backed by fixtures
[`local_agent_outlives.ndjson`](fixtures/claude/local_agent_outlives.ndjson)
and
[`local_agent_user_input_during_wait.ndjson`](fixtures/claude/local_agent_user_input_during_wait.ndjson).

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

Per the agent SDK's `SDKResultError` discriminated union the four
documented error subtypes are:

- `"success"` — turn ended normally (may still carry
  `is_error: true` when a prior `assistant.error` flagged the turn —
  see Error envelope shapes below)
- `"error_during_execution"` — runtime error (see interrupted note)
- `"error_max_turns"` — auto-turn cap reached
- `"error_max_budget_usd"` — cost cap reached
- `"error_max_structured_output_retries"` — structured-output
  validator retry cap exhausted

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

For the chat context meter, the verified Claude Code 2.1.139 signal is
the **last `type:"message"` entry inside `message_delta.usage.iterations[]`**
(with no `parent_tool_use_id`), using:

```text
input_tokens + cache_creation_input_tokens + cache_read_input_tokens
```

Do not include `output_tokens` for current context occupancy. When
the array is absent (or there is no `type:"message"` entry), fall
back to the top-level fields on the same envelope — for a single-
parent-call turn the iteration value equals the top-level value, so
the fallback is the safe degenerate path.

#### ⚠ `message_delta.usage` top-level is a cumulative SUM, not a snapshot

`message_delta.usage` reports a top-level cumulative parent-only sum
across every `type:"message"` iteration in the **same SSE message**.
For most tool-using turns (Bash, Task, etc.) that is harmless: the
parent's stream message ends at `tool_use`, a fresh stream message
opens after the tool result returns, and each message_delta carries
exactly one parent iteration so the top-level equals
`iterations[-1]`. **The advisor changes this.**

`server_tool_use(name="advisor")` is a SERVER-side tool — the
advisor runs as a separate model call but the parent's SSE message
does NOT terminate at the advisor block. The parent's text continues
streaming in the same message after `advisor_tool_result` lands.
Result: one SSE message contains N parent API calls
(`type:"message"`) interleaved with M advisor calls
(`type:"advisor_message"`), and the trailing `message_delta.usage`
top-level is the SUM of all N parent iterations. Using the top-level
as a meter reading scales the displayed value by `(N+1)×` overcount
for N advisor calls in the turn (one advisor ⇒ ~2×, two ⇒ ~3×,
scaling linearly because cached tokens created in iter 1 are read
again in iter 3 etc.).

Wire-verified against Claude 2.1.139
(`fixtures/claude/advisor_context_usage_20260522.summary.json`):

| Turn shape | iterations | top-level | last `type:"message"` iter | overcount if using top-level |
| --- | --- | --- | --- | --- |
| control (no tools, no advisor) | `[msg]` | 33329 | 33329 | 1.0× (no-op) |
| single advisor | `[msg, adv, msg]` | 55995 | 28114 | 1.99× |
| double advisor | `[msg, adv, msg, adv, msg]` | 100542 | 33634 | 2.99× |

The implementation lives in
`internal/provider/claude/parse_stream.go::lastParentIterationUsage`.

#### Other rules

Do not update the parent chat meter from Agent/Task side signals:
`system.task_notification.usage`, `user.tool_use_result.usage`, or any
assistant/stream event carrying `parent_tool_use_id`. Those belong to
the subagent's private context/cost accounting.

`get_context_usage` is the canonical `/context` breakdown and returns
`totalTokens`, `maxTokens`, `rawMaxTokens`, categories, and `apiUsage`.
Use it when exact category parity is needed; otherwise the passive
`message_delta.usage.iterations[-1]` is enough for the live meter.

Captured references:
`fixtures/claude/context_usage_spike_20260429.summary.json`
(Bash + Agent subagent, single iteration on message_delta) and
`fixtures/claude/advisor_context_usage_20260522.summary.json`
(control / single advisor / double advisor — the iteration extraction
contract).

Other captured usage-adjacent signals worth preserving for future UI:

| Signal | Future use | Context-meter rule |
| --- | --- | --- |
| `assistant.message.usage` | Fallback if partial `message_delta` events are unavailable; useful for showing per-response usage once an assistant envelope arrives. | Top-level only, and prefer `message_delta` because assistant envelopes can be earlier snapshots. Carries no `iterations[]` — it's a single-call snapshot scoped to the assistant frame. |
| `stream_event.event.type == "message_start"` `message.usage` | Early API-response usage snapshot, useful for diagnostics or "request started" telemetry. | Do not treat as settled context usage. |
| `stream_event.event.type == "message_delta"` `usage` | Best passive live/settled context signal. | Read `iterations[-1]` (last `type:"message"`); fall back to top-level only when `iterations` is absent. Use input + cache creation + cache read, excluding output. |
| `result.usage.iterations[-1]` | Same value as `message_delta.usage.iterations[-1]` on the closing envelope. Useful for replay diagnostics. | Do not drive the live meter from `result`; the trailing message_delta already pushed the right value. |
| `result.usage` (flat) | Per-turn API-call/cost accounting; same cumulative parent-only sum the message_delta top-level carries. Good for "tokens spent this turn" or billing diagnostics. | Never use the flat aggregate for current context occupancy — it is N× inflated whenever the turn had N advisor calls. |
| `result.modelUsage[parent_model]` | Per-model accounting across top-level calls. Carries the same cumulative sum as `result.usage` (flat). `contextWindow` is a useful max-window hint. | Token totals are spend/accounting, not used context — same inflation as `result.usage`. |
| `result.modelUsage[advisor_model]` | Advisor's own per-call usage (separate model run, separate context window). | Subagent-style private accounting; never updates the parent meter. |
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
than its launching turn will emit `task_updated` after `result`. See
[`turn-lifecycle.md`](../architecture/turn-lifecycle.md) for rules.

### Stash on `task_updated` — sibling on agent observation

For `patch.status` in `{completed, failed}`, triage stashes the
terminal in `pending_background_task_terminals` (PK
`(thread_id, task_id)`) and emits
`provider:background_task_state{state:"exited"}`. The chat-side
`tool_completion` sibling is **not** written here — it lands later
when the agent observes via `task_notification` or a TaskOutput
`tool_result` (the stash is drained at that point). This decouples
the tray ("process state — is it still running?") from the chat
("agent observation state — has the model seen it complete?"). See
[`turn-lifecycle.md §Tray decoupling`](../architecture/turn-lifecycle.md#tray-decoupling--process-state-vs-agent-observation-tray-a).

`patch.status="killed"` is the carve-out: it only appears as the CLI's
reply to a user-initiated `stop_task` control_request. The user
already knows the process was stopped, so triage skips the stash and
writes the `tool_completion{status:"killed"}` sibling immediately.

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

### Synthetic-XML delivery channel (concurrent-tool path)

When a backgrounded subagent (Task / Bash with `run_in_background:true`)
completes WHILE a concurrent foreground `tool_result` is in flight,
the CLI does NOT emit a structured `system/task_notification`
envelope for the backgrounded task. The completion observation is
delivered ONLY as inline XML inside the next `user{isReplay:true}`
envelope's content — `LocalShellTask.tsx:160-165` wraps the queued
attachment via `wrapCommandText('task-notification', ...)`:

```
A background agent completed a task:
<task-notification>
<task-id>...</task-id>
<tool-use-id>...</tool-use-id>
<status>completed</status>
<output-file>...</output-file>
<summary>...</summary>
</task-notification>
```

The 5s-subagent-alone scenario (no concurrent foreground tool) emits
the structured envelope as documented above and never the inline XML.
The two channels are mutually exclusive in practice.

`internal/provider/claude/parse_user_replay.go` extracts the inner
fields out of the suppressed `isReplay` envelope before discarding it
and emits `EventBackgroundTaskNotification` with the same meta shape
the structured-envelope path produces, so triage's stash-drain ->
`tool_completion` sibling write runs in either case. Without this
parser fallback, the launch row stays `running` indefinitely. See
`internal/provider/claude/CLAUDE.md` §Synthetic XML extraction.

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

## Error envelope shapes

API failures land on three loosely-coupled shapes. Both providers
must emit them in roughly this order; the parser pins the surfaces
so a future SDK schema change is a visible code change rather than a
silent UI freeze.

### `system.api_retry` - retry progress

While Claude retries an overloaded or transiently-failed provider
request, it emits `system` envelopes with `subtype:"api_retry"`.
Current Claude Code builds put retry fields at the top level:
`attempt`, `max_retries`, `retry_delay_ms`, `error_status`, `error`,
`session_id`, and `uuid`. Older/alternate builds wrapped equivalent
fields under `data`. Agent Overflow normalizes both shapes into
`EventAPIRetry` metadata so the timeline row can render the latest
retry attempt and status.

### `assistant.error` — closed-set enum on the assistant envelope

When the API rejects a prompt mid-turn, the SDK populates the
top-level `error` field on the `assistant` envelope (alongside the
existing `message` content). The string is a closed enum from the
agent SDK:

| Enum | Meaning |
|---|---|
| `authentication_failed` | OAuth/key invalid; user needs to `/login` |
| `billing_error` | Payment / org-level billing problem |
| `rate_limit` | Quota exhausted on the request's model |
| `invalid_request` | Malformed request — usually a prompt-too-long carve-out |
| `server_error` | 5xx from the Anthropic API |
| `max_output_tokens` | The model emitted enough tokens to hit the cap |
| `unknown` | SDK fallback for anything outside the closed set |

Older/alternate SDK shapes may put the same enum under
`message.error`; the parser treats that as a compatibility fallback
while preferring the documented top-level field.

Per the agent SDK source, `assistant.error` is followed by a
`result{is_error:true}` envelope that closes the turn through the
normal wire path. Parser emits the assistant.error as a fatal
`EventError` tagged `expect_turn_complete:true` so triage waits for
the wire turn-complete instead of synthesizing a duplicate.

### `result` error subtypes — `SDKResultError`

The Python agent SDK's `SDKResultError` discriminator names four
subtypes the result envelope can carry:

- `error_during_execution` — runtime error (also the carrier for
  user-aborted turns; see `interrupted` heuristic below)
- `error_max_turns` — auto-turn cap reached
- `error_max_budget_usd` — cost cap reached
- `error_max_structured_output_retries` — structured-output
  validator retry cap exhausted

`subtype:"success"` with `is_error:true` is the carve-out for "the
API call succeeded as a transport but the assistant flagged the
turn" — the assistant.error path produces this exact shape.

### `terminal_reason` — telemetry-only enum (12 values)

Some result envelopes carry an additional `terminal_reason` field
naming the precise wire-level reason the SDK terminated the turn:

`end_turn`, `max_tokens`, `tool_use`, `stop_sequence`, `pause_turn`,
`refusal`, `cancelled`, `interrupted`, `aborted`, `timeout`,
`network_error`, `unknown`.

The parser keeps this on the raw line for replay/debug. It is not
carried into normalized turn-complete metadata, and triage does NOT
branch on it. The actionable signals are `subtype` and `is_error`.

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
- `signature_delta: {signature}` — ❌ unhandled (thinking signature; flows
  even when the `thinking` text is redacted — see §Extended thinking)
- `input_json_delta: {partial_json}` — ❌ unhandled (fine-grained tool streaming)

### `message_stop` vs `result`

`message_stop` is per-assistant-message (one per assistant turn in
the API stream). `result` is per-CLI-turn (fires after the final
`message_stop` and any trailing tool round-trips settle). `result`
remains authoritative for the cumulative turn payload (token usage,
cost, terminal_reason) — but it is
**not** the only signal that the round has ended; see soft-round-close
below.

### Soft round close — `message_delta.stop_reason`

The wire-typed signal that the parent assistant has stopped emitting
content for the current round is
`stream_event.message_delta.delta.stop_reason`, **gated on
`parent_tool_use_id == null`** to exclude subagent messages. Treat
the following stop_reasons as round-end:

- `"end_turn"` — model decided it was done
- `"stop_sequence"` — model emitted a configured stop sequence
- `"refusal"` — content policy refusal

Do **not** treat these as round-end:

- `"tool_use"` — model paused to call a tool; more text follows the
  tool_results in the same round
- `"pause_turn"` — model explicitly asked for more time
- `"max_tokens"` — model truncated; the harness may auto-continue

Why this matters: with a `local_agent` subagent in flight, the CLI
withholds `result` until the subagent completes. Without consuming
the message_delta signal, the working indicator stays on for the
duration of the subagent runtime even though the parent is idle.

The top-level `assistant` envelope's `message.stop_reason` is `null`
in both partial and final snapshots — only the partial-messages
`message_delta` event carries the actual model stop_reason. Without
`--include-partial-messages`, this signal is unreachable; the
adapter always sets that flag (see `parse_stream.go`).

See [`docs/architecture/turn-lifecycle.md`](../architecture/turn-lifecycle.md)
for how triage absorbs the soft signal and the trailing `result`
envelope idempotently.

---

## Extended thinking — opting in via `--thinking-display`

Captured against `claude --version` 2.1.132, OAuth-subscription auth.
The investigation log lives in
[`fixtures/claude/opus47_thinking_summary.json`](fixtures/claude/opus47_thinking_summary.json)
and the two raw NDJSON fixtures
[`opus47_thinking_redacted.ndjson`](fixtures/claude/opus47_thinking_redacted.ndjson)
and
[`opus47_thinking_summarized.ndjson`](fixtures/claude/opus47_thinking_summarized.ndjson).

### Two independent knobs

The Anthropic API treats these as **orthogonal**:

- `thinking.type` — whether and how the model thinks
  (`adaptive` / `enabled` / `disabled`). CLI flag: `--thinking`.
- `thinking.display` — how the resulting thinking is surfaced on the
  wire (`summarized` / `omitted`). CLI flag: `--thinking-display`.

The CLI flags map 1:1 to those API fields. Both are **hidden from
`claude --help`** but the binary validates the choice set. The
display knob is the one that controls whether `thinking` text reaches
the parser; the mode knob controls the underlying reasoning behavior.

### Default behavior per model

Same prompt, same flags as `session.go#buildArgs`, only the model differs:

| Model | API default for `thinking.display` | `thinking` text on wire | `thinking_delta` events |
|---|---|---|---|
| `sonnet` / `claude-sonnet-4-6` | `summarized` | populated | yes |
| `claude-opus-4-6` | `summarized` | populated | yes |
| `claude-opus-4-7` (current Opus) | **`omitted`** | **empty** (signature only) | **none** |

Opus 4.7 still emits a `thinking` content block — the block boundary
and `signature` come through normally — but the `thinking` field is an
empty string and no `thinking_delta` events fire. Anthropic documents
this change explicitly on
[the Opus 4.7 release notes](https://platform.claude.com/docs/en/about-claude/models/whats-new-claude-4-7#thinking-content-omitted-by-default):
display defaults to `omitted` for Opus 4.7 and the caller must
explicitly set `display: "summarized"` to restore text.

### The opt-in flag

Adding `--thinking-display summarized` to the existing invocation
restores the thinking flow on Opus 4.7. It's a no-op for Sonnet 4.6
and Opus 4.6 (those already default to `summarized`).

With the flag set:

- `assistant.message.content[].thinking` is populated.
- `stream_event` emits `thinking_delta` deltas alongside the existing
  `signature_delta` — already wired to `EventThinking` by
  `parse_stream.go`.
- The `signature` is identical regardless of display mode; it carries
  the encrypted full thinking and is required for multi-turn
  continuity. Switching `display` between turns is supported.

What "summarized" means is intentionally fuzzy. The Anthropic public
docs describe it as a *summary* of the model's full thinking, processed
by a different model from the target, and note that for Claude 4 the
raw chain of thought is not returned via the public API (sales-team
contact required). Empirically though, the surfaced text reads as
first-person reasoning with self-corrections, planning, and
uncertainty — see Sonnet 4.6's thinking block in
[`ndjson_bash.log`](fixtures/claude/ndjson_bash.log). Treat the
output as legitimate thinking content for UX purposes; treat the
"summary" framing as the documented API contract, not as evidence
that the text is lossy. Anthropic notes that "the first few lines of
thinking output are more verbose, providing detailed reasoning that's
particularly helpful for prompt engineering purposes."

Billing: you're charged for the FULL underlying thinking tokens, not
the visible characters. `summarized` vs `omitted` only changes what
the wire surfaces, not what you pay.

`--thinking-display omitted` keeps the redacted shape. Setting
`--thinking enabled|adaptive` without a display flag does NOT flip
display; the display knob is the load-bearing one.

### Empirical (same prompt, claude-opus-4-7)

| Extra flags | `thinking` chars | `thinking_delta` events |
|---|---|---|
| none | 0 | 0 |
| `--effort max` | 0 | 0 |
| `--thinking adaptive` | 0 | 0 |
| `--thinking-display omitted` | 0 | 0 |
| `--thinking-display summarized` | 343 | 6 |
| `--thinking adaptive --thinking-display summarized` | 233 | 5 |
| `--thinking enabled --thinking-display summarized` | 326 | 4 |

`--effort` (low / medium / high / xhigh / max) scales the thinking
budget. On Opus 4.7 with display=summarized: low → ~650 chars,
high/max → ~1180 chars on a "prove infinitude of primes" prompt.

### Caveats

- Both flags are **undocumented** in CLI help. They map cleanly to
  the documented API fields, but the CLI surface isn't part of any
  stability contract — they may change or disappear.
- `--betas interleaved-thinking-2025-05-14` (and any other custom
  beta) is rejected with `Custom betas are only available for API key
  users. Ignoring provided betas.` under OAuth auth. The
  `--thinking-display` flag is the only opt-in path on the
  subscription tier.
- This had a regression window: on CLI 2.1.128 the flag was no-op for
  Opus 4.7 (anthropics/claude-code#56356). On 2.1.132 it works as
  documented. Older binaries in the wild may still no-op.

### Adapter implications

Adding `--thinking-display summarized` to `buildArgs` is a one-line
change that unlocks Opus 4.7 thinking while staying a no-op for
Sonnet 4.6 and Opus 4.6 (already default). No parser changes are
required — `parse_stream.go` already routes `thinking_delta` to
`EventThinking`, and `parse_assistant.go` already extracts the
`thinking` content block from the assistant envelope.

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
- `subtype: "set_permission_mode"` — switch the live session's permission
  mode (Plan ↔ chat ↔ accept-edits ↔ bypass). Takes `mode`.
- `subtype: "mcp_set_servers"` — in-process diff-reconcile of the
  live MCP server set against `servers`. Returns
  `{added, removed, errors}`. Used by AO to sync per-thread MCP
  toggles without respawning the session.
- `subtype: "mcp_authenticate"` — start the OAuth handshake for an
  http/sse MCP server. Takes `server_name`. Returns `{authUrl,
  requiresUserAction}`.
- `subtype: "mcp_oauth_callback_url"` — post the captured callback
  URL back to the CLI to finish OAuth when the browser landed
  somewhere other than the CLI's loopback listener. Takes
  `server_name` and `callback_url`.
- `subtype: "mcp_status"` — read-only snapshot of current MCP server
  state. No additional params. See [§mcp_status](#mcp_status) below.

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

### mcp_status

Read-only snapshot of every MCP server the live session has loaded.
No state mutation, no API call, no token cost — the CLI just walks
its three in-memory client pools (`currentMcpClients`, `sdkClients`,
`dynamicMcpState.clients`) and returns each entry's status / config /
tools.

**Wire shape (request):**

```json
{
  "type": "control_request",
  "request_id": "caller-unique-id",
  "request": {
    "subtype": "mcp_status"
  }
}
```

**Wire shape (response):**

```json
{
  "type": "control_response",
  "response": {
    "subtype": "success",
    "request_id": "caller-unique-id",
    "response": {
      "mcpServers": [
        {
          "name": "github",
          "status": "connected",
          "serverInfo": {"name": "github", "version": "1.0"},
          "config": {"type": "stdio", "command": "npx", "args": ["..."]},
          "scope": "user",
          "tools": [{"name": "get_repo", "annotations": {"readOnly": true}}]
        },
        {
          "name": "sentry",
          "status": "needs-auth",
          "config": {"type": "http", "url": "https://example/mcp"},
          "scope": "user"
        },
        {
          "name": "broken",
          "status": "failed",
          "error": "connection refused"
        }
      ]
    }
  }
}
```

`status` is one of `connected | failed | needs-auth | pending | disabled`.
The five-value enum matches `mcpstatus.Status` projection via
`MCPStatusFromRaw` in `internal/provider/claude/mcpstatus.go`.

**Important caveat:** the response is built from in-memory client
pools only. A server that's configured (e.g., in `~/.claude.json`)
but the CLI has never attempted to connect to may be missing from
the array. Polling callers should treat "missing entry" as
"keep retrying," not as a terminal state.

**Used by AO** in the OAuth-completion poller at
`app_mcp_bindings.go:pollClaudeMCPAfterOAuth`. Claude emits no
spontaneous post-OAuth wire envelope (`reconnectMcpServerImpl`
runs inline in `print.ts` and updates state in-process), so AO
polls `mcp_status` after `TriggerMcpAuth` with a 1+2+3+5+8+13s
backoff to detect the `needs-auth → {connected, failed}` flip.
Codex provides the equivalent signal via the
`mcpServer/oauthLogin/completed` notification on its own
session channel — the AO surface (`mcp:oauth-completed` event)
is identical between providers.

**Verified via spike on Claude CLI 2.1.139** — spawn with
`--print --input-format stream-json --output-format stream-json`,
send the request above on stdin BEFORE any user message. The
control_response lands directly without spinning up a turn (no
API call billed). Response shape per the example above.

---

## `rate_limit_event`

```json
// Warning-band envelope — carries a usable `utilization`.
{"type": "rate_limit_event",
 "rate_limit_info": {
   "status": "allowed_warning",
   "resetsAt": 1776981600,
   "rateLimitType": "seven_day",
   "utilization": 0.51,
   "isUsingOverage": false
 }}

// Steady-state envelope during normal usage — `utilization` is OMITTED.
// Claude only populates that field once you cross the warning band.
{"type": "rate_limit_event",
 "rate_limit_info": {
   "status": "allowed",
   "resetsAt": 1777920000,
   "rateLimitType": "five_hour",
   "isUsingOverage": false
 }}
```

camelCase wire fields (`resetsAt`, `rateLimitType`, etc.). See
`parse_control.go` (`parseRateLimitEvent`).

**Important consumer note**: `utilization` is an optional field on the
wire — present only when Claude has signal to share (warning band,
overload). The parser uses `*float64` to distinguish absent (drop the
snapshot, preserve last-known good in the global store) from explicit
`0.0` (a real "0% used" reading). Don't synthesize 0% when the field is
missing — the empty ring would be visually identical to "no data" and
could clobber a previously-known good reading.

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
