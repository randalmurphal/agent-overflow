# internal/provider/claude/

Wraps the Claude Code CLI. One process per active thread, NDJSON over
stdio both ways. The CLI owns OAuth for the parser/session path; we
never touch credentials from any code that talks to the subprocess.

The lone exception is `ratelimits_probe.go`, which reads the OAuth
bearer from `~/.claude/.credentials.json` to make a direct HTTP call
to the Anthropic Messages API. Claude only emits `utilization` on the
NDJSON wire above the 89% warning band, so steady-state rate-limit
rings can only be populated by reading the `anthropic-ratelimit-unified-*`
response headers. The probe is read-only on the credential file and
never writes back.

## Invocation

```
claude --output-format stream-json --input-format stream-json --verbose
```

Session resume uses `--resume <session-ref>`. Fork is replay-based: we
replay from the chosen turn against a fresh session.

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
  blocks, thinking blocks, usage). Dispatches each content block to
  `appendTextEvent` / `appendToolUseEvent` / `appendThinkingEvent` /
  `appendExitPlanModeEvent` / `appendAssistantUsageEvent`.
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
- `ratelimits_probe.go` — out-of-band HTTP probe of the Anthropic
  Messages API. Reads OAuth bearer from `~/.claude/.credentials.json`,
  POSTs a 1-token Haiku request, parses `anthropic-ratelimit-unified-*`
  response headers into a `RateLimitsSnapshot`. Triggered from
  `app_claude_ratelimits.go` (startup, periodic, turn-complete);
  emits go through the standard `provider:usage` channel.
- `mcpstatus.go` — ephemeral MCP status fetcher (`MCPStatusFetcher`,
  driven by `claude mcp list`) plus the `system/init` → unified status
  projectors (`MCPStatusFromRaw`, `MCPStatusFromListLine`) consumed by
  `internal/mcpstatus` via the shared `Fetcher` interface.
  `sanitizeChildStderr` lives here too for bounding child-process
  stderr in user-facing errors.

Parser state method names are part of the contract:

- `take*` / `consume*` methods read and clear parser state. Use them
  only when there is exactly one lifecycle owner for that value, and
  document that owner on the method.
- `peek*`, `has*`, and `lookup*` methods are read-only. If a second
  same-boundary reader appears for a `take*` value, add a `peek*`
  companion rather than smuggling reads through the consuming method.
- State that may span multiple future wire envelopes needs an explicit
  cleanup point (`parseResult`, `Close`, or bounded map eviction).

## NDJSON shapes we handle

⚠ **Authoritative wire reference**:
[`docs/references/claude-wire.md`](../../../docs/references/claude-wire.md).
Read that before adding or changing parser logic — it has the
canonical JSON examples, pinned citations into the Python SDK and
forge, and a list of contradictions/ambiguities we've confirmed.
Don't guess shapes from this guide; `claude-wire.md` is the source
of truth.

Summary of what `ParseLine` dispatches:

- `system` subtypes: `init`, `compact_boundary`, `task_started`,
  `task_updated`, `task_notification`,
  `session_state_changed`, `api_retry`. `tool_progress` is
  intentionally dropped.
- `system.task_started` — meta-only `EventToolStart` emission that
  records the `task_id ↔ tool_use_id` mapping into `items.meta`.
  Fires for EVERY Bash/Task — not just backgrounded ones.
- `system.task_updated` (terminal `patch.status` in
  `{completed, failed, killed}`) — emits
  `EventBackgroundTaskTerminal` keyed by task_id + resolved
  tool_use_id.
- `system.task_notification` — **NOT a completion source**. Emits
  `EventBackgroundTaskNotification` so triage can persist a distinct
  notification row and optionally ingest `output_file` into SQLite.
  See [`claude-wire.md §task_notification`](../../../docs/references/claude-wire.md#systemtask_notification)
  and [`turn-lifecycle.md §Task lifecycle`](../../../docs/architecture/turn-lifecycle.md#2-task-lifecycle-claude-only).
  `parseTaskNotificationEvent` and the synthetic-XML extraction in
  `parse_user_replay.go` share a single
  `buildBackgroundTaskNotificationEvent` so both wire paths produce
  identical inputs for triage. The synthetic-XML path runs when a
  backgrounded subagent completes while a concurrent foreground
  tool_result is in flight; the CLI then delivers the observation only
  via `<task-notification>` inside an `isReplay:true` user envelope.
- `assistant` — text deltas, tool_use, thinking, exit_plan_mode,
  usage. Subagent messages identified by top-level
  `parent_tool_use_id`.
- `user` — `tool_result` blocks. Three variants: standard inline,
  backgrounded placeholder, TaskOutput. All emit `EventToolComplete`
  for their own `tool_use_id` (universal tool-lifecycle invariant);
  TaskOutput ADDITIONALLY emits `EventBackgroundTaskTerminal`.
- `stream_event` — streaming deltas (requires
  `include_partial_messages: true`).
- `result` — **turn-complete signal**. Emits `EventTurnComplete`.
- `control_request`: inbound from the CLI carries `CanUseTool` and
  `exit_plan_mode`. Outbound from us carries `interrupt` (abort the
  current turn — `Session.Interrupt`), `stop_task` (kill a
  backgrounded Bash / Task subagent by `task_id` —
  `Session.StopTask`), `set_permission_mode`, the four MCP control
  subtypes (`mcp_set_servers` / `mcp_authenticate` /
  `mcp_oauth_callback_url` / `mcp_status`, all in `mcp.go`), and
  more. Every outbound subtype shares a single `sendControlRequest`
  helper that owns the allocate/register/marshal/write/await-response
  state machine; each caller adds its own response interpretation
  (or per-success side effect, in `setPermissionMode`'s case).
  Failure to ack within the timeout surfaces as a wrapped error — we
  do NOT kill the session as a fallback (a kill would also reap
  backgrounded tasks, inverting the documented foreground-only
  interrupt behaviour and silently masking a CLI bug). The
  `mcp_status` subtype is the read-only poll Claude exposes for
  post-OAuth state — used by `app_mcp_bindings.go:pollClaudeMCPAfterOAuth`
  to mirror Codex's `mcpServer/oauthLogin/completed` notification on
  Claude. See
  [`claude-wire.md §control_request`](../../../docs/references/claude-wire.md#control_request)
  for the full schema and the verified `stop_task` / `mcp_status`
  flows.
- `rate_limit_event` — rate-limit state.

`parent_tool_use_id` on tool events correlates subagent (`Task`) work
back to the parent tool call.

## Lifecycles we drive

- **Tool lifecycle** — every `tool_use` produces a matching
  `EventToolComplete`. Universal invariant. See
  [`turn-lifecycle.md §Tool lifecycle`](../../../docs/architecture/turn-lifecycle.md#1-tool-lifecycle).
- **Task lifecycle** (Claude-only) — backgrounded tools (Bash with
  `run_in_background:true`, Task subagent) emit
  `EventBackgroundTaskTerminal` via `task_updated` terminal or
  TaskOutput enrichment. Triage writes a `tool_completion` sibling
  row idempotently. User-initiated stop is a client-sent
  `stop_task` control_request (see `claude-wire.md §stop_task`); the
  CLI replies with `control_response{subtype:success}` and fires
  `task_updated` with `patch.status:"killed"` — the same terminal
  channel normal completion uses, routed by task_id.
- **Turn lifecycle** — `result` envelope remains authoritative for
  cumulative turn payload (token usage, cost, and raw
  `terminal_reason` for wire reference). The final
  `assistant_message_id` is tracked from the last in-stream assistant
  `message.id`; it is NOT carried on `result`. `result` is also NOT
  the only source of `EventTurnComplete`: when the parent message ends with a
  "model has stopped" stop_reason (`end_turn` / `stop_sequence` /
  `refusal`) and `parent_tool_use_id` is null, `parse_stream.go`
  emits a typed `provider.SoftRoundCloseMeta` `EventTurnComplete`
  immediately so the working indicator clears even when the CLI
  withholds `result` (it does this whenever a `local_agent` subagent
  is still in flight). The trailing `result` envelope, when it
  eventually arrives, folds in the cumulative payload via
  `persistLateTurnPayload` — see
  [`invariants.md §27`](../../../docs/architecture/invariants.md#27-soft-round-close-from-message_deltastop_reason-is-wire-typed)
  and the
  [`local_agent_outlives.ndjson`](../../../docs/references/fixtures/claude/local_agent_outlives.ndjson)
  fixture.

Do NOT derive turn activity from item state. Do NOT emit lifecycle
state from `task_notification`. Do NOT rewrite `tool_use_id` between
start and complete. These are load-bearing rules enforced by
[`invariants.md`](../../../docs/architecture/invariants.md).

## Captured wire samples (authoritative test fixtures)

- `docs/references/fixtures/claude/ndjson_bash.log` — backgrounded + foreground
  Bash + Read
- `docs/references/fixtures/claude/ndjson_task.log` — Task subagent + TaskOutput
- `docs/references/fixtures/claude/ndjson_outlives.log` — bg Bash outliving its
  turn (the `result` envelope arrives BEFORE `task_updated`)
- `docs/references/fixtures/claude/taskoutput_multi.ndjson` — two parallel bg Bashes
  + blocking TaskOutput
- `docs/references/fixtures/claude/local_agent_outlives.ndjson` — counterpart
  to `ndjson_outlives.log`: bg `local_agent` (Task subagent) at parent
  end_turn — CLI withholds `result` until subagent completes (~10s gap).
  Authoritative for the soft-round-close behaviour.
- `docs/references/fixtures/claude/local_agent_user_input_during_wait.ndjson`
  — same scenario plus a stdin user-message injected mid-wait. Backs the
  composer-unblock safety argument.
- `docs/references/fixtures/claude/local_agent_plus_bg_bash.ndjson` —
  bg Bash + bg local_agent combined: the result-delay is keyed on
  `local_agent` specifically.
- `docs/references/fixtures/claude/advisor_context_usage_20260522.summary.json`
  — sanitized summary across three captures (no advisor, one advisor,
  two advisors). Authoritative for the
  `message_delta.usage.iterations[]` shape and the parent-only-cumulative
  behaviour of the top-level usage. Backs
  `parse_stream.go::lastParentIterationUsage` and the
  `TestParseStreamEventMessageDeltaUsesLastParentIteration*` regression
  set.

Use these in tests via file path. When fresh captures prove wire drift,
refresh the checked-in fixtures from a new `AGENT_OVERFLOW_DEBUG=provider`
run and update `docs/references/claude-wire.md` in the same commit.

## Responsibility boundary

- What BELONGS here:
  - NDJSON parse/marshal for every shape the CLI emits.
  - Per-session correlation maps (task_id, tool_use_id, dedupe sets).
  - Approval response encoding, binary probing, session spawn/read/signal.
- What does NOT belong here:
  - SQLite writes or `app.Event.Emit`.
  - Cross-thread coordination or retry policy.
  - Provider-agnostic event shapes — those live in `internal/provider/`.

## Extension points

- To add a new NDJSON shape: pick the matching `parse_*.go` file (or
  create a new one and list it in Layout above), add a round-trip test
  in the same commit, then wire the event type in shared `provider/`
  types.
- To add a new approval Kind: extend `approvals.go` plus the shared
  `provider.ApprovalRequest`, then wire the frontend branch. See
  `docs/architecture/how-to.md#add-a-new-approval-kind`.

## Anti-patterns

- Do NOT silently drop an NDJSON line. Every type must be handled or
  explicitly logged as "unknown type — ignored". Parser maps must be
  bounded or cleared on Close.
- Do NOT let a parse error kill the read loop. Log with enough context
  to reproduce, keep reading. There is a regression test — keep it
  passing.
- Do NOT touch UI shapes before adding parser + round-trip tests.

## References

- Forge's adapter: `apps/server/src/provider/Layers/ClaudeAdapter.ts` and
  `apps/server/src/provider/claude/*.ts` (full `CanUseTool`,
  subscription probe, Task subagent correlation).
- Upstream SDK: `@anthropic-ai/claude-agent-sdk`.
- `docs/references/spike-policy.md` — if behavior drifts, spike against
  the real CLI before changing this code.
