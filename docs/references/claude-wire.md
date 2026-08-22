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
| `system` | `parseSystem` | Init, task lifecycle, compact boundary, session status, the [model-fallback family](#system-model-fallback-family), and the [permission notices](#permission-notices-permission_denied-permission_retry). |
| `assistant` | `parseAssistant` | Text / thinking / tool_use blocks, token usage. |
| `user` | `parseUser` | `tool_result` blocks echoed back after tool execution, plus replayed user text (`--replay-user-messages`, `isReplay:true`) — see [§Outbound user message](#outbound-user-message--client-supplied-uuid---replay-user-messages). |
| `stream_event` | `parseStreamEvent` | Incremental deltas (requires `include_partial_messages:true`). |
| `result` | `parseResult` | **Turn-complete signal.** One per CLI turn. |
| `control_request` | `parseControlRequest` | Bidirectional. Inbound: `can_use_tool`, `exit_plan_mode`. Outbound (client → CLI): `interrupt`, `stop_task`, `set_permission_mode`, `mcp_set_servers`, `mcp_authenticate`, `mcp_oauth_callback_url`, `mcp_status`. |
| `rate_limit_event` | `parseRateLimitEvent` | Rate limit state changes. |
| `command_lifecycle` | `parseCommandLifecycle` | Delivery ack for a user message written to stdin, keyed by the client-minted `uuid`. See [§command_lifecycle](#command_lifecycle--stdin-message-delivery-acks-verified-21219). |

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
- `fast_mode_state` / `fast_mode_disabled_reason` — see
  [§fast_mode_state](#fast_mode_state--fast_mode_disabled_reason-verified-21219)

### `fast_mode_state` / `fast_mode_disabled_reason` (verified 2.1.219)

Two OPTIONAL top-level fields the CLI restates on **every `result`
envelope** and on **`system/init`**. They report whether the running
session is actually serving turns in fast mode — which is not the same
question as whether the client asked for it.

```json
{"type":"result", …, "fast_mode_state":"off",
 "fast_mode_disabled_reason":"sdk_opt_in_required"}
```

`fast_mode_state` values seen: `on`, `off`, `cooldown` (paused after a
rate limit).

`fast_mode_disabled_reason` enum on the 2.1.219 binary:
`not_first_party`, `disabled_by_env`, `unknown`, `model_not_allowed`,
`sdk_opt_in_required`, `pending`, `free`, `preference`,
`extra_usage_disabled`, `network_error`.

⚠ **Version tolerance.** The reason field was added between 2.1.105 and
2.1.219. `internal/provider/claude/testdata/real_output.ndjson` (2.1.105)
carries `fast_mode_state` on `result` and no reason key anywhere. An
absent field is therefore **no signal**, never `off` — a parser or UI that
defaults absence to "off" reports a denial the binary never made.
`extractFastModeStatus` returns `(nil, false)` when neither key is
present, and the frontend treats "no report yet" as unknown.

**AO context.** AO passes `--settings {"fastMode":true}` on fast-mode
threads, which DOES satisfy the `sdk_opt_in_required` gate for real
sessions. Seeing `sdk_opt_in_required` on a live thread session is
therefore an AO bug worth surfacing, not a normal state. The one place it
is correct is the zero-token account probe, which never opts in — hence
its presence in
`docs/references/fixtures/claude/initialize_models_20260802.json`.

**AO handling.** Live session state, not history: it flows
`parse_result.go` / `parse_system.go` → `WireTurnCompleteMeta.FastMode` /
`SessionInfo.FastMode` → triage `provider:fast_mode` → the frontend's
per-thread `fastModeState` store, and is never persisted (Core Principle
2 — don't duplicate provider state). The composer's fast-mode menu uses
it to qualify a toggle the provider is not honouring.

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

#### `get_context_usage` (verified 2.1.219)

`get_context_usage` is the canonical `/context` breakdown — the CLI
routes the slash command and this control subtype through one
`collectContextData` path, so the numbers are identical to what
`/context` prints. It returns `totalTokens`, `maxTokens`,
`rawMaxTokens`, `percentage`, `model`, `categories[]`, `gridRows[]`,
per-item drilldowns (`memoryFiles`, `mcpTools`, `agents`,
`slashCommands`, `skills`, `messageBreakdown`), the autocompact state
(`autocompactSource`, `autoCompactThreshold`, `isAutoCompactEnabled`),
and `apiUsage`. Use it when exact category parity is needed
(`totalTokens` matches top-level on non-advisor turns; advisor parity
is still pending an explicit capture); otherwise the passive
`message_delta.usage` top-level is enough for the live meter.

Three properties matter to any consumer:

- **It consumes no turn and makes no API call.** The stream-json input
  loop handles it out of band, so it answers on a session that has
  never received a user message and is safe to issue mid-turn.
- **Deferred categories are listed but NOT counted.** A row with
  `isDeferred:true` (unloaded tool definitions) is excluded from
  `totalTokens`. Summing every row overcounts by the deferred total;
  the non-deferred rows sum to exactly `rawMaxTokens`, with
  `Free space` as the remainder term.
- **`apiUsage` is `null` before the process's first API call**, so it is
  absent on exactly the fresh-session case. It carries the same
  quantity `message_delta.usage` already supplies.

⚠ **Version tolerance.** `autocompactSource` and `messageBreakdown`'s
`redirectedContextTokens` / `unattributedTokens` are newer than the
2.1.88 SDK schema; `deferredBuiltinTools`, `systemTools`, and
`systemPromptSections` are optional and were absent on the 2.1.219
capture. Category NAMES are not an enum — decode them as data.

**AO handling.** On-demand only, and never persisted (Core Principle 2
— the reading describes the provider process right now). The session
method is `Session.GetContextUsage` /
`ParseContextUsage` (`internal/provider/claude/context_usage.go`),
surfaced through the `GetThreadContextUsage` binding and the
`ContextBreakdown.svelte` expansion inside the context meter's popover;
a thread with no live Claude session gets a typed "not available"
answer rather than a synthesized one. AO decodes only
`totalTokens` / `maxTokens` / `rawMaxTokens` / `percentage` / `model` /
`categories[]` — `gridRows` is a terminal-UI artifact, the drilldowns
are already summarised by their category row, and the per-category
`color` is a CLI theme token.

Captured references:
`fixtures/claude/context_usage_control_20260803.summary.json`
(2.1.219 — the control_response shape, the deferred-exclusion
arithmetic, and the version drift above),
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
| `control_response` for `get_context_usage` | Canonical `/context` parity: exact `totalTokens`, `maxTokens`, category breakdown, and `apiUsage`. SHIPPED as the meter popover's "Show exact breakdown" expansion (`GetThreadContextUsage` → `ContextBreakdown.svelte`) — user-initiated, live-session-only, never cached or polled. | Use `totalTokens` directly when actively requested. It does NOT drive the always-on meter: that stays on the passive `message_delta.usage` top-level, which costs nothing and updates every delta. |
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
 "apiKeySource": "none",
 "fast_mode_state": "on",
 "mcp_server_errors": [{"name": "broken", "error": "invalid config: ..."}],
 "capabilities": ["interrupt_receipt_v1", "msg_lifecycle_v1"]}
```

Emits `EventInit`. Parser extracts model id for usage pricing, and the
optional fast-mode pair (same shape and same version caveat as on
`result` — see
[§fast_mode_state](#fast_mode_state--fast_mode_disabled_reason-verified-21219)).
`system/init` is the only fast-mode report a thread gets before its first
turn ends, and the one that reflects a fresh session's spawn flags after
a resume.

### `mcp_server_errors` (2.1.237)

`mcp_servers[]` lists the servers the CLI ACCEPTED. A server whose config
entry it REFUSED is absent from that array entirely, which makes it
indistinguishable from a server that was never configured — so 2.1.237
added a second array naming the rejects with the CLI's own explanation:

```json
"mcp_server_errors": [{"name": "broken", "error": "invalid config: ..."}]
```

Both arrays are optional and the two never name the same server, so a
consumer can iterate both without a collision check. `error` is
provider-authored prose: bound it before it reaches user-facing state.
AO projects both onto the unified MCP status
(`claude/mcpstatus.go` → `internal/mcpstatus`), and every entry it
derives from this array carries a non-empty `Error`.

### `capabilities`

An array of opaque feature tokens the running build advertises
(`interrupt_receipt_v1`, `interrupt_cancel_queued_v1`, `msg_lifecycle_v1`,
`queued_notifications`). Two rules: prefer a token to CLI-version parsing
when one exists for the behaviour, and treat ABSENCE as no statement
rather than as a denial — 2.1.237's stream-json engine under-reports what
it actually implements. `system/init` is re-emitted before every turn, so
anything hung off it must be idempotent.

## `system/status` (verified 2.1.219)

**Fires**: on session-status transitions. Two families share the channel:

- `status:"requesting"` — per-API-request noise, emitted constantly
  during normal turns. Dropped by the parser; nothing may route on it.
- `status:"compacting"` — a compaction (manual `/compact` AND
  auto-compact; both route through the CLI's shared
  `compactConversation`) has started. The window then runs in near-total
  wire silence — observed 108–184s across 17 production auto-compacts —
  and closes with a `status:null` frame carrying `compact_result`:

```json
{"type":"system","subtype":"status","status":"compacting",
 "session_id":"...","uuid":"..."}
{"type":"system","subtype":"status","status":null,
 "compact_result":"success","session_id":"...","uuid":"..."}
{"type":"system","subtype":"status","status":null,
 "compact_result":"failed",
 "compact_error":"API Error: Request was aborted.","session_id":"...","uuid":"..."}
```

On success the `system/compact_boundary` follows ~20ms after the close
frame; on failure/cancel no boundary follows. Auto-compact runs
MID-TURN (tool loop → compacting → silence → close → boundary → summary
user row → turn continues, with no `system/init` in the auto path). The
30s re-emit of the open frame is remote-session-only
(`isSessionActivityTrackingActive`); locally exactly one open frame
fires. Parser (`parseStatusEvent` in `parse_system.go`) maps
`compacting` → `EventCompactionStatus` Active and `compact_result` →
the inactive close (carrying the error string); `requesting` and
unknown statuses are dropped. Triage projects the window onto
`provider:compacting` (see `internal/triage/compaction_status.go`).

## `system` model-fallback family

Four subtypes, one shape, one meaning each: the model the user asked for
is not the model this request ran on. Three of them are NOTICES (the turn
continues on `fallback_model`) and one is an ERROR (the turn produced
nothing). All four spell their fields snake_case ON THE WIRE
(`fallback_model`, `original_model`, `api_refusal_category`, verified in
the 2.1.214 / 2.1.219 / 2.1.237 serializers) while the CLI's internal
object is camelCase; a reader that accepts only one spelling rejects every
real envelope from the other producer, so read both.

`fallback_model` is required on the three notices — without it there is no
"now running as" to report.

| Subtype | Meaning | AO event |
|---|---|---|
| `model_refusal_fallback` | the API refused this request; retried elsewhere | `EventModelFallback` |
| `model_fallback` | the model was unavailable or blocked (`trigger`: `model_not_found`, `permission_denied`, `model_blocked`, …) | `EventModelFallback` |
| `model_consent_fallback` | a credits/consent choice moved the session | `EventModelFallback` |
| `model_refusal_no_fallback` | refused with NO fallback route — the turn is dead | `EventError` |

`meta.kind` is the discriminator; the three notices are one event kind
because the user-visible consequence is identical and only the cause
differs.

### Row identity

`request_id` names the API REQUEST, not the notice, and one request can
produce more than one member of this family (a consent move and a refusal
fallback for the same request). AO's row id is therefore
`model-fallback:<subtype>:<request_id>` — triage upserts on the item id,
so a subtype-less id would render two real events as one.

### `model_refusal_fallback`

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

### `model_consent_fallback`

**Fires**: when a credits/consent choice moves the session onto another
model — the user (or the CLI's stored default) answered a "you are out of
X, continue on Y?" prompt.

```json
{"type":"system","subtype":"model_consent_fallback",
 "content":"Continuing on Opus 4.8.",
 "choice":"continue_on_fallback",
 "original_model":"claude-fable-5","fallback_model":"claude-opus-4-8",
 "persisted_as_default":true,"request_id":"req_..."}
```

Two fields of its own. `choice` is which consent option was taken.
`persisted_as_default` answers whether the move was WRITTEN BACK as the
account default or applies to this session only — AO records it only when
true, because `false` is what the composed sentence already says.

### `model_refusal_no_fallback`

**Fires**: when a refusal has no fallback route. This is the one member
whose turn produces NOTHING, so it is an ERROR, not a notice.

```json
{"type":"system","subtype":"model_refusal_no_fallback",
 "content":"Fable 5's safeguards flagged this message and no fallback model is available.",
 "original_model":"claude-fable-5","api_refusal_category":"cyber",
 "refused_user_message_uuid":"...","request_id":"req_..."}
```

It carries no `fallback_model` — there is none. AO emits `EventError` and
deliberately gives the meta NO top-level `fatal` bool and NO top-level
`error` string: those are how triage recognises a dead PROCESS and an SDK
error enum respectively, and only the TURN died here.

---

## Permission notices (`permission_denied`, `permission_retry`)

Two `system` subtypes that report a permission OUTCOME rather than asking
for one. Neither is an interactive request — the ask, when there is one,
rides `control_request/can_use_tool`.

**Live-wire only.** 2.1.237's two persistence paths BOTH drop these
envelopes (one returns `{type:"ignored"}`, the other a bare `continue`),
so neither ever appears in a session transcript. A resumed, imported, or
forked thread has no record of them and NOTHING may be inferred from their
absence.

### `permission_denied`

**Fires**: where the CLI auto-denies a tool call BEFORE it could ask — the
pre-ask gate. Without this envelope the timeline would show a tool that
silently never ran.

```json
{"type":"system","subtype":"permission_denied",
 "tool_name":"Bash","tool_use_id":"toolu_...",
 "decision_reason":"Bash command not in the allowed list",
 "decision_reason_type":"rule",
 "message":"Claude requested permissions to use Bash, but you haven't granted it yet.",
 "agent_id":"..."}
```

`decision_reason` is the deciding component's OWN sentence and is what to
display — the CLI's debug renderer prefers it over `message`.
`decision_reason_type` is the discriminator of the CLI's
`PermissionDecisionReason` union. The 2.1.237 set is `rule`, `mode`,
`subcommandResults`, `permissionPromptTool`, `hook`, `asyncAgent`,
`sandboxOverride`, `workingDir`, `safetyCheck`, `classifier`, `other` —
and it is an OPEN SET: an unrecognised value must still compose a
sentence rather than drop the notice.

`workingDir` is the only workspace-boundary signal a denial carries, and
the distinction is load-bearing in the COPY: the CLI answers a boundary
refusal with `addDirectories` suggestions, never a `Bash(...)`-style tool
rule, so telling the user to add a permission rule would be advice that
fixes nothing. (`blocked_path` itself rides `can_use_tool`, not this
envelope.)

AO emits `EventNotification` with `meta.kind:"permission_denied"`, on the
model-fallback family's shape rather than a new event kind. Triage
attaches the notice to the tool call under the NAMESPACED row id
`permission-denied:<tool_use_id>` — the `tool_call` row's id is the bare
`tool_use_id`, so an un-namespaced id would collide — and annotates that
row with `Decision="declined"` plus a `permissionDenied` meta block. It
deliberately does NOT touch the row's Status: a row that has left
`statusRunning` makes `persistToolCallCompletion` drop the real
completion, so a denial must never pre-settle the row.

### `permission_retry`

**Fires**: from the interactive REPL's `onRetryDenials` dialog after a
permission-mode change — its only 2.1.237 producer.

```json
{"type":"system","subtype":"permission_retry",
 "content":"Allowed Bash(npm test), Bash(npm run build)",
 "commands":["Bash(npm test)","Bash(npm run build)"],
 "level":"info","isMeta":false,"uuid":"...","session_id":"..."}
```

It carries NO `tool_use_id` and no attempt count: it reports by command
NAME. Parse it as a plain timeline notice with an optional bounded
`commands` list (`EventNotification`, `meta.kind:"permission_retry"`) and
do NOT build a tool correlation on it.

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

## `command_lifecycle` — stdin message delivery acks (verified 2.1.219)

**Fires**: for **every** user message written to the CLI's stdin, direct
sends included — not just mid-turn ones. Spiked 2026-08-02 on claude
2.1.219 with AO's exact flag set.

```json
{"type":"command_lifecycle","command_uuid":"<client-minted uuid>","state":"queued"}
```

`command_uuid` is the **client-minted top-level `uuid`** AO put on the
outbound envelope (see
[§Outbound user message](#outbound-user-message--client-supplied-uuid---replay-user-messages)),
so the ack correlates with no ordering assumptions.

| `state` | Meaning |
|---|---|
| `queued` | The CLI accepted the envelope and holds it. Written immediately on stdin write, before the message reaches the model. |
| `started` | The message reached the model. |
| `completed` | The turn the message drove has finished. |
| `cancelled` | The message will **never** be delivered. |

### Why this is better than the `isReplay` echo

AO's existing confirmation for a queued send is the `user{isReplay:true}`
echo, which can arrive an arbitrarily long time after the stdin write —
the whole remaining turn (documented above under
[§Queued-message consumption](#queued-message-consumption--two-flavors-claude-21202--21205)).
The lifecycle frames are prompt and explicit. They do NOT replace the
echo: the echo is still what confirms the message entered context and
what stamps `provider_item_id` on the row. The acks answer the different
question of what is happening to a message that is still pending.

### Reading the delivery flavour off the ordering

A user message written to stdin mid-turn is **never dropped**. Default
handling is a queue at priority `"next"`:

- During a turn that still has tool iterations, the CLI's queue processor
  drains it **mid-turn** (between a tool result and the next API round)
  and the running turn visibly changes course — one `result`, no second
  `system/init`.
- During pure text streaming with no further tool iterations, it degrades
  to running as the **next** turn after the current one completes.

`started` **before** the running turn's `result` ⇒ delivered mid-turn.
`started` **after** ⇒ it ran as a new turn. AO classifies this in triage
by comparing the wire round open when the `queued` ack landed against the
one open at `started` (`internal/triage/command_lifecycle.go`), because
the enqueue-time round is not recoverable after the fact.

### ⚠ The `priority` field is known and deliberately unused

The queue honours a `priority` on the outbound envelope, and
`priority:"now"` would force immediate mid-turn delivery rather than
"next drain point". **AO does not send any `priority` field** — hard
steer is a separate, on-hold scope decision, not an oversight. Documented
here so the next reader does not re-derive it, and so adding it stays a
deliberate act. The same drift caveat as the client-supplied `uuid`
applies: this is an undocumented binary contract, spike before relying on
it.

### Version tolerance

Older CLIs emit no `command_lifecycle` frames at all. Absence must never
strand UI state: AO's queue events alone drive the pending overlay, the
`isReplay` echo remains the confirmation signal, and every lifecycle
detail is purely additive labelling. AO also drops any frame whose
`state` it does not recognise rather than forwarding an unhandled value.

### AO handling

`parse_command_lifecycle.go` → `EventCommandLifecycle` → triage
`handleCommandLifecycle`, which resolves the AO row id from the
pending-send registry and emits `provider:command_lifecycle`. Live UI
state only; nothing is persisted. The correlation is registered at
`queued` and survives the wire echo popping the pending-send FIFO, so
both arrival orders resolve.

### A `command_uuid` the client never minted (verified 2.1.237)

Every `command_uuid` above is one AO stamped on an outbound envelope —
with ONE exception, and it is the whole tell for a turn this app did not
ask for. When cross-session messaging is on and a peer's `SendMessage` is
accepted, the CLI mints its own uuid for the user row it injects and
opens the bracket with it:

```
command_lifecycle{state:"started", command_uuid:"<CLI-minted>"}
system/init                       <- the turn re-emits init
user{isReplay:true,isSynthetic:true,uuid:"<the same uuid>",origin:{kind:"peer",...}}
assistant …
result
command_lifecycle{state:"completed", command_uuid:"<CLI-minted>"}
```

`started` precedes even the `system/init`, so the bracket is the FIRST
observable of a peer turn. AO keeps a ledger of the uuids it issued
(`internal/provider/claude/session_peer.go`) and stamps
`Meta.origin = "peer-session"` on every frame of a bracket whose uuid the
ledger positively lacks — every frame, not just `started`, so a consumer
reading the terminal frame reaches the same verdict. The classifier fails
SAFE: an unknown session or an overflowed ledger reads as ours, because
labelling the user's own message "from another Claude session" is the
transcript lying about who asked for what.

Observed states for a peer turn are `started` and `completed` only. No
`queued` and no `discarded` were seen for one in any spike run
(2026-08-21, /tmp/spike-xsession) — a refused or held delivery produces
NOTHING on the receiver's stdout at all, not even a lifecycle frame, so
"a peer message expired" has no wire form to render.

---

## Cross-session messaging (harbor kite, 2.1.224+ / verified 2.1.237)

One Claude session on a host can discover (`ListAgents`) and address
(`SendMessage`) another. The pieces, all spike-verified against 2.1.237
under AO's own flag set:

**Gate.** The feature is behind a GrowthBook experiment with exactly one
environment override, `CLAUDE_CODE_HARBOR_KITE` — parsed as a boolean, so
`"0"` and `""` are off. With the gate open the session binds a unix
socket at `(XDG_RUNTIME_DIR || CLAUDE_CODE_TMPDIR || tmpdir)/cc-socks/<pid>.sock`
and `system/init` carries `messaging_socket_path`, with `ListAgents`
joining `tools[]` (`SendMessage` is present either way). Without the gate
the CLI logs "cross-session messaging gate off" and binds nothing, which
no settings key can undo. A hidden `--messaging-socket-path <path>` flag
overrides the socket location.

**Discovery** is keyed on a SHARED `CLAUDE_CONFIG_DIR`: the registry is
`<CLAUDE_CONFIG_DIR>/sessions/<pid>.json` plus a keyfile, deleted on
exit. AO's Claude sessions CLEAR that variable (`NewSession`'s
`UnsetEnv`), so they land in the user's own `~/.claude` and can see — and
be seen by — sessions the user ran in a terminal.

**Name.** The peer-visible name comes from `--name` / `-n` at spawn or
`CLAUDE_CODE_SESSION_NAME`; the default is the cwd BASENAME, which would
name every AO thread of one project identically. The CLI's normalizer is
`trim` → collapse each run of `\p{Cc}\p{Cf}\u2028\u2029` to one space →
drop residual `\x00-\x1f\x7f-\x9f` → truncate to 200 CODE POINTS →
`trim` (mirrored by `claude.SanitizePeerSessionName`). Two rename paths
exist and only one moves the registry: the `rename_session` control
request changes the TITLE only, while `/rename <name>` sent as an
ordinary stdin user message moves the registered name. `/rename` is a
LOCAL command — `result` comes back `num_turns: 0` with cost unmoved, no
request reaches the model — so a live rename is free.

**Inbound policy** is the `crossSessionInbound` key in the `--settings`
block: `accept` / `hold` / `refuse`.

| Value | Behavior |
|---|---|
| `accept` | Delivered as a turn. The only value that produces anything on stdout. |
| `hold` | Parks awaiting approval with NO expiry timer; settled "expired" only at shutdown. Nothing on stdout. |
| `refuse` | Dropped silently. The SENDER still gets `success: true`. |
| absent | MODE PARITY, not "off". A sender asserting no permission-mode class holds with cause `no-mode-asserted`, which DOES arm `CLAUDE_CODE_USER_DIALOG_TIMEOUT_MS` (default 5m) and then drops the message. Mode attestation is behind a second flag, `tengu_harbor_kite_mode_emit`, with no environment override. |

Agent Overflow therefore never writes `hold` and never leaves the key
unset AT ALL. A headless session has no approval surface, so `hold` and
parity's hold both silently discard — and parity's DELIVERING outcome is
worse: `tengu_harbor_kite` can bind the inbox remotely for a user who
never enabled the feature in AO, and with `tengu_harbor_kite_mode_emit`
also live a class-matching peer would auto-deliver a turn into that
thread. Neither flag is something AO controls, and the environment
override cannot express "off" (it is checked for truthiness and falls
through to the flag when unset), so a disabled session spawns with an
explicit `"crossSessionInbound":"refuse"`.

**Delivery shape.** Because AO always spawns with
`--replay-user-messages`, the message itself reaches stdout — no
transcript read is needed:

```json
{"type":"user","isReplay":true,"isSynthetic":true,
 "uuid":"<== the bracket's command_uuid>",
 "message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/cc-socks/3896836.sock\" from-name=\"BETA\">\nPEER PAYLOAD from BETA\n</cross-session-message>\n\n…"},
 "origin":{"kind":"peer","from":"uds:/tmp/cc-socks/3896836.sock",
           "verifiedPeerPid":3896836,"msg_id":"…","name":"BETA",
           "body":"PEER PAYLOAD from BETA"}}
```

The structured `origin` object is the better source — it carries the
peer's registered NAME, which the wrapper's `from` (a socket path) does
not — and the wrapper parse is the fallback for CLIs that predate it.
`origin.kind` is checked rather than assumed: `origin` is a general
envelope slot.

**`SendMessage` input** accepts `to` / `message` and echoes the CLI's own
canonical `recipient` / `content` alongside them.

**AO handling.** `parse_user_replay.go` lifts the message and flags it
`cross_session_message` / `cross_session_from` /
`cross_session_from_name` / `origin: "peer-session"`; triage
(`handle_user_text.go`) persists it as a `user_text` row under
`user:peer:<uuid>` rather than as injected context, because the
provenance is positively known. The wrapper stays in
`InjectedUserContentWrappers` for the fork-point detector, and the live
path deliberately branches on it BEFORE the wrapper suppression — the
peer's text is real conversation content.


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

⚠ The envelope above is the TOP-LEVEL shape. A SIDECHAIN launch (a
subagent launching its own async agent) carries the same ack body with
**no `tool_use_result` at all**, so none of these markers exist on it —
see [§E5b](#e5b--sidechain-async-launch-tool_use_result-is-omitted).

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

### E5b — Sidechain async launch: `tool_use_result` is OMITTED

**The E5 envelope above is the TOP-LEVEL shape only.** When the launch
comes from a SUBAGENT — a sidechain line, `parent_tool_use_id` set, i.e.
an async agent launching its own async agent (depth 2) — Claude Code
emits the identical ack BODY but no `tool_use_result` object at all.

Verified from a live capture (2026-08-19,
`provider-events-2026-08-19.ndjson.1`, sessions `a36a622b` and
`ed8a5c81`; re-confirmed 2026-08-21). Across 17 async-launch acks in one
file the split is clean and by depth, not by build:

| Line | `parent_tool_use_id` | Top-level keys |
|---|---|---|
| top-level ack | `null` | message, parent_tool_use_id, session_id, timestamp, **tool_use_result**, type, uuid |
| sidechain ack | `toolu_…` | message, parent_tool_use_id, session_id, **subagent_type**, **task_description**, timestamp, type, uuid |

The sidechain ack replaces the structured sibling with two top-level
scalars (`subagent_type`, `task_description`) that carry none of the
E5 markers — no `isAsync`, no `status`, no `agentId`, no
`backgroundTaskId`. `message.content[0]` is a `tool_result` whose
`content` is a text-block array, and the text is byte-identical in shape
to the top-level ack's:

```
Async agent launched successfully. (This tool result is internal metadata — never quote or paste any part of it, including the agentId below, into a user-facing reply.)
agentId: a126ec31b78a8dfc6 (internal ID - do not mention to user. Use SendMessage with to: 'a126ec31b78a8dfc6', summary: '<5-10 word recap>' to continue this agent.)
The agent is working in the background. …
output_file: /tmp/…/tasks/a126ec31b78a8dfc6.output
…
```

#### Symptom before the fallback existed

All four background signals in `parse_user.go` miss, `is_background`
stays false, and the ack's `EventToolComplete` settles the launch row in
place — so a depth-2 async agent rendered as an instantly-DONE card
whose body was the internal ack metadata (the text that explicitly says
never to show it). Downstream, triage's foreground gates
(`writeBackgroundCompletionSibling`'s `!launch.IsBackground`) then
dropped every `task_updated` / `task_notification` for that agent, so
kills and completions vanished silently.

#### Terminal delivery is unaffected — promoting is safe

The same capture shows each sidechain-launched agent getting the full,
ordinary lifecycle on TOP-LEVEL `system` envelopes: `task_started`
(carrying the correct `task_id ↔ tool_use_id` binding),
`background_tasks_changed`, a `task_progress` stream, and terminal
`task_updated {patch:{status:"killed"}}` + `task_notification`. Only the
`user` ack envelope loses its structured half; nothing about the task
lifecycle is sidechain-specific.

#### Text fallback (the discriminator)

`asyncLaunchAckAgentID` in `parse_user.go` classifies from the ack TEXT,
under three CONJUNCTIVE conditions — all three must hold, and failing
any of them leaves the pre-fix behaviour rather than promoting:

1. **No `tool_use_result` on the block.** When the structured sibling
   exists it stays the sole authority; the text is never sniffed. This
   is what keeps an INLINE agent's real completion (`{agentId, status:
   "completed"}`, §E5 discriminator subtlety above) out of the path.
2. **The `tool_use_id` was observed as Claude's agent-launch tool**
   (`Agent`, or `Task` on older builds — `isAgentLaunchToolName`). The
   marker is reliably in place: the assistant envelope carrying the
   launch precedes its ack on the same sequentially-parsed stream, and
   `local_agent` tasks die with their CLI process, so no pre-restart
   agent can ack onto a fresh parser.
3. **The text passes both halves of the ack test** — an EXACT PREFIX
   match on `Async agent launched successfully.` (a single literal in
   the 2.1.237 bundle, one occurrence, verified by binary grep) AND a
   line-anchored `agentId: <lowercase-hex>` from which an id is
   recoverable. The id's LENGTH is deliberately not asserted (17 chars
   observed).

Condition 3's two halves are both required because either alone is
reachable by non-acks: the sentence alone appears on the §E6 resume ack
(which has no `agentId` line) and in any agent's prose about this code
path, while `agentId:` lines appear in ordinary agent output. A prefix —
never a contains — is what keeps tool output that merely quotes the
sentence from classifying.

**No agentId ⇒ no promotion via the TEXT path, deliberately.** An ack
whose agent id cannot be recovered has nothing the text alone can
correlate its terminal against. When `system/task_started` was observed
for the tool_use, the liveness signal below still promotes it — safely,
because task_started already recorded the terminal's route; only when
BOTH paths miss does the launch settle as the instantly-done card.

Once promoted the two behaviours are the top-level ones verbatim:
`is_background: true` on the completion meta, and `rememberTaskToolUse`
re-seeding `agentId ↔ tool_use_id` (idempotent against the binding
`task_started` normally supplies a few ms earlier). Regression guards:
`TestAppendToolResultBlock_SidechainAsyncLaunchAckPromotesToBackground`
plus the three refusal tests beside it in `parse_user_test.go`.

#### Terminal-before-result ordering (the typed discriminator)

An awaited (inline) `local_agent` launch resolves its terminal
`task_updated` BEFORE its real `tool_result` arrives — 0–45ms before,
across all 34 awaited completions in three weeks of wire logs
(2026-07-31 → 2026-08-21, 247 `local_agent` task_starteds total). Every
tool_result that instead arrived while its task was still live was an
ack: 184 top-level §E5 launch acks, 10 sidechain §E5b acks, and 14 §E6
resume acks (whose wording differs — a reason text matching alone is
not enough). Parser signal (5) (`liveAgentTaskToolUses`, parse_user.go)
encodes this: `task_started(task_type:"local_agent")` arms the
tool_use, the terminal `task_updated` disarms it, and a tool_result
carrying NO `tool_use_result` that lands while armed promotes to
background regardless of its text. `local_bash` never arms (a
foreground Bash result's ordering is not part of this contract), and a
present `tool_use_result` stays the sole authority. This is the signal
that survives an ack rewording and covers the no-`agentId` refusal
above; the §E5b text fallback in turn covers session-JSONL replay,
where no `system` envelopes exist. Guards:
`TestAppendToolResultBlock_LiveAgentTaskPromotesRewordedAck` and the
three tests beside it.

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
  `model` (the same string `--model` accepts) and optionally
  `system_prompt` — those two are the subtype's whole parameter set in
  the 2.1.219 dispatcher (binary disassembly, 2026-08-12). Verified on
  2.1.205: the CLI acks immediately even mid-turn, the in-flight turn
  finishes on the previous model, and the next turn (plus the fresh
  `system/init` it emits) runs on the new one. A `[1m]`-suffixed model
  string is accepted and switches the CONTEXT TIER live too: the
  `context-1m-2025-08-07` beta header appears on (or disappears from)
  the next API request (verified 2.1.219 via a local
  `ANTHROPIC_BASE_URL` capture sink, 2026-08-12). Used by AO's config
  reconciler (`app_session_config.go`) so a model or context-window
  change never kills a working session. The CLI also echoes a replayed
  user envelope containing
  `<local-command-stdout>Set model to ...</local-command-stdout>`.

  **`system_prompt` (bundle-read 2.1.214 / 2.1.219 / 2.1.237, NOT
  spiked against a live CLI).** The field is `@internal` and appears in
  no changelog entry, so everything here is `rg -a -o -N` over the
  shipped bundles:

  - **Validation is up front and strict.** `system_prompt` must be a
    non-empty string when present: `{ok:false, error:"set_model:
    system_prompt must be a non-empty string when present"}` otherwise.
    There is therefore **no revert-to-built-in form** — a session that
    started with `--system-prompt-file` can only get the CLI's own
    prompt back by respawning without the flag.
  - **The model gates the prompt.** The handler resolves and validates
    the model FIRST; an unrecognized or disallowed model answers
    `{ok:false}` with the prompt untouched, and marks telemetry
    `system_prompt_switch: "model_switch_rejected"`. A rejected
    `set_model` means **neither** axis applied — never "the model failed
    but the prompt landed".
  - **The setter is an unguarded assignment.**
    `setSystemPrompt:(v)=>{p.systemPrompt=v}` onto the same slot
    `--system-prompt-file` fills at spawn (the flag is read into that
    string before the query starts). So populating an EMPTY slot is not
    a special case: turning an override on live is as valid as swapping
    one for another.
  - **Old builds and other transports ack without applying.** The
    field's own schema doc says so. That is the one failure mode with no
    wire signal at all, which is why AO version-gates
    (`minLiveSystemPromptCLIVersion`) and treats an unknown version as
    too old.
  - **Success says nothing.** The payload is a bare `{ok:true}` — no
    applied model, no confirmation the prompt landed. A family-alias
    step-down also answers ok (telemetry `model_switch:
    "family_alias_stepped_down"`), so `get_settings` is the only channel
    that reports what is actually running.
- `subtype: "set_max_thinking_tokens"` — set the live session's thinking
  budget and/or display. Takes `max_thinking_tokens` (int or null) and
  `thinking_display` (`"summarized"` / `"omitted"` / null); both are
  optional and either alone is a legal request. AO sends it for the
  settings-level thinking axis (`internal/provider/claude/live_update.go`,
  `LiveUpdate.Thinking`). Verified accepted on 2.1.205; behavior
  spike-proven on 2.1.237 against an isolated config + a local API sink:

  ```json
  {"type":"control_request","request_id":"…",
   "request":{"subtype":"set_max_thinking_tokens",
              "max_thinking_tokens":2048,"thinking_display":"omitted"}}
  ```

  answered `{"type":"control_response","response":{"subtype":"success",
  "request_id":"…"}}`, and the NEXT turn's `/v1/messages` request carried
  `thinking: {"budget_tokens":2048,"type":"enabled","display":"omitted"}`.
  Four facts that decide how it can be used:

  - **The budget only binds on models that take an explicit budget.**
    sonnet-4-5 went 31999 → 2048; on an adaptive-thinking model only
    `display` changes. (`CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1` is what
    makes the budget matter everywhere.)
  - **`max_thinking_tokens: 0` is DISABLE**, not "unset": the request
    becomes `thinking: {"type":"disabled"}` and `display` is dropped.
  - **`max_thinking_tokens: null` is a NO-OP.** It is accepted and
    changes nothing — there is no reset-to-default form, so returning a
    session to the CLI's own choice requires a respawn.
  - **Bad types are refused with a single message**:
    `set_max_thinking_tokens: max_thinking_tokens must be an integer or
    null and thinking_display must be "summarized", "omitted", or null`.

  There is still no
  `set_effort` or `set_fast_mode` control_request as of 2.1.219 (the
  dispatcher's full subtype list: `interrupt`, `stop_task`, `set_model`,
  `set_permission_mode`, `set_max_thinking_tokens`, `set_cwd`,
  `get_context_usage`, `get_usage`, `rename_session`,
  `file_suggestions`, the four `mcp_*` subtypes) — but effort and fast
  mode ARE live-settable through the provider-executed `/effort` and
  `/fast` slash commands; see
  [§Live config commands](#live-config-commands-effort-and-fast).
- `subtype: "get_settings"` — read the effective merged settings and the
  raw per-source ones. Takes no parameters. Present in 2.1.237 and absent
  from the 2.1.219 subtype list above; it appears in no changelog entry,
  so AO does not version-gate it — it sends once and remembers an
  "Unsupported control request subtype" error response as unsupported for
  the life of the session. The response spreads the effective/source
  merge and adds (bundle-read 2.1.237):

  ```
  {
    ...effective+sources merge,   // per-source raw settings, keyed
                                  // userSettings / projectSettings /
                                  // localSettings / policySettings / ...
    applied: {
      model:     string,          // what is ACTUALLY running
      effort:    string | null,   // null when the model has no effort axis
      advisor:   string | null,
      ultracode: ...
    },
    errors?: [{file, path, message}]   // severity !== "warning" only
  }
  ```

  `applied.model` is the only wire channel that reports a family-alias
  step-down, and `applied.effort` is the authoritative confirmation of an
  `/effort` apply — AO uses both instead of parsing the command's reply
  text (`app_claude_live_config.go`). A `projectSettings` /
  `localSettings` source carrying a `model` or `effortLevel` that differs
  from what AO asked for is recorded as a
  `claude.SettingsOverrideNotice`.
- `subtype: "mcp_set_servers"` — in-process diff-reconcile of the
  live MCP server set against `servers`. Returns
  `{added, removed, errors}`. Used by AO to sync per-thread MCP
  toggles without respawning the session.
- `subtype: "mcp_authenticate"` — start the OAuth handshake for an
  http/sse MCP server. Takes `serverName` (plus an optional
  `redirectUri`; a custom one the authorization server rejects falls
  back to localhost). Returns either
  `{authUrl, requiresUserAction: true, callbackExpected,
  redirectScheme, state, callbackPort}` when a browser hop is needed,
  or a bare `{requiresUserAction: false}` when the flow settled
  without one — the second form carries NO `authUrl` and is a
  success, not a malformed body.
- `subtype: "mcp_oauth_callback_url"` — post the captured callback
  URL back to the CLI to finish OAuth when the browser landed
  somewhere other than the CLI's loopback listener. Takes
  `serverName` and `callbackUrl`. Only resolves against a flow the
  SAME session started via `mcp_authenticate`; otherwise
  `No active OAuth flow for server: <name>`.

> ⚠ **Key spelling is per-handler and unguessable.** The CLI
> destructures the fields it wants off `request` and validates
> nothing, so a wrong key is not an error — the field reads
> `undefined` and the handler fails on the value. `mcp_authenticate`
> and `mcp_oauth_callback_url` are camelCase (`serverName`,
> `callbackUrl`); `stop_task` is snake (`task_id`); `set_model` mixes
> both (`model`, `system_prompt`). AO sent `server_name` to
> `mcp_authenticate` until 2026-08-21 and got
> `Server not found: undefined` for every server, which read as an
> MCP-plugin bug because plugin servers were the only ones that ever
> needed OAuth. Re-derive spellings from the installed binary and pin
> them in `TestControlRequestWireKeys`
> (`internal/provider/claude/control_request_wire_test.go`).
- `subtype: "mcp_status"` — read-only snapshot of current MCP server
  state. No additional params. See [§mcp_status](#mcp_status) below.

Plugin MCP servers participate in all of these under their qualified
`plugin:<plugin>:<server>` name, and only that name — the bare server
name is refused with `Server not found: <name>`. They report
`scope: "dynamic"` in `mcp_status`, never `"plugin"`. `mcp_reconnect`
refuses a server sitting in needs-auth with `Server status:
needs-auth`; that refusal comes from inside the reconnect
implementation, AFTER name resolution, so it is not a name problem and
reconnect can never substitute for sign-in. All verified against
2.1.237 (2026-08-21 spike).

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

## `initialize` control_response — `models[]`

The `initialize` control_response AO already sends on every account probe
carries a `models` array alongside `account`. The separate `list_models`
control_request returns the **same array** — verified byte-identical on 2.1.219
— so AO reads it off `initialize` and never spends a second subprocess on it.

Captured fixture:
[`docs/references/fixtures/claude/initialize_models_20260802.json`](fixtures/claude/initialize_models_20260802.json)
(2.1.219, trimmed to `models` + `account` + the fast-mode keys, identity
anonymised).

```json
{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init",
 "response":{"models":[
   {"value":"default","resolvedModel":"claude-opus-5[1m]",
    "displayName":"Default (recommended)",
    "description":"Opus 5 with 1M context · Best for everyday, complex tasks",
    "supportsEffort":true,"supportedEffortLevels":["low","medium","high","xhigh","max"],
    "supportsAdaptiveThinking":true,"supportsFastMode":true,"supportsAutoMode":true},
   {"value":"opus[1m]","resolvedModel":"claude-opus-5[1m]","displayName":"Opus (1M context)", …},
   {"value":"claude-fable-5[1m]","resolvedModel":"claude-fable-5","displayName":"Fable", …},
   {"value":"sonnet","resolvedModel":"claude-sonnet-5","displayName":"Sonnet", …},
   {"value":"haiku","resolvedModel":"claude-haiku-4-5-20251001","displayName":"Haiku",
    "description":"Haiku 4.5 · Fastest for quick answers"}
 ], "account":{…}}}}
```

Fields (the descriptions are the CLI's own zod `.describe()` strings):

| Field | Meaning |
|---|---|
| `value` | Identifier to use in API calls — an alias, the `default` pointer, or a canonical id. |
| `resolvedModel` | Canonical wire id `value` resolves to. Optional. |
| `displayName` | Human-readable name **of the row**. |
| `description` | Picker prose. Content varies by auth mode — the API-key capture carries `$2/$10 per Mtok` pricing the subscription capture omits. |
| `supportsEffort` / `supportedEffortLevels` | Effort support; levels are `low`/`medium`/`high`/`xhigh`/`max`. |
| `supportsAdaptiveThinking` | Claude decides when and how much to think. |
| `supportsFastMode` | Fast-mode capable. |
| `supportsAutoMode` | Can run under `--permission-mode auto`. |
| `disabled` | Visible but not selectable (an org's Zero Data Retention setting excluding it); the reason is folded into `description`. **Never observed in a capture.** |
| `promoListPrice` | Struck-through pre-promo price (`"$3/$15"`) for a model on a launch promo. |

### ⚠ It is a picker shortlist, not a catalog

Four properties that decide how it can be used:

1. **Five rows, aliases included.** `default`, `opus[1m]`, `claude-fable-5[1m]`,
   `sonnet`, `haiku` — an alias space and an id space share one `value` field.
2. **No context windows.** The array says nothing about 200k vs 1M.
3. **Older models are absent.** opus-4.x and sonnet-4-6 do not appear on
   2.1.219 and still run. Absence is not a denial.
4. **`[1m]` is baked into id strings, inconsistently.** `opus[1m]` resolves to
   `claude-opus-5[1m]` (marker kept) while `claude-fable-5[1m]` resolves to
   `claude-fable-5` (marker dropped). Consumers must strip it from both sides.

AO's consumer is `internal/claudemodels`, which merges the array into the
hand-maintained catalog under those constraints (`internal/claudemodels/AGENTS.md`
carries the policy). One real discrepancy the array settled: **Haiku reports no
effort support at all**, under both subscription and API-key auth, while AO's
catalog declared low/medium/high — the catalog was corrected.

---

## Slash commands (provider-executed)

Verified on **claude 2.1.219** by a 2026-08-03 live probe using AO's exact
flag set (`--output-format stream-json --input-format stream-json --verbose`).
Every command exercised was a zero-token local one.

The CLI executes a whole class of commands itself — built-ins (`/usage`,
`/context`, `/cost`), skills, user/project commands, plugin commands, and MCP
prompts. None of them make an API call.

### ⚠ The CLI routes stdin user messages, and a routed message never reaches the model

A `user` message whose text starts with `/` goes to the CLI's own command
router. Three verified consequences:

1. **It executes.** `{"type":"user","message":{"role":"user","content":[
   {"type":"text","text":"/usage"}]}}` runs `/usage` CLI-side, `num_turns: 0`,
   no API call. **Routing happens for the array-of-content-blocks shape too** —
   AO's wire shape from `buildUserMessageBlocks` is not a hiding place.
2. **An unknown name swallows the WHOLE message.**
   `"/workflow run nightly\n\n[appended block]"` produces the assistant text
   "Unknown command: /workflow" and `result{subtype:"success", num_turns:0}`.
   The model never sees any of it, and the swallow is silent — a `success`
   result, not an error.
3. **First-word shape decides.** Command-shaped words (`/workflow`,
   `/zzz-not-a-real-command`) are routed; a word with an INTERIOR slash
   (`/etc/hosts on this box …`) is passed to the model as prose.

AO's outbound guard is `internal/provider/claude/slash_guard.go`: when a
message's first word matches `^/[A-Za-z0-9_:-]+(\s|$)` it prefixes a single
`"\n"`, which defeats the CLI's `startsWith('/')` test. `provider.SendOptions.
AllowClaudeSlashCommand` opts a deliberate command out of the guard.

### Local command envelope sequence

Captured fixture:
[`docs/references/fixtures/claude/local_command_20260803.ndjson`](fixtures/claude/local_command_20260803.ndjson)
(hand-written from the probe's shapes).

```
command_lifecycle{state:"queued"}
command_lifecycle{state:"started"}
system/init                          ← re-fires per command turn
assistant                            ← the command output (see below)
user{isReplay:true}                  ← the <command-name> metadata echo
result{subtype:"success",num_turns:0,total_cost_usd:0,result:<same text>}
command_lifecycle{state:"completed"}
```

**The output rides an `assistant` envelope stamped `<synthetic>`:**

```json
{"type":"assistant","message":{
  "id":"…","model":"<synthetic>","role":"assistant",
  "stop_reason":"stop_sequence","stop_sequence":"","type":"message",
  "usage":{"input_tokens":0,"output_tokens":0,
           "cache_read_input_tokens":0,"cache_creation_input_tokens":0},
  "content":[{"type":"text","text":"<command output>"}]},
 "parent_tool_use_id":null,"session_id":"…","uuid":"…"}
```

`<synthetic>` is upstream's `SYNTHETIC_MODEL`
(`claude-code-source-code/src/utils/messages.ts:300`), and
`localCommandOutputToSDKAssistantMessage`
(`src/utils/messages/mappers.ts:196`) is what builds this envelope — it strips
ANSI and unwraps `<local-command-stdout>` / `<local-command-stderr>` before
handing the body over. The comment there records why it is an `assistant`
envelope rather than the dedicated `system/local_command_output` subtype:
mobile clients and session-ingress have no handler for that subtype.

**⚠ `<synthetic>` is not exclusive to command output.** The same sentinel is on
the CLI's own synthesized API-error message
(`createAssistantAPIErrorMessage`), which additionally carries the
`assistant.error` enum. The enum therefore takes precedence in AO's parser: a
`<synthetic>` envelope WITH an error enum is an error, one WITHOUT is command
output. No content matching is involved either way.

Older CLIs (the 2.1.88 source copy) delivered the same body as a
`user{isReplay:true}` envelope wrapped in `<local-command-stdout>`; that wrapper
is in `sessionfork.InjectedUserContentWrappers` and stays suppressed.

**The `<command-name>` metadata echo** rides a `user{isReplay:true}` envelope
on the LIVE wire (not only on resume), preserving the CLIENT-minted uuid:

```json
{"type":"user","message":{"role":"user","content":
  "<command-name>/usage</command-name>\n            <command-message>usage</command-message>\n            <command-args></command-args>"},
 "uuid":"<the uuid AO stamped>","isReplay":true}
```

Aliases echo the CANONICAL name — `/cost` echoes `<command-name>/usage`. The
2.1.88 source filtered this shape out of the SDK stream ("command input
metadata … must not leak", `mappers.ts:160-165`); 2.1.219 emits it. AO
suppresses it through `sessionfork.InjectedUserContentWrappers`
(`<command-name>` … `</command-name>`), which covers both the live
`parse_user_replay.go` path and the fork-point detector.

`result.result` repeats the command output verbatim. `parse_result.go` reads
that field only when building an error message, so it produces no second row.

**No `stream_event` deltas were observed for command output** — the assistant
envelope is a complete snapshot, and AO persists it as one completed
`command_result` row.

### Discovery surfaces (three, none subsuming another)

| Surface | Shape | Notes |
|---|---|---|
| `initialize` control_response `commands[]` | `{name, description, argumentHint}` | The RICH list. 52 entries on a real 2.1.219 install: built-ins, skills, user/project commands, plugin commands. Rides the zero-token account probe. **Omits MCP prompt commands.** |
| `system/init` `slash_commands[]` | `string[]` | Names only. The ONLY surface that includes MCP prompts (`mcp__server__prompt`). Restated per session/resume. |
| `system/commands_changed` | `{commands:[{name, description, argumentHint}]}` | Spontaneous push; contract is **REPLACE your cached list**. |

`name` carries **no leading slash** on every surface (the CLI's zod:
"Skill name (without the leading slash)"). `description` carries provenance
suffixes the CLI renders in its own picker — `"… (user)"`, `"… (project)"` —
passed through as prose, never parsed.

`system/init` also carries two sibling lists AO decodes onto
`provider.SessionInfo`:

```json
{"skills":["ship-it"],
 "plugins":[{"name":"release-tools","path":"/…/plugins/release-tools","source":"local"}]}
```

All three init lists are optional and version-dependent: **absence is no
signal**, never "this session has no commands".

`system/commands_changed` is undocumented and absent from the 2.1.88 source
copy; it was observed on 2.1.219 firing after mid-session skill discovery and
after `reload_plugins` (whose control_response carries the same
`commands`/`agents`/`plugins` triple). AO treats an envelope with a `commands`
key as a replacement — including `"commands": []` — and drops one without the
key, because the two are different statements.

AO consumers: `internal/provider/claude/commands_wire.go` (decode),
`internal/claudecommands` (per-probe-identity cache, replace-only),
`internal/triage/provider_commands.go` (`provider:commands` live projection).

### Live config commands: `/effort` and `/fast`

Verified on **claude 2.1.219** by a 2026-08-12 spike (AO's exact flag set;
every command zero-token, plus one minimal real turn through a local
`ANTHROPIC_BASE_URL` capture sink to prove the request-level effect).
Sanitized fixture:
[`fixtures/claude/effort_live_20260812.ndjson`](fixtures/claude/effort_live_20260812.ndjson).
These are the live path for two axes that have no control_request subtype.

**`/effort <low|medium|high|xhigh|max|ultracode|auto>`** — sets the
session's reasoning effort, effective from the NEXT API request
(`output_config.effort` in the captured request body tracks it exactly).
Facts a client depends on:

- **Session-only.** The success text says so explicitly: `Set effort
  level to <tier> (this session only): <tier blurb>`. Settings files and
  the spawn `--effort` flag are untouched; a restart reverts to spawn
  config.
- **Survives the rest of the session** — later turns, and a `set_model`
  control_request, keep the override.
- **Works mid-turn** like any stdin user message: drained into the
  running turn per §command_lifecycle's mid-turn semantics (spike run 3
  wrote it against an active turn). Either way the in-flight API request
  finishes on the old tier and the next one carries the new tier.
- **Rejection is not a wire error.** A bad argument answers
  `is_error:false`, `num_turns:0` with the text `Invalid argument:
  <arg>. Valid options are: low, medium, high, xhigh, max, ultracode,
  auto`; bare `/effort` answers `Usage: /effort <…>`; `/effort current`
  answers `Current effort level: <tier> (<blurb>)`. Only the `Set effort
  level to ` prefix means a change happened.
- **Availability gate:** `effort` present in `system/init.slash_commands`
  (and the other discovery surfaces).

**`/fast [on|off]`** (bare form toggles) — enables/disables fast mode
live. The failure replies arrive IMMEDIATELY in the command's own result,
not at the next turn boundary.

The handler has four return sites (2.1.237 `Ksw`, and the toggle `eFi` it
tails into), so the reply is always one of five shapes and never anything
else:

| Reply | Meaning | Restart helps? |
|---|---|---|
| `<glyph> Fast mode ON[ · model set to <m>] · <plan>[ (this session only)]` | enabled | — |
| `Fast mode OFF[ (this session only)]` | disabled | — |
| `Fast mode unavailable: <reason>` | an availability gate, reason from the table below | only for the SDK reason |
| `<reason>`, bare | fast mode is off for the WHOLE process — `Ksw` short-circuits before the toggle | no |
| `Unknown argument "<x>". Use: /fast [on|off]` | the CLI did not parse the command | yes (it never ran) |

The bare form is the one worth knowing about, because it has no
`Fast mode unavailable: ` in front of it and a prefix-shaped matcher
misses it entirely. Its guard is the same `!Ru()` the reason table
branches on first, so only two reasons reach it:
`Fast mode is not available` (`disabled_by_env`) and `Fast mode is only
available when using the Anthropic API directly` (`not_first_party`).

Gate reasons, and whether a restart is the recovery:

- `Fast mode is not available in the Agent SDK` — the process was spawned
  without the fast-mode settings opt-in (`sdk_opt_in_required`). A
  restart WITH the opt-in fixes this — it is the one fast-mode
  transition that stays on the restart path. ⚠ It CONTAINS the
  `disabled_by_env` reason above as a substring while meaning the
  opposite thing about restarts, so any matcher for the shorter string
  has to carve this one out explicitly.
- `Fast mode requires usage credits · /usage-credits to turn them on`
  (`extra_usage_disabled`), `Fast mode has been disabled by your
  organization` (`preference`), `Fast mode requires a paid subscription`
  (`free`), `Fast mode unavailable due to network connectivity issues`
  (`network_error`), `Fast mode is currently unavailable` (`unknown`),
  `<model> is not in your organization's allowed models`
  (`model_not_allowed`), `Checking fast mode availability` (`pending`) —
  a restart hits the identical gate; never restart for these.

⚠ Provenance: the success texts come from binary strings (2.1.219,
re-read at 2.1.237), not a wire capture — the spike account had no fast
access, so the fixture holds only the failure replies. Enabling can
IMPLICITLY switch the model (the reply appends `model set to
<fast-capable model>`), which is why ON is a containment match, and why
a client applying model + fast changes together must send `set_model`
BEFORE `/fast`. The ON line also leads with a glyph, so NO arm of this
vocabulary can be matched by prefix.

AO consumers: `internal/provider/claude/live_update.go` sends both
commands (uuid-stamped, slash-guard-exempt);
`parse_command_lifecycle.go` + `parse_assistant.go` correlate the command
output back to the send (`CommandResultMeta.CommandUUID`, valid only
inside the `started`→`completed` lifecycle window);
`app_claude_live_config.go` confirms the answer text or falls back to a
restart.

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

## `auto` permission mode (AI-reviewed approvals)

Spike-verified on **claude 2.1.219** (2026-08-02), captures under the Claude
spike named in `t3-improvements.md` §Decision log. Consumed by
`internal/provider/claude/options.go` (`claudeAutoPermissionMode`) as the
`auto` RuntimeMode tier.

### It functions on a normal install, and the flag bypasses the feature gates

`claude --input-format stream-json --output-format stream-json --verbose
--permission-mode auto` starts and `system/init` echoes
`permissionMode:"auto"`. The CLI does carry statsig gates for auto
(`tengu_harbor_willow`, `tengu_moss_anchor`) plus a settings-source rule
("only policy/user/flag settings may grant auto mode — projectSettings and
localSettings are repo-controllable") and a cached circuit breaker, but all
three sit on the branch that picks a mode when the client asked for none. An
explicit request never reaches them.

`set_permission_mode` accepts it too. The mode set on this release is
`acceptEdits | auto | bypassPermissions | default | dontAsk | plan`; sending
`{"subtype":"set_permission_mode","mode":"auto"}` returned
`control_response{subtype:"success", response:{"mode":"auto"}}` and an
unrecognised value returned the enumerated error. Only `bypassPermissions` is
additionally gated on how the process was spawned, so `auto` ↔ any other
non-`read-only` tier is a live transition.

> There is a SECOND `set_permission_mode` handler in the binary — an
> `[engine] set_permission_mode:auto rejected — gate not enabled` path guarded
> by a feature check. It belongs to the internal engine message loop, not to
> the stream-json control protocol (it logs instead of writing a
> `control_response`). The stream-json handler above is the one AO talks to,
> and it was observed succeeding.

### Decision path

acceptEdits-would-allow → allow (fast path, no classifier call); safe-tool
allowlist → allow; otherwise a two-stage **Haiku** classifier that allows or
**denies**. Fails closed when the classifier is unavailable.

### It falls back to a real ask — the CanUseTool responder stays load-bearing

`tengu_auto_mode_fallback_to_ask` fires with reason `safety_check`,
`ask_rule`, `plan_mode_floor`, `org_ask_ceiling`,
`requires_user_interaction`, `workflow_usage_consent`, or
`mode_changed_while_queued`. The fallback is an ordinary
`control_request{subtype:"can_use_tool"}` on the same channel
`--permission-prompt-tool stdio` installs. A client without a responder hangs.

### ⚠ Headless posture turns the fallback into a deny, and a streak into an abort

Every one of those fallbacks is preceded by
`if (toolPermissionContext.shouldAvoidPermissionPrompts) return deny(...)`, and
the denial-limit check throws `Agent aborted: too many classifier denials in
headless mode` on the same flag rather than reverting to prompting.

That flag has exactly two producers in 2.1.219, both nested-loop constructors:
the `avoid_prompts` permission layer pushed when a tool-use context is forked
for a subagent that does not share the parent's app state, and the subagent
context builder keyed on `agentType` / `requestDialog`. **A top-level
stream-json session is neither.** AO spawns exactly that and supplies a
CanUseTool responder, so it takes the "Classifier denial limit exceeded,
falling back to prompting" path.

### `result.modelUsage` gains a classifier row

An auto turn that ran one Bash call reported two models: the thread's model and
`claude-haiku-4-5-20251001` (530 in / 12 out / `costUSD` 0.00059). AO's
accounting is keyed by wire model name and cumulative-delta based, so the row
is billed correctly and attributed as its own model — a Fable thread will show
a Haiku row. Regression:
`TestParseResult_AutoModeClassifierRowIsAccountedNotDropped`.

### Per-model support flag exists but is not consumed

`initialize`'s `models[]` carries `supportsAutoMode` (true on opus/fable/sonnet,
absent on haiku). AO decodes it (`claude.WireModel`) but does not gate the tier
on it: the flag only exists for the five models the array lists, so consuming
it would read as "auto unsupported" for every model the shortlist omits. See
[§`initialize` control_response — `models[]`](#initialize-control_response--models)
and `internal/claudemodels/AGENTS.md`. Follow-up, not a shipped behaviour.

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
and repairs picks the CLI would reject (off-branch OR filter-dropped,
next section); `resolveClaudeResumeAt` validates explicit cursors at
spawn. `internal/provider/claude/sessionleaf_branch.go` mirrors the row
table above via `sessionfork.TranscriptTypes`.

## Session JSONL: resume deserialization filters (crash tails)

Being on the active branch is necessary but NOT sufficient for a
`--resume-session-at` cursor: the CLI validates the cursor against the
branch chain **after** running `deserializeMessages`
(`conversationRecovery.ts`) over it, so a row the filters drop
hard-fails resume with the same pre-init
`No message found with message.uuid of: <uuid>` error even when it is
physically the file's branch tip. Incident 2026-08-03: a Windows BSOD
killed a 34-minute Bash mid-run; the transcript ended at the assistant
`tool_use` row (its `tool_result` was never written), AO's scan picked
that row — correctly, by file order and branch — and every resume of
the thread failed until the row was repaired.

The filters, in order (source: `utils/messages.ts`, 2.1.219):

1. **`filterUnresolvedToolUses`** — drops every assistant message with
   ≥1 client `tool_use` block where ALL of them lack a matching
   `tool_result` anywhere in the chain. Text in the same message does
   not save it. This is the crash-mid-tool shape.
2. **`filterOrphanedThinkingOnlyMessages`** — drops assistant messages
   whose blocks are all `thinking`/`redacted_thinking` unless another
   *remaining* assistant message with the same `message.id` has
   non-thinking content. Streaming persists one row per content block,
   so dropping a turn-final `tool_use` row (rule 1) usually takes its
   thinking sibling with it.
3. **`filterWhitespaceOnlyAssistantMessages`** — drops assistant
   messages whose blocks are all whitespace-only text ("\n\n" then
   cancel). If ANY row was dropped here, every adjacent user-row run
   is additionally merged into its first row (`mergeUserMessages`),
   erasing the later rows' uuids (the survivor is the run head's uuid
   when the head is non-meta; meta-headed runs depend on the
   HISTORY_SNIP feature flag).

AO enforcement: `sessionleaf_resumefilters.go` mirrors the three
filters over the active chain; `repairLeafForActiveBranch` substitutes
the deepest surviving row (or no cursor at all — always safe) and
`ResumeAtOnActiveBranch` rejects explicit cursors the filters would
drop. The mirror is deliberately conservative — see the file header
for the containment argument around
`recoverOrphanedParallelToolResults`.

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

## System prompt assembly + `--system-prompt` replacement (verified 2.1.234)

Captured via a local `ANTHROPIC_BASE_URL` sink with an isolated fake
`HOME` and a dummy `ANTHROPIC_API_KEY`, running the CLI with AO's exact
spawn flags and env (`CLAUDE_CODE_ENTRYPOINT=agent-overflow`,
`CLAUDE_CODE_ENABLE_TODO_TOOLS=true`, stream-json in/out). The sink
returns a non-retryable 400, so the capture spends zero tokens and
never touches real credentials.

### Request shape

The `/v1/messages` request carries `system` as three text blocks:

1. **Billing header** (~81 chars): `x-anthropic-billing-header:
   cc_version=…; cc_entrypoint=…;`. Internal, immutable.
2. **SDK identity line** (62 chars): `"You are a Claude agent, built
   on Anthropic's Claude Agent SDK."` — **not replaceable**; survives
   `--system-prompt` verbatim.
3. **The body** (~10.5k chars / ~2.6k tokens on Fable 5). This is the
   entire surface `--system-prompt` replaces: with the flag, block 3
   is exactly the given text and nothing else.

The body is **assembled conditionally per model and mode**. Observed on
2.1.234: Fable 5 gets "Communicating with the user", the Fable/Mythos
identity paragraph, and an "operating autonomously" section; Opus 5
under plain `-p` instead gets "Delivering work", "Corrections", and
"Do not call the AgentTool unless the user requested it" lines.
Dynamic sections include Environment (cwd, git flag, platform, model
id, cutoff), the Memory path, a gitStatus snapshot when cwd is a repo,
and a feature-gated Scratchpad section. Any pinned replacement is a
snapshot of one (model, mode) variant.

### What survives replacement (verified with a marker prompt)

- **The `tools` array** — untouched. This is the real context mass
  (~92KB of schemas with AO's flag set; the `Workflow` description
  alone is ~5k tokens).
- **Agent-types + skills listing** — injected as a separate
  `role: "system"` message in the `messages` array, not in `system`.
- **CLAUDE.md contents, auto-memory `MEMORY.md` index, current date**
  — all arrive inside the `<system-reminder>` block of the first user
  message.
- Hooks, the `can_use_tool` control protocol, and mid-conversation
  system reminders.

### What replacement kills (memory feature, dynamic sections)

- The Memory *instructions* section and the CLI's mkdir of
  `~/.claude/projects/<slug>/memory/` — verified with a fresh cwd:
  under `--system-prompt` the project dir gets only the transcript
  jsonl, no `memory/`. Memory **recall** still works (a seeded
  sentinel `MEMORY.md` was injected into the first-user-message
  system-reminder identically with and without the flag), and memory
  bodies are read on demand via the Read tool. A client that wants
  the write side must mkdir the directory itself and carry its own
  instructions text. The slug is the workdir with non-alphanumerics
  mapped to `-` (same layout `internal/sessionimport` walks).
- The Environment block — the model no longer knows cwd/platform/git
  state unless the replacement prompt carries them.

### Related flags (verified 2.1.234)

- `--exclude-dynamic-system-prompt-sections` is genuinely **ignored**
  when combined with `--system-prompt` (help says so; wire confirms —
  no dynamic sections appear anywhere in the request).
- `--append-system-prompt` appends **after the entire default body**,
  including the CLI-injected `<total_tokens>` footer. It cannot remove
  default behavioral text, only argue with it.
- `--system-prompt-file` / `--append-system-prompt-file` exist as
  file-based variants. **`--system-prompt-file <path>` is wire-identical
  to `--system-prompt <text>`** — the same capture run both ways
  produced byte-identical `/v1/messages` requests (same three `system`
  blocks, block 3 exactly the file's content). AO spawns with the FILE
  form for two reasons that have nothing to do with the wire:
  - **`MAX_ARG_STRLEN`.** Linux caps a single argv string at 128KB and
    the limit is not tunable. A rendered system-prompt override
    (`docs/specs/prompt-tool-overrides.md` — `{{GIT_BLOCK}}` expands to
    a repository snapshot) can cross it, and then EVERY spawn fails with
    E2BIG, which the user sees as a session that refuses to start.
  - **`/proc` exposure.** argv is world-readable via
    `/proc/<pid>/cmdline`; the temp file is 0600. A system prompt
    carries workspace paths, git state, and whatever the user wrote.

  `internal/provider/claude/session.go` writes the file at spawn
  (`WriteSystemPromptFile`) and removes it in `Close` and on every
  failed-spawn path.

  **The INTERACTIVE TUI honors the flag too** (verified 2.1.234 by
  running the real TUI under a PTY against the same sink). The
  replacement is total exactly as it is headless; the one difference is
  block 2, which is the TUI's own fixed identity line — `"You are
  Claude Code, Anthropic's official CLI for Claude."` — rather than the
  SDK's `"You are a Claude agent, built on Anthropic's Claude Agent
  SDK."`. Neither is replaceable. So the TUI's `system` array under the
  flag is [billing header, that identity line, the file's content].
  `internal/provider/claudetui/launch.go` passes
  `--system-prompt-file` on the PTY launch and shares the headless
  writer; `claudetui.Session.Close` removes the file, which the
  `NewSession` failure path also runs.
- `--disallowedTools <name>` removes the tool's schema from the
  request `tools` array entirely (verified: `Workflow`,
  `EnterPlanMode`, `ExitPlanMode` absent from the capture) and
  composes with `--system-prompt`. Consistent with §"Permission modes
  for read-only sessions": removal is spawn-only; no control_request
  adds or drops a tool mid-session. **The interactive TUI honors
  repeated `--disallowedTools` identically** (same 2.1.234 PTY spike);
  `claudetui` passes one flag per settings entry. One aliasing quirk:
  the CLI treats `Task` and `Agent` as the same tool, so disallowing
  `Task` removes `Agent` from the request too.

---

## When this doc is wrong

Capture fresh NDJSON (`AGENT_OVERFLOW_DEBUG=provider`), compare
against these shapes, and update this file before writing parser
code against a new assumption. This doc is the single source of
truth for parser behavior; if it's stale, code written against it
will be too.
