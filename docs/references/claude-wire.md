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
| `user` | `parseUser` | `tool_result` blocks echoed back after tool execution, plus replayed user text (`--replay-user-messages`, `isReplay:true`) — see [§Outbound user message](#outbound-user-message--client-supplied-uuid---replay-user-messages). |
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
**`"interrupted"` is NOT a value** — interruption surfaces through the
`error_during_execution` envelope below.

### Interrupted-turn `result` envelope (verified 2.1.170)

Captured 2026-06-10, 6/6 runs identical (3× interrupt mid-stream, 3×
interrupt before first output; spike per spike-policy):

```json
{
  "type": "result",
  "subtype": "error_during_execution",
  "duration_ms": 1183, "duration_api_ms": 0,
  "is_error": true,
  "num_turns": 2,
  "stop_reason": null,
  "total_cost_usd": 0,
  "usage": {"input_tokens": 0, "output_tokens": 0, "...": "..."},
  "modelUsage": {},
  "permission_denials": [],
  "terminal_reason": "aborted_streaming",
  "fast_mode_state": "off",
  "uuid": "9bab7771-...",
  "errors": ["[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null"]
}
```

Three observations that drive parser behavior:

- `errors[]` no longer contains "aborted"/"interrupted" — only the
  `[ede_diagnostic] ...` marker. The legacy substring heuristic (forge
  `sdkMessageParsing.ts:112-125`, upstream Python SDK) misreads this
  envelope as a hard error. `is_error` is `true` (older docs claimed
  `false` for interrupts).
- `terminal_reason` is `"aborted_streaming"`, but it is a 12-value
  telemetry enum (see §terminal_reason) we deliberately keep out of the
  normalized payload — not a classification key.
- **The interrupt `control_response` ack is always written before the
  `result` line** (6/6, both timings). AO therefore classifies by ack
  correlation: the read loop flags the parser on a successful interrupt
  ack, and the next `error_during_execution` result is the interrupt's
  termination (`session.go handleControlResponseLine` →
  `Parser.MarkInterruptAcked` → `parse_result.go`). The substring
  heuristic stays as fallback for interrupts AO didn't originate.

### Fields the SDK exposes that we should capture
- `total_cost_usd` (NOT `cost_usd`) — ⚠ SESSION-CUMULATIVE across turns
  within one CLI process, not per-turn (verified 2026-07-03 across a
  3-turn session: 0.0216 → 0.0253 → 0.0282; fixture
  `multiturn_cost_cumulative_20260703.ndjson`). Per-turn cost is the
  delta between consecutive envelopes.
- `usage` — per-turn token accounting, but PARENT-ONLY: Task-subagent
  (sidechain) tokens are excluded (fixture
  `subagent_usage_inclusion_20260703.ndjson`). Do not use its
  aggregate token counts as current context-window occupancy.
- `modelUsage` — per-model tokens + CLI-computed `costUSD`,
  subagent-INCLUSIVE, but session-cumulative like `total_cost_usd`.
  This is the preferred accounting source once snapshot-deltaed
  (`internal/provider/claude/usage_accounting.go`).
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
the **top-level fields** on `message_delta.usage` (with no
`parent_tool_use_id`):

```text
input_tokens + cache_creation_input_tokens + cache_read_input_tokens
```

Do not include `output_tokens` for current context occupancy.

#### Top-level is cumulative, and that is what auto-compact tracks

`message_delta.usage` top-level is the cumulative parent-only sum
across every `type:"message"` iteration in the **same SSE message**.
For most tool-using turns (Bash, Task, etc.) that means a single
iteration and the top-level equals it. For an advisor turn — where
`server_tool_use(name="advisor")` runs a separate model call without
terminating the parent's SSE message — the SSE message contains N
parent API calls (`type:"message"`) interleaved with M advisor calls
(`type:"advisor_message"`), and the top-level is the SUM across all N
parent iterations.

That cumulative sum IS what the CLI's own auto-compact trigger uses.
Verified across five production compactions on Claude 2.1.139 (two
threads, four no-advisor turns, three advisor turns): the trailing
`message_delta.usage` top-level matched `compactMetadata.preTokens`
within 1-2% in every case. The last-iteration snapshot was ~2× off
on advisor turns — i.e., reading `iterations[-1]` lets compaction
trigger before the displayed meter crosses any user-visible threshold.

Wire-verified anchor data:

| Turn shape | iterations | top-level | last `type:"message"` iter | `compactMetadata.preTokens` |
| --- | --- | --- | --- | --- |
| no advisor (ef8fb8ee L573 → L578) | `[msg]` | 292,087 | 292,087 | 295,710 |
| single advisor (ef8fb8ee L1241 → L1247) | `[msg, adv, msg]` | 584,017 | 292,614 | 587,018 |
| no advisor (ef8fb8ee L1804 → L1812) | `[msg]` | 288,714 | 288,714 | 310,316 (+22k from next user prompt + nested CLAUDE.md injections) |
| single advisor (ef8fb8ee L2043 → L2050) | `[msg, adv, msg]` | 347,336 | 174,095 | 348,880 |
| single advisor (b951a768 L125 → L131) | `[msg, adv, msg]` | 289,917 | 143,750 | 294,675 |

Implementation: `internal/provider/claude/parse_stream.go` reads the
top-level fields directly into `assistantUsage` and emits one
`EventTokenUsage` per `message_delta`. No iteration extraction is
needed; the iterations array is informational only and useful for
diagnostics (e.g., separating advisor cost from parent cost).

History: an earlier iteration of the parser extracted
`iterations[-1].(type=message)` based on the May 22 spike under the
mistaken assumption that the 2× ratio on advisor turns was an
overcount. That fix (`1c1f9467`) was reverted after the production
`compactMetadata.preTokens` correlation made it clear the top-level
sum is the value Claude itself counts. The May 22 spike was on a
trivial prompt and didn't include a `compactMetadata` anchor; reading
the table above as an overcount instead of a real cumulative was the
specific mistake.

#### Other rules

Do not update the parent chat meter from Agent/Task side signals:
`system.task_notification.usage`, `user.tool_use_result.usage`, or any
assistant/stream event carrying `parent_tool_use_id`. Those belong to
the subagent's private context/cost accounting.

`get_context_usage` is the canonical `/context` breakdown and returns
`totalTokens`, `maxTokens`, `rawMaxTokens`, categories, and `apiUsage`.
Use it when exact category parity is needed (`totalTokens` matches
top-level on non-advisor turns; advisor parity is still pending an
explicit capture); otherwise the passive `message_delta.usage`
top-level is enough for the live meter.

Captured references:
`fixtures/claude/context_usage_spike_20260429.summary.json`
(Bash + Agent subagent, single iteration on message_delta),
`fixtures/claude/advisor_context_usage_20260522.summary.json`
(control / single advisor / double advisor — wire shape only, no
ground-truth anchor), and
`fixtures/claude/advisor_pretokens_correlation_20260523.summary.json`
(authoritative — five production compactions correlating top-level
against `compactMetadata.preTokens`).

Other captured usage-adjacent signals worth preserving for future UI:

| Signal | Future use | Context-meter rule |
| --- | --- | --- |
| `assistant.message.usage` | Fallback if partial `message_delta` events are unavailable; useful for showing per-response usage once an assistant envelope arrives. | Top-level fields. Carries no `iterations[]` — it's a single-call snapshot scoped to the assistant frame. Prefer `message_delta` because assistant envelopes can be earlier snapshots. |
| `stream_event.event.type == "message_start"` `message.usage` | Early API-response usage snapshot, useful for diagnostics or "request started" telemetry. | Do not treat as settled context usage. |
| `stream_event.event.type == "message_delta"` `usage` | Best passive live/settled context signal. | Read top-level (`input_tokens + cache_creation_input_tokens + cache_read_input_tokens`), excluding `output_tokens`. The cumulative sum across iterations is what auto-compact uses. |
| `result.usage.iterations[]` | Per-call breakdown for the closing envelope. Useful for replay diagnostics or splitting advisor cost from parent cost. | Do not drive the live meter from `result`; the trailing message_delta already pushed the right value. |
| `result.usage` (flat) | Per-turn, PARENT-ONLY API-call accounting — it excludes Task-subagent (sidechain) tokens (verified: `subagent_usage_inclusion_20260703.ndjson`, flat in=42/cc=22168 vs modelUsage in=52/cc=35397). Accounting fallback only, when `modelUsage` is absent (claudetui synthesized results). | Same shape as message_delta top-level — both correlate with `compactMetadata.preTokens`. We drive the meter from the live stream, not the closing envelope. |
| `result.modelUsage` | THE turn-accounting source: per-model tokens + CLI-computed `costUSD`, subagent-inclusive. ⚠ SESSION-CUMULATIVE across turns within one process — like `total_cost_usd` (verified: `multiturn_cost_cumulative_20260703.ndjson`, in=10→20→30, cost monotonic). Per-turn truth is the delta between consecutive snapshots; `parse_result.go`/`usage_accounting.go` own that subtraction. `contextWindow` is a useful max-window hint. | Token totals are spend/accounting; meter is driven from the live stream. |
| `result.modelUsage[advisor_model]` | Advisor's own per-call usage (separate model run, separate context window). | Subagent-style private accounting; never updates the parent meter. |
| `system.task_notification.usage` | Subagent/background-task progress or row-level token display. | Subagent-private accounting; do not update parent meter. |
| `user.tool_use_result.usage` and `tool_use_result.totalTokens` | Completed Agent/Task details and subagent cost display. | Subagent-private accounting; do not update parent meter. |
| `control_response` for `get_context_usage` | Canonical `/context` parity: exact `totalTokens`, `maxTokens`, category breakdown, and `apiUsage`; useful on resume/start or for audits. | Use `totalTokens` directly when actively requested. |
| `system.compact_boundary` `compactMetadata.preTokens` | The CLI's own measurement of context at auto-compact time. | Read-only ground truth for validating the meter; correlates with message_delta top-level within 1-2%. Do not drive the meter from this — it only fires at compaction. |
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

## `system/model_refusal_fallback`

**Fires**: when Fable's safety classifier refuses a request and Claude Code
retries it on Opus. The envelope is non-fatal: the turn continues on the
fallback model.

```json
{"type":"system","subtype":"model_refusal_fallback",
 "content":"Fable 5's safeguards flagged this message. ... Switched to Opus 4.8.",
 "trigger":"refusal","originalModel":"claude-fable-5",
 "fallbackModel":"claude-opus-4-8","apiRefusalCategory":"cyber",
 "apiRefusalExplanation":null,
 "refusedUserMessageUuid":"...","requestId":"req_..."}
```

The CLI also records an assistant snapshot for the same request whose sole
content block is `{"type":"fallback","from":...,"to":...}`. Treat the
system envelope as authoritative because it carries the user-facing reason,
classifier category, and refused-message identity. Emit `EventModelFallback`,
persist one warning notification, and project `fallbackModel` as live
session state. Do not overwrite `threads.model`: that is the user's requested
model and a later session may try it again. `GetThreadLiveState` hydrates the
effective model after a frontend refresh; session cleanup clears it.

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

### Injected non-user content on the `isReplay` envelope (`<agent-message>`, …)

`task-notification` is not the only thing the CLI injects into user-role
content. Claude 2.1.x delivers a completed subagent's **final report**
into the PARENT conversation as a `queued_command` attachment, echoed on
the `user{isReplay:true}` envelope wrapped in
`<agent-message from="…">…</agent-message>` (confirmed in the 2.1.202
binary — the tag is `Uyr="agent-message"`, wrapped by the regexes
`^<agent-message[^>]*>\n` / `\n<\/agent-message>$`; it is NOT in the
local source copy). The 2.1.x binary defines several sibling injection
tags in the same table — `teammate-message`, `<channel source="…">`,
`cross-session-message`, `fork-boilerplate` — whose exact envelope shapes
are not yet spiked.

These are NOT user-authored. The canonical suppression set is
`sessionfork.InjectedUserContentWrappers` (one list, shared by the
live-wire parser `isClaudeInjectedReplayContent` and the fork-point
detector `hasInjectedUserContentTag`, so it can never drift). `agent-message`
is catalogued there and suppressed like `task-notification`/`system-reminder`.

Defense in depth: even an *uncatalogued* future wrapper can no longer
surface as a user message. Triage's `handleUserText` treats any top-level
`isReplay` echo that matches no pending send as provider-injected context
and persists it as a non-user `notification` row (`injected:wire:<uuid>`),
never a `user_text` bubble. Cataloguing a wrapper suppresses it entirely;
not cataloguing it makes it a visible-but-clearly-non-user row. Before
this, an uncatalogued `<agent-message>` shipped three subagent reports as
top-level `user:wire` user bubbles (incident 2026-07).

---

## Outbound `user` message + client-supplied `uuid` (`--replay-user-messages`)

AO mints a uuidv4 at send time and sets it as the **top-level `uuid`** on
the outbound user envelope written to the CLI's stdin:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]},"uuid":"<client-minted-uuidv4>"}
```

The CLI honors that id. This is the contract the revert / fork slice
relies on: AO can address a transcript entry by a uuid it knew *at send
time*, before any wire echo, so a fast send→escape revert takes the
UUID-keyed slice (`sessionfork.WriteForkFileForUserMessageUUID`) rather
than the synthetic-entry-sensitive ordinal walk. The app stamps the same
id onto the `user_text` row meta (`provider_item_id`) and the message
checkpoint (`ProviderUserMessageID`) before the optimistic persist —
see `internal/provider/claude/session.go` (`Send`) and `app_send.go`.

### ⚠ Verified behavior (claude 2.1.150, AO's exact flags) — undocumented contract

Confirmed by isolated spike on 2026-05-27 against the installed binary
with the full base flag set (`--input-format stream-json --output-format
stream-json --verbose --permission-prompt-tool stdio
--include-partial-messages --replay-user-messages --thinking-display
summarized`), 3 turns in one persistent session:

- **Persisted verbatim.** The session JSONL
  (`~/.claude/projects/<proj>/<session_id>.jsonl`) gains a `type:"user"`
  entry whose `uuid` is *exactly* the value we sent — no normalization,
  no reassignment.
- **Echoed verbatim.** The CLI echoes the message back on stdout as a
  `user` envelope with `isReplay:true` carrying the same top-level
  `uuid` (this is what `--replay-user-messages` adds; triage promotes it
  to `EventUserText` and folds in `parentUuid` — see
  `internal/triage/handle_user_text.go`).
- **`parentUuid` is CLI-assigned.** The client supplies only `uuid`; the
  CLI assigns `parentUuid` itself, threading the entry onto the transcript.

### Queued-message consumption — two flavors (claude 2.1.202 / 2.1.205)

A user envelope written to stdin **while a turn is running** honors the
client-supplied uuid in both cases, but the CLI consumes it at one of
two points with **different transcript shapes**:

**At turn pickup** (spike 2026-07-09, 2.1.202: msg2 queued ~5 s into a
~20 s turn): the CLI holds the message until the running turn finishes,
then echoes it as `user{isReplay:true}` carrying the supplied uuid
**verbatim** and persists a real `type:"user"` JSONL entry under that
uuid — exactly like a direct send, just delayed.

**Mid-loop** (production transcripts, 2.1.205): the CLI's queue
processor drains the message into the RUNNING turn at the next API
iteration (query.ts:1547 in the local source copy). No `system/init`,
no new wire turn — the response continues on the same wire round, and
the transcript entry is NOT a user row:

```json
{"type":"attachment","uuid":"<CLI-minted>","parentUuid":"<prior entry>",
 "isSidechain":false,
 "attachment":{"type":"queued_command",
               "prompt":[{"type":"text","text":"<queued message>"}],
               "source_uuid":"<client-supplied uuid>",
               "commandMode":"prompt","timestamp":"…"}}
```

The client uuid survives only as `attachment.source_uuid`; the next
assistant entry parents to the attachment row's own uuid, so the row is
on the active branch. The stdout echo still carries the client uuid, so
AO's identity matching works the same in both flavors.

Consequences:

- AO's flush-queue dispatch (`app_flush_queue.go`) mints a uuid per
  queued item exactly like a direct send, and triage's pending-send
  matching (`consumeMatchingPendingSend`) keys on it. No order-based
  fallback is needed for Claude.
- The echo can arrive an arbitrarily long time after the stdin write
  (the whole remaining turn), so any Claude-injected `user{isReplay}`
  envelope landing in that window (e.g. an `<agent-message>` subagent
  report) interleaves with pending queued sends. Identity matching —
  not FIFO position — is what keeps those from mispairing.
- The revert / fork slice must anchor on EITHER shape:
  `sessionfork.parentUUIDForUserMessageUUIDInTranscript` prefers the
  real user entry and falls back to the `queued_command` attachment's
  `source_uuid`. A user-uuid-only matcher silently misses every
  mid-loop-consumed queued message (`ErrMessageNotFound` → ordinal
  fallback → wrong slice for a mid-turn anchor).
- Mid-turn slices resume cleanly: spike 2026-07-15 (2.1.205) resumed a
  session JSONL truncated immediately after a `tool_result` entry with
  full prior context retained — the contract behind reverting to a
  queued message that shares its turn with a running prompt.

### Timing — the fast send→escape race window

Same spike, each value measured from the stdin write of the envelope:

| Turn | user entry on disk | first assistant token | `result` |
|---|---|---|---|
| 0 (cold — also creates the session file) | ~1856 ms | ~3415 ms | ~3702 ms |
| 1 (warm) | ~98 ms | ~1449 ms | ~1537 ms |
| 2 (warm) | ~102 ms | ~1468 ms | ~2711 ms |

The user entry lands on disk **before the first assistant token in every
case** — ~100 ms in steady state, up to ~1.9 s on the cold first turn
that also creates the session file. So the window in which AO has stamped
the uuid on its own rows but the CLI has not yet written it to the JSONL
is ~100 ms and closes *before the user sees the turn begin responding*.
A revert firing inside that sliver fails safe: the UUID-keyed slice
returns `ErrMessageNotFound` and falls back to the
(synthetic-entry-corrected) ordinal walk, then to a full-transcript clone
+ composer-draft restore. See `app_checkpoint.go` (`writeClaudeSessionSlice`).

### Drift

This is an **undocumented binary contract** pinned to the observed CLI
version. If a future CLI stops honoring the supplied `uuid` (or starts
canonicalizing / reassigning it), revert-by-uuid silently degrades to the
ordinal walk, and pending-send identity matching degrades loudly: the
echo matches no expected id, triage logs the mismatch and persists it as
an injected-context notification instead of confirming the send (the
queued-message overlay would then stay visible — a loud failure, not a
mispair). Re-spike per [`spike-policy.md`](spike-policy.md) before
assuming it still holds; `claude/session.go` rejects non-canonical input
up front so the row, checkpoint, envelope, and echoed JSONL `uuid` stay
byte-identical.

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

**Fires ~instantly** once a Bash command starts running in the
background.

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

**Marker: `tool_use_result.backgroundTaskId` present.** This is the
authoritative signal and is set for ALL THREE ways a command ends up
backgrounded — do NOT key off `input.run_in_background` alone:

1. **Explicit** — the tool_use input carried `run_in_background: true`.
   (The example above.)
2. **Timeout auto-background** — the CLI moves a *foreground* command to
   the background once it exceeds its Bash timeout. The tool_use input
   carries NO `run_in_background` flag (observed input keys: just
   `{command, description}`), so the launch-time hint is absent and
   `backgroundTaskId` is the only signal. The sibling additionally
   carries `"assistantAutoBackgrounded": false`. Captured 2026-06-20 from
   a real session (thread `d920dc89`, command `make check`).
3. **Assistant-initiated** — the model backgrounds a running command
   mid-execution. The sibling's `assistantAutoBackgrounded` boolean is the
   field that distinguishes this trigger (captured = `false` in the timeout
   case above; the `true` variant is inferred from the field name, not yet
   captured). Either way `backgroundTaskId` is still present, so the parser
   needs no per-trigger branch.

`backgroundTaskId` equals the `task_id` carried by the
`system/task_started` + `system/task_updated` lifecycle, so the terminal
that writes the sibling `tool_completion` row is unaffected by which
trigger fired.

**Parser behavior**: DOES emit `EventToolComplete` for the tool's
own id (universal invariant), with `is_background: true` whenever the
marker is present (see `toolResultBackgrounded` in `parse_user.go`).
Per agent-overflow spec, triage keeps the `tool_call` row at
`status='running'` for backgrounded tools — the sibling
`tool_completion` row comes later via the task lifecycle. See
[`turn-lifecycle.md`](../architecture/turn-lifecycle.md).

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

### E5 — Async `local_agent` launch (bare ack)

A THIRD launch shape for `local_agent` (Task/Agent subagent), distinct
from both E2 (backgrounded Bash placeholder) and an ordinary inline
completion. Fires when the `Agent`/`Task` tool_use's `input` carries NO
`run_in_background` flag AND the CLI still chooses to run the subagent
asynchronously — the tool_result arrives almost immediately (~ms) as a
bare acknowledgment rather than the subagent's actual result:

```json
{"type": "user",
 "message": {"role": "user", "content": [{
   "tool_use_id": "toolu_01DND5fS6nX3LnefKJuJeBzQ",
   "type": "tool_result",
   "content": [{"type": "text", "text": "Async agent launched successfully.\nagentId: a32408c956466d32c ..."}]
 }]},
 "tool_use_result": {
   "isAsync": true,
   "status": "async_launched",
   "agentId": "a32408c956466d32c",
   "description": "Review 4b: perf/memory lens",
   "resolvedModel": "claude-fable-5",
   "prompt": "...",
   "outputFile": "/tmp/.../tasks/a32408c956466d32c.output",
   "canReadOutputFile": true
 }}
```

**Marker: `isAsync: true` and/or `status: "async_launched"`.** No
`backgroundTaskId` anywhere on this shape — `agentId` is the id the
later task lifecycle addresses as `task_id`.

### ⚠ Discriminator subtlety — inline completions ALSO carry `agentId`

An INLINE (awaited) `local_agent` result — the normal case where the
subagent's real output arrives as the `tool_result` — carries the SAME
`agentId` field, plus `status: "completed"` and richer totals
(`totalDurationMs`, `totalTokens`, `totalToolUseCount`, `toolStats`,
`usage`):

```json
{"agentId": "a0e27f56d74e34245", "agentType": "general-purpose",
 "content": [{"type": "text", "text": "…result text…"}],
 "resolvedModel": "claude-fable-5", "status": "completed",
 "totalDurationMs": 431917, "totalTokens": 129893,
 "totalToolUseCount": 29, "usage": {"...": "..."}}
```

`agentId` (or the mere presence of a `status` field) is **NOT** a valid
async discriminator by itself — every inline agent's terminal result
also carries both. Key exclusively on `isAsync: true` /
`status: "async_launched"`; see `toolResultAsyncLaunch` in
`parse_user.go`.

### Terminal delivery — same task lifecycle as E2/E3

The async ack's `agentId` equals the `task_id` the later
`system/task_updated` + `system/task_notification` pair addresses —
identical correlation to E2's `backgroundTaskId`. `system/task_started`
normally seeds the `task_id ↔ tool_use_id` map ~4ms before the ack
arrives, but the ack's own `tool_use_id` + `agentId` are enough for a
parser that reconnected and missed `task_started` to re-seed the same
mapping (see `rememberTaskToolUse` call inside `appendToolResultBlock`).

### ⚠ Inline agents emit the full task lifecycle too

`system/task_started` fires for EVERY `local_agent` launch, inline or
async (see §`system/task_started` above — it is not a
backgrounded-only signal). Consequently an INLINE agent also gets a
`system/task_updated` terminal and a `system/task_notification` for the
SAME launch that already completed via its own real `tool_result`.
Triage must not treat that lifecycle signal as authorization to write a
second completion row for an already-completed inline launch — see
`internal/triage/tool_lifecycle.go`'s `writeBackgroundCompletionSibling`
foreground gate (`!launch.IsBackground`) and
[`turn-lifecycle.md §Task lifecycle`](../architecture/turn-lifecycle.md#2-task-lifecycle-claude-only).

### Parser behavior

1. **Always** emit `EventToolComplete` for the launch's own
   `tool_use_id` (universal invariant), with `is_background: true`
   whenever `isAsync`/`status: "async_launched"` is present — same
   `is_background` meta shape E2 uses, so triage's existing
   "keep `status=running`, wait for the sibling" handling applies
   unchanged.
2. Re-seed `task_id ↔ tool_use_id` correlation from the ack (see
   above) so the terminal resolves even across a reconnect that missed
   `task_started`.

See
[`local_agent_async_launch.ndjson`](fixtures/claude/local_agent_async_launch.ndjson)
for the full captured 7-line sequence (content_block_start,
assistant tool_use, task_started, the ack, one task_progress sample,
task_updated terminal, task_notification).

### E6 — Resuming an idle async agent (`task_started` rebind)

An E5 async agent goes idle once it finishes and can be RESUMED — the
model calls the harness's resume tool (observed: `SendMessage`,
`input.to: <agentId>`) to send it a follow-up message. Verified from a
live capture (AO thread `9941d40f`, 2026-07-02; fixture
[`local_agent_async_resume.ndjson`](fixtures/claude/local_agent_async_resume.ndjson)).

On resume the CLI emits a FRESH `system/task_started` with the SAME
`task_id` (the agentId) but `tool_use_id` = the resuming tool's OWN
call, carrying the ORIGINAL agent's `description` + `subagent_type` +
`task_type:"local_agent"` — not the resuming tool's own description:

```json
{"type": "system", "subtype": "task_started",
 "task_id": "a464e54e96a45cd0c",
 "tool_use_id": "toolu_01HNzp6MQbMMcTmoY7Yy1wdw",
 "description": "Frontend transitive suppression fix",
 "subagent_type": "general-purpose", "task_type": "local_agent",
 "prompt": "Apply the reviewer-synthesized single-forward-pass rework..."}
```

The resuming tool's own `tool_result` arrives right after with no
async markers at all — no `isAsync`, no `status`, no
`backgroundTaskId`:

```json
{"tool_use_result": {
   "success": true,
   "message": "Agent \"a464e54e96a45cd0c\" had no active task; resumed from transcript in the background with your message. You'll be notified when it finishes. Output: /tmp/.../a464e54e96a45cd0c.output",
   "resumedAgentId": "a464e54e96a45cd0c"
 }}
```

**Marker: `task_started` with `task_type:"local_agent"` binding a
task_id to a DIFFERENT `tool_use_id` than the one already on file for
it.** This is the resume signal — there is no marker on the ack itself
(`resumedAgentId` is present but arrives too late to gate on; the
rebind on `task_started` is the earlier, authoritative signal).

The resumed round's child envelopes (the agent's own tool calls)
observed on the wire stay parented to the ORIGINAL Agent launch's
`tool_use_id` via `parent_tool_use_id` — DB-verified (271 + 142
children across the two rounds, zero parented under the resuming
tool). Only the task LIFECYCLE (`task_started`/`task_updated`/
`task_notification`) rebinds; the agent's actual conversation tree
does not move.

Round-2 `task_updated` carries no `tool_use_id` on the wire at all
(matching `task_updated`'s general behavior — see
[§task_updated](#systemtask_updated)); round-2 `task_notification`
DOES carry the resuming tool's `tool_use_id` inline, matching whichever
tool_use is currently bound.

#### AO's carrier normalization

AO embraces the rebind rather than fighting it: the resuming tool's
own `tool_use_id` becomes the resumed round's **background carrier**.
`rememberTaskToolUse` lets the map move to the new id (no
first-binding-wins) — that IS what correctly routes round-2's
`task_updated`/`task_notification` through the map-first resolution
both handlers already use. The parser additionally:

1. Marks the resuming tool_use backgrounded via the same mechanism
   `run_in_background` launches use, so its `EventToolComplete` (the
   ack above) carries `is_background:true` even though the ack itself
   has no async marker.
2. Enriches the meta-only `EventToolStart` the rebind `task_started`
   emits with `resumes_tool_use_id` (the previously-bound tool_use —
   the original launch) and the wire's `description`. The envelope
   also carries `subagent_type`, but nothing downstream consumes it,
   so the parser deliberately does not stamp it.

Triage's keep-running flip then marks the carrier row backgrounded +
running, and — because its meta carries `resumes_tool_use_id` —
rewrites its Summary to the original launch's own Summary (or
`"Agent: " + description` as a fallback), so the carrier reads
"Agent: Frontend transitive suppression fix" instead of "SendMessage:
…". Round 2's `task_updated`/`task_notification` then write a NEW
`tool_completion` sibling under the carrier (`complete:<carrierID>`,
distinct from round 1's `complete:<originalLaunchID>`), which
`buildBackgroundTerminalSummary` renders as "Agent: Frontend
transitive suppression fix -> done" — indistinguishable from any other
backgrounded agent completion. See
[`turn-lifecycle.md §Task lifecycle`](../architecture/turn-lifecycle.md#2-task-lifecycle-claude-only)
and `internal/triage/tool_lifecycle.go`'s `resumeCarrierSummary`.

Why this matters operationally: AO's idle-session reaper closes a
quiet session unless `ListRunningBackgroundToolCalls` is non-empty.
Without the carrier, the ORIGINAL launch already has its round-1
sibling (`NOT EXISTS` completion predicate fails), so nothing keeps
the predicate satisfied during round 2 — a quiet resumed agent would
get the whole session reaped mid-run. The carrier (backgrounded,
running, no sibling until round-2 terminal) is what keeps the
predicate true.

**Reconnect edge**: if the parser restarts between the original
launch and its resume (a fresh process `--resume`d the session and
the model re-resumed the agent from its transcript), `taskToolUses`
has no binding for the task_id at all. The parser falls back to a
name-agnostic rule: a `local_agent` `task_started` binding to a
tool_use that was never observed as the launch tool (`Agent`, or
`Task` on older builds — see `isAgentLaunchToolName` in
`parse_assistant.go`) is still classified as a resume, so the carrier
marking survives the restart even though `resumes_tool_use_id` is
unknown (omitted) and the Summary rewrite has no anchor row to look
up. A fresh launch cannot false-positive into this rule: parser
lifetime == CLI process lifetime (stdio), and the assistant envelope
carrying the launch tool_use always precedes its `task_started` on
the same sequentially-parsed stream, so the launch-tool marker is
already in place when the rule runs — and `local_agent` tasks die
with their CLI process, so no pre-restart in-flight agent can re-emit
`task_started` on the new process. The one unprotected window is
losing the carrier's background flag between the rebind
`task_started` and the resume ack — only possible via the parser's
bounded-map wholesale reset (`parserTaskMapCap`, 1024 live entries
accumulating inside a ~ms window; practically unreachable). A lost
flag degrades to pre-fix behavior: the carrier lands foreground and
that resumed round is reaper-unprotected.

See
[`local_agent_async_resume.ndjson`](fixtures/claude/local_agent_async_resume.ndjson)
for the full captured 10-line two-round sequence (assistant tool_use /
task_started / ack / task_updated / task_notification, twice — the
second round's `task_started` and `tool_use_id`s are the resuming
tool's own).

### E7 — Monitor watch-task launch ack

The harness's `Monitor` tool runs a Bash command as a **background
`local_bash` task** that notifies the model on each output event
(`persistent: true` runs until `TaskStop` or session end;
`persistent: false` runs until its first matching event or `timeoutMs`).
Like E5, the launch input carries NO `run_in_background` flag and the
ack carries NO `backgroundTaskId` — a fourth background-launch shape.
Captured live 2026-07-28 (AO thread `b44a738d`, session `d946175f`;
shape summary:
[`monitor_wakeup_20260728.summary.json`](fixtures/claude/monitor_wakeup_20260728.summary.json)):

```json
{"type": "user",
 "message": {"role": "user", "content": [{
   "tool_use_id": "toolu_015DPAp7hi8LaywoMtec4c3Y",
   "type": "tool_result",
   "content": "Monitor started (task bpzc8uiti, persistent — runs until TaskStop or session end). You will be notified on each event. ..."
 }]},
 "tool_use_result": {"taskId": "bpzc8uiti", "timeoutMs": 0, "persistent": true}}
```

Non-persistent variant (same keys, different values):
`{"persistent": false, "taskId": "bmh73qh8o", "timeoutMs": 600000}`.
Input-validation failures ack with a STRING `tool_use_result`
(`"InputValidationError: …"`), which decodes to zero signals.

**Marker: `taskId` + presence of `persistent` / `timeoutMs`.**
`taskId` ALONE is **NOT** a valid discriminator — the task-list tools'
acks (`TaskCreate`/`TaskUpdate`: `{success, taskId, updatedFields,
statusChange?}`) carry a top-level `taskId` too and describe a
bookkeeping row, not a process. `TaskStop` acks (`{message, command}`)
and `TaskOutput` acks (task nested under `task`) carry no top-level
`taskId` at all. Surveyed across every `toolUseResult` shape in the
capture session — the Monitor ack is the only shape pairing `taskId`
with `persistent`/`timeoutMs`; `toolResultMonitorLaunch` in
`parse_user.go` accepts either sibling key so a future CLI dropping one
still classifies.

Lifecycle correlation is standard: `system/task_started` fires with
`task_type: "local_bash"` binding `taskId ↔ tool_use_id` (~ms before
the ack), each Monitor event arrives via `system/task_notification` (or
the queued `<task-notification>` user-turn injection), and terminal
delivery is the same `system/task_updated` channel E2 uses (`TaskStop`
→ `status:"killed"`). Parser behavior mirrors E5: the completion emits
`is_background: true` so triage's keep-running flip holds the launch
row at `status=running` (reaper protection), and the ack re-seeds
`rememberTaskToolUse` for reconnected parsers.

The completion additionally carries `watch_task: true`, which the
keep-running flip copies onto the launch row's meta. A Monitor
OBSERVES — it never produces the result a queued user send could be
waiting on, and a persistent one runs until session end — so the
flush-queue drain uses the store's watch-excluding predicate
(`HasQueueBlockingBackgroundToolCall`) and dispatches queued sends
past a running watch, while the reaper / revert / context-repair
consumers still count the watch as live background work (closing or
restarting the session WOULD kill it).

Missing this signal was found the hard way: a session whose only live
work was a persistent Monitor read as fully idle to the reaper, and
closing it would have killed the watched multi-hour job (2026-07-28,
thread `b44a738d`).

### E8 — ScheduleWakeup ack (pending in-process wakeup, NO task lifecycle)

The harness's `ScheduleWakeup` tool arms an **in-process timer** (delay
clamped to [60s, 3600s]); when it fires, the CLI injects the stored
prompt as a fresh user turn. There is NO task behind it — no
`task_started`, no `task_updated`, no wire traffic of any kind between
the ack and the fire — so a session waiting on a wakeup is
indistinguishable from an abandoned one by every other signal. Captured
live 2026-07-24 (session `d946175f`; same summary fixture as E7):

```json
{"tool_use_result": {"clampedDelaySeconds": 1500, "scheduledFor": 1784917860000, "wasClamped": false}}
```

`scheduledFor` is the absolute fire time in epoch **milliseconds**. The
`{stop: true}` input ends the loop and acks with:

```json
{"tool_use_result": {"scheduledFor": 0, "clampedDelaySeconds": 0, "wasClamped": false, "stopped": true}}
```

**Marker: `scheduledFor` corroborated by `clampedDelaySeconds` /
`stopped`** (`toolResultScheduledWakeup` in `parse_user.go` requires a
sibling key so an unrelated future shape reusing `scheduledFor` alone
does not classify). The parser emits `EventSessionWakeup`
(`SessionWakeupMeta.ScheduledForUnixMs`; `<= 0` clears) alongside the
normal `EventToolComplete` — the ack is NOT a background launch and
carries no `is_background`.

Operationally: triage records the fire time per thread
(`internal/triage/session_wakeup.go`) and the idle-session reaper
refuses to close a session whose wakeup is still in the future (plus a
firing-latency grace) — closing the process would silently kill the
timer. The record is swept on session teardown AND on
replacement-session commit: the timer never survives its CLI process.
When the wakeup fires, the injected turn's own wire activity takes over
protection; observed delays run 240–1800s, so a wakeup can legally
exceed the idle-reap threshold on its own.

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

⚠ Retries that recover leave NO wire trace beyond these envelopes —
but they DO leave deferred `system/api_error` rows in the session
JSONL, written **at the next user send** with a **stale parentUuid**.
That file-side artifact (not a wire shape) breaks resume topology; see
[§Session JSONL: deferred `system/api_error` rows](#session-jsonl-deferred-systemapi_error-rows-stale-parents).

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
  mode (Plan ↔ chat ↔ accept-edits ↔ bypass). Takes `mode`. Escalating to
  `bypassPermissions` is REJECTED unless the process was launched with
  `--allow-dangerously-skip-permissions` / `--dangerously-skip-permissions`
  (error: "Cannot set permission mode to bypassPermissions because the
  session was not launched with --dangerously-skip-permissions"; verified
  2.1.205) — AO restarts the session for that transition instead.
- `subtype: "set_model"` — switch the live session's active model. Takes
  `model` (the same string `--model` accepts). Verified on 2.1.205: the
  CLI acks immediately even mid-turn, the in-flight turn finishes on the
  previous model, and the next turn (plus the fresh `system/init` it
  emits) runs on the new one. Used by AO's config reconciler
  (`app_session_config.go`) so a model change never kills a working
  session. The CLI also echoes a replayed user envelope containing
  `<local-command-stdout>Set model to ...</local-command-stdout>`.
- `subtype: "set_max_thinking_tokens"` — set the live session's max
  thinking-token budget. Takes `max_thinking_tokens` (int). Verified
  accepted on 2.1.205; NOT currently used by AO (our effort tiers map to
  the spawn-time `--effort` flag, which has no live equivalent — there is
  no `set_effort`, `set_fast_mode`, or `set_context_window` subtype as of
  2.1.205, so those changes restart the session).
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
`app_mcp_auth.go:pollClaudeMCPAfterOAuth`. Claude emits no
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

## Permission modes for read-only sessions

Spike-verified on **claude 2.1.219** (2026-07-25). Consumed by
`internal/provider/claude/options.go`
(`claudeBasePermissionMode` / `claudeDisallowedTools`) to implement the
workflow `access: read-only` mapping (spec §9, decision D22).

`--permission-mode` accepts `acceptEdits | auto | bypassPermissions | manual |
dontAsk | plan` on this release. There is no `--sandbox` flag.

### `dontAsk` denies; it does not auto-approve, and it does not prompt

The name is ambiguous — it could mean "don't ask, just do it". It means the
opposite. One turn under
`claude -p --output-format stream-json --input-format stream-json --verbose
--permission-mode dontAsk`, asked to read a file, write a file, run `ls -1`,
and run `touch mutated.txt`:

| Action | Result |
|---|---|
| `Read readme.txt` | succeeded |
| `Write written.txt` | denied — `tool_result` with `is_error: true` |
| `Bash ls -1` | succeeded |
| `Bash touch mutated.txt` | denied — `tool_result` with `is_error: true` |

Denial text (verbatim prefix):

```
Permission to use Write has been denied because Claude Code is running in
don't ask mode.
```

The properties that matter for unattended work, all observed:

- **No `control_request` was emitted at all.** The denial is synthesised
  in-process, so nothing waits on a `CanUseTool` response that no human will
  send.
- **The turn completed normally** — `result{subtype:"success",
  is_error:false}`, process exit 0. The model reads the error `tool_result`
  and keeps going.
- **The working tree was untouched** — neither file was created,
  `git status` clean.
- Bash is judged **per command**, not wholesale: `ls -1` ran, `touch` did not.

Mechanically, `dontAsk` is applied at the very end of the permission pipeline
as an `ask → deny` rewrite (`hasPermissionsToUseTool`, claude-code source
`src/utils/permissions/permissions.ts`), which is exactly why the next section
matters.

### ⚠ `dontAsk` alone is NOT enforcement — an allow rule defeats it

Because the rewrite only converts `ask`, anything a settings source already
resolves to `allow` never becomes an ask and is permitted. Re-running the same
turn with `--settings '{"permissions":{"allow":["Write","Edit","Bash(touch:*)"]}}'`:
**every step succeeded** and the repo was dirtied (`written.txt` and
`mutated.txt` both created). A user's own
`~/.claude/settings.json` `permissions.allow` would do the same thing to a
session AO intended to be read-only.

Adding `--disallowedTools "Write,Edit,NotebookEdit"` to that same pre-allowed
run removed the write tools from the session entirely — the model reported
"the Write tool isn't currently loaded" and could not call it — while `Read`
and `ls -1` still worked. Tool removal is a deny that outranks any allow rule,
so **both flags are required**; neither is redundant.

**Residual, deliberately not closed here:** a pre-existing allow rule for a
*mutating Bash command* (e.g. `Bash(touch:*)`) is still honoured under
`dontAsk`, because Bash stays available and that specific command resolves to
`allow`. Closing it completely would need either `--setting-sources ""` (which
also drops the project's `CLAUDE.md` — `isSettingSourceEnabled('projectSettings')`
gates memory loading) or Claude's `sandbox.filesystem.denyWrite` settings block
(OS-level, platform-dependent, and silently falls back to unsandboxed unless
`sandbox.failIfUnavailable` is set). Both are behaviour changes beyond a
permission mapping. Codex's read-only sandbox has no equivalent gap, so the two
providers' read-only tiers are not exactly equal in strength.

### Flag form: repeated is accepted

`--help` documents `--disallowedTools <tools...>` as "comma or space-separated".
The **repeated-flag** form AO emits (`--disallowedTools Write --disallowedTools
Edit --disallowedTools NotebookEdit`, matching how `--allowedTools` is already
rendered in `buildArgs`) was verified equivalent: with
`permissions.allow: ["Write","Edit"]` also in play, all three tools were still
removed and the working tree stayed clean.

### Spawn-time only

`--disallowedTools` is applied when the process starts and there is no
`control_request` that adds or removes a tool mid-session. A live
runtime-mode change into or out of read-only therefore requires a session
restart, not a `set_permission_mode` — enforced by
`claude.PlanLiveUpdate` comparing `Config.DisallowedTools`.

### `set_permission_mode` accepts `dontAsk`

`dontAsk` must be listed in `normalizeClaudePermissionMode`
(`internal/provider/claude/session.go`); values it does not recognise collapse
to `"default"`, which would silently restore a *prompting* base mode after a
plan turn — a hang rather than a refusal for an unattended run.

---

## Resume does not re-emit assistant content (stdout)

Spike-verified on 2.1.170 (2026-06-24), fixture
[`fixtures/claude/resume_no_assistant_replay_20260624.summary.json`](fixtures/claude/resume_no_assistant_replay_20260624.summary.json).

`claude --resume <id>` loads prior turns into the model's context
**silently** — it does **not** re-emit historical `assistant` content
(text / thinking) on stdout. A resumed process streams only the **new**
turn, with the same envelope shape a fresh turn produces
(`stream_event:message_start` → `content_block_*` deltas → coalesced
`assistant` snapshot → `result`). Two back-to-back turns sharing one
session emit identical top-level envelope counts on resume (one
`assistant`, one `user`); turn 2's stdout contains none of turn 1's
assistant text.

The only replayed envelope is the **user** message the client just
sent, echoed with `isReplay:true` by `--replay-user-messages` (routed
through `parse_user_replay.go`). Assistant envelopes never carry
`isReplay`.

This is the safety anchor for the snapshot-recovery path
(`parse_assistant.go` → the `streamedMessageIDs` discriminator in
`parser.go`): because resume never delivers a historical `assistant`
snapshot, a bare snapshot with no in-process `message_start` is
**always** an in-turn CLI-internal retry (thread fc24607e), never
replayed history. Recovering it cannot duplicate the assistant history
on reopen — the discriminator being process-local (empty on a fresh
resume) is therefore not a hazard.

> Design notes elsewhere say `--resume` "replays the full session log
> including tool_results". This spike did **not** exercise that path —
> it verified only that assistant text/thinking is not re-emitted and
> that the just-sent user message is echoed `isReplay:true`. Whether
> historical user-role `tool_result`s re-fire on resume is a separate,
> unverified-here mechanism (it would route through `parse_user_replay.go`,
> not the assistant path) and is irrelevant either way to the
> snapshot-recovery path this section anchors.

---

## Session JSONL: active-branch semantics (`--resume` / `--resume-session-at`)

Not a wire shape — the on-disk contract for
`~/.claude/projects/<slug>/<sessionID>.jsonl` that resume flags are
validated against. Verified by spike on 2.1.170 (2026-06-10) plus the
2026-06-10 incident empirics (2.1.167/168/170).

**The active branch** is the chain Claude reconstructs by walking
`parentUuid` back from the file's **last uuid-bearing transcript row**.
Resumed context = that chain only; rows off the chain stay in the file
but contribute nothing.

**`--resume-session-at <uuid>` is validated against the active branch
only**, eagerly at startup (pre-init, pre-API — a rejected cursor costs
no tokens). An off-branch uuid hard-fails:
`result{subtype:"error_during_execution", is_error:true, num_turns:0,
errors:["No message found with message.uuid of: <uuid>"]}` — and the
process then **lingers** instead of exiting (AO reaps it; see
`teardownDeadPreInitSession`).

Row types that define the walk (spike-verified per type):

| Row type | Considered by claude's walk? | Evidence |
|---|---|---|
| `user`, `assistant` | yes | incident + spike A1 |
| `attachment` | **yes** | spike A2 — an attachment tail chained mid-branch made the file-order content leaf off-branch (rejected) |
| `system` (incl. `api_error`) | **yes** | incident — the deferred api_error rows WERE the branch tip |
| `custom-title` (uuid-bearing) | no | spike A3 — trailing uuid-bearing title row did not move the tip |
| `mode`, `last-prompt`, `queue-operation` | no (uuid-less) | inventory of production files |
| sidechain rows (`isSidechain:true`) | no | separate graphs |

More spike-verified behavior (A1, B):

- **Interior on-branch cursor**: accepted. The turn runs with context
  ending at the cursor; rows past it remain in the file (abandoned
  branch) and the new user row's `parentUuid` is the cursor — in-file
  branching, no truncation.
- **System rows as explicit cursors**: accepted by the CLI (spike B).
  AO still only ever passes user/assistant rows
  (`ResumeAtOnActiveBranch` rejects system rows by design — resuming at
  an error row would end context on furniture).
- Plain `--resume` with no cursor uses the CLI's own default leaf —
  omitting `--resume-session-at` is always safe, never wrong-branch.

AO enforcement: invariant 28 — `sessionfork` re-chains deferred
api_error tails so fork output keeps its writable tail on-branch;
`ScanSessionLeaf` validates its file-order pick against a branch index
and repairs off-branch picks; `resolveClaudeResumeAt` validates
explicit cursors at spawn. `internal/provider/claude/sessionleaf_branch.go`
mirrors the row table above (`branchTranscriptTypes`).

## Session JSONL: deferred `system/api_error` rows (stale parents)

**Upstream bug** (2.1.167–2.1.170, report draft:
[`claude-api-error-upstream-report.md`](claude-api-error-upstream-report.md)).
When an API request inside a turn fails and is retried (the wire shows
`system/api_retry`, the turn completes normally), the CLI buffers the
error rows and writes them to the session JSONL **at the next user
send** — with `parentUuid` pointing at the **mid-turn leaf from
retry time**, bypassing the rest of the turn in the parent graph:

```json
{"type":"system","subtype":"api_error","level":"error","uuid":"<err1>",
 "parentUuid":"<MID-TURN row, not the turn's final assistant>",
 "retryAttempt":1,"retryInMs":1000,"maxRetries":10,
 "error":{"message":"Connection error.","connection":{"code":"ECONNRESET"}},
 "content":"API error"}
```

Consequences (because system rows define the active branch — see
section above): every cold `--resume` silently drops the prior turn's
tail from context, and `--resume-session-at` any tail row hard-fails.
The next user row chains onto the api_error rows, entrenching the
bypass. Fixture:
[`fixtures/claude/session_api_error_offbranch.jsonl`](fixtures/claude/session_api_error_offbranch.jsonl)
(sanitized incident replica).

AO countermeasures: `sessionfork/rechain.go` forces each deferred
api_error row's fork parent to its file predecessor (subtype-scoped —
compact-boundary system rows are legitimate `parentUuid:null` roots and
are never touched); the branch-aware leaf scan + spawn validation cover
unforked files.

## Session JSONL: compact_boundary ordering

`system/compact_boundary` rows are `parentUuid:null` chain **roots**
carrying `logicalParentUuid` (the pre-compact leaf). In both production
samples (auto-compact, 2.1.x) the boundary row is immediately followed
by the `isCompactSummary:true` user row whose `parentUuid` is the
boundary's uuid — the pair lands together, so the active-branch tip
after a compact is the summary row (or later), never the bare boundary.
A file-trailing boundary has not been observed (an idle-`/compact`
synthesis attempt on a tiny session didn't trigger compaction at all);
if one ever occurs, AO's branch walk finds no content row and resumes
with no cursor — the safe degenerate.

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

### `docs/references/fixtures/claude/local_agent_async_launch.ndjson`
**Scenario**: a `local_agent` (`Agent` tool) launch whose input carries
NO `run_in_background`, launched asynchronously anyway — the bare
"Async agent launched successfully." ack (E5), then the real terminal
via `system/task_updated` + `system/task_notification`.

**Shapes covered**: `stream_event` content_block_start for the launch
tool_use, `assistant` tool_use, `system/task_started`, the async ack
`user` tool_result (E5), `system/task_progress`, `system/task_updated`
terminal, `system/task_notification`. 7 real wire lines; the three long
`prompt` values (assistant `input.prompt`, `task_started.prompt`, ack
`tool_use_result.prompt`) are truncated to a placeholder sentence —
every other key/value is byte-identical to the capture.

### `docs/references/fixtures/claude/local_agent_async_resume.ndjson`
**Scenario**: an E5 async agent resumed via the harness's SendMessage
tool (E6) — the CLI rebinds `system/task_started` onto SendMessage's
own `tool_use_id` carrying the ORIGINAL agent's description, and the
SendMessage `tool_result` ack has no async markers at all.

**Shapes covered**: two full rounds back to back — round 1 is an
ordinary E5 async launch + terminal + notification (assistant tool_use
/ `task_started` / ack / `task_updated` / `task_notification`); round 2
is the SendMessage resume with the same 5-envelope shape, `task_id`
unchanged, `tool_use_id` rebound to the SendMessage call. 10 real wire
lines; only the long free-text values (assistant `input.prompt`,
`task_started.prompt`, SendMessage `input.message`/`input.content`)
are truncated to placeholders — every other key/value, including the
`description`/`subagent_type` echoed on the round-2 `task_started` and
the ack's `resumedAgentId`, is byte-identical to the capture (AO
thread `9941d40f`, 2026-07-02).

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
5. `interrupted` is not a `stop_reason`. RESOLVED 2026-06-10 (2.1.170
   spike, 6/6 runs): the interrupt result is
   `subtype == "error_during_execution"` + `is_error == true` +
   `errors[] == ["[ede_diagnostic] ..."]` — the old aborted/interrupted
   substrings are GONE, and `is_error` flipped from the previously
   documented `false`. Classification keys on interrupt-ack correlation
   (the ack always precedes the result line); the substring check is
   fallback only. See §"Interrupted-turn result envelope".
6. `assistant_message_id` must be tracked from the last `assistant`
   envelope's `message.id`. Not carried on `result`.

---

## When this doc is wrong

Capture fresh NDJSON (`AGENT_OVERFLOW_DEBUG=provider`), compare
against these shapes, and update this file before writing parser
code against a new assumption. This doc is the single source of
truth for parser behavior; if it's stale, code written against it
will be too.
