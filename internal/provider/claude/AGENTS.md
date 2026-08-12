# internal/provider/claude/

Wraps the Claude Code CLI. One process per active thread, NDJSON over
stdio both ways. The CLI owns OAuth for the parser/session path; we
never touch credentials from any code that talks to the subprocess.

The lone exception is `ratelimits_probe.go`, which reads the OAuth bearer
from a selected native credential path to query Anthropic's OAuth usage
endpoint. Claude only emits `utilization` on the NDJSON wire above the warning
band, so the endpoint supplies steady-state and dynamically scoped limits;
legacy unified response headers remain a compatibility fallback. The probe is
read-only on the credential file and never writes back.

## Invocation

```
claude --output-format stream-json --input-format stream-json --verbose
```

Session resume uses `--resume <session-ref>`. Fork is replay-based: we
replay from the chosen turn against a fresh session.

Structured output is session-sticky: `Config.OutputSchema` is passed as
inline JSON through `--json-schema` when the process starts. Per-turn
`provider.SendOptions.OutputSchema` is deliberately ignored by Claude.

## Layout

The parser is split by wire-envelope type so each NDJSON shape has one
owner. The top-level `ParseLine` (in `parser.go`) reads the envelope's
`type` field and dispatches to the matching helper.

- `parser.go` — `Parser` struct, `ParseLine` dispatch, per-parser
  correlation state (background flag, task_id ↔ tool_use_id map,
  dedupe sets, `interruptAcked` — set by the read loop when the CLI
  acks OUR interrupt control_request, consumed by the next
  `parseResult` so 2.1.170's marker-less interrupt results classify
  as user aborts; same-goroutine state, no lock; see
  claude-wire.md §"Interrupted-turn result envelope").
- `parse_system.go` — `system` envelopes (init metadata, compact_boundary,
  model_refusal_fallback, task_started / task_updated / task_notification,
  `commands_changed`, `status`). `commands_changed` is an undocumented push
  whose contract is REPLACE-the-cached-list, so it emits
  `EventCommandsChanged` with the whole list; an empty `commands` array
  is a real replacement while an ABSENT key is dropped. `system/init`
  additionally carries the `slash_commands` / `skills` / `plugins`
  discovery arrays onto `SessionInfo` — `slash_commands` is the only
  surface listing MCP prompt commands (`mcp__server__prompt`).
  `system/status` maps `status:"compacting"` and the `compact_result`
  close onto `EventCompactionStatus`; the per-API-request
  `status:"requesting"` noise is dropped. See claude-wire.md
  §`system/status`.
- `parse_assistant.go` — `assistant` envelopes (text deltas, tool_use
  blocks, thinking blocks, usage). Dispatches each content block to
  `appendTextEvent` / `appendToolUseEvent` / `appendThinkingEvent` /
  `appendExitPlanModeEvent` / `appendAssistantUsageEvent`.
- `parse_user.go` — `user` envelopes carrying `tool_result` blocks,
  split into `appendTaskOutputCompletion` (Task-tool background path)
  and `appendToolResultCompletion` (standard inline path).
- `parse_control.go` — `control_request` envelopes: CanUseTool
  approvals and the exit_plan_mode signal.
- `parse_command_lifecycle.go` — `command_lifecycle` envelopes: the
  CLI's delivery ack for a user message we wrote to stdin, keyed by
  the client-minted `command_uuid`. Send-side correlation lives in
  triage (the pending-send registry those ids belong to); this parser
  additionally tracks the `started`→`completed`/`cancelled` window as
  `Parser.activeCommandUUID` so `parse_assistant.go` can stamp a
  provider-executed command's output (`EventCommandResult`) with the
  command uuid it answers (`provider.CommandResultMeta`) — the
  confirmation channel for `/effort` / `/fast` live applies. Windows can
  nest (a mid-turn message drains into the running turn), so the field
  is last-started-wins with an identity guard on clear; it is safe
  because only `<synthetic>` envelopes are stamped and a command's
  synthetic output sits inside its own window. Unknown `state` values
  are dropped, not forwarded. See claude-wire.md §command_lifecycle.
- `fastmode.go` — `fast_mode_state` / `fast_mode_disabled_reason`
  extraction, shared by `parse_result.go` and `parse_system.go`
  (init). Both keys are optional and version-dependent: absence is
  NO SIGNAL, never "off".
- `parse_stream.go` — `stream_event` envelopes (incremental deltas
  between assistant-message boundaries).
- `usage_accounting.go` — per-turn usage extraction for `result`
  envelopes. The wire's `modelUsage` / `total_cost_usd` are
  SESSION-CUMULATIVE (and `modelUsage` is the only subagent-inclusive
  source); the parser keeps a cumulative snapshot per process and
  `takeTurnUsage` emits per-model per-turn DELTAS onto
  `WireTurnCompleteMeta.{Usage,ModelUsage}`. Cost is wire-reported
  only — no client-side pricing table. Verified against the
  `*_20260703.ndjson` fixtures below.
- `protocol_meta.go` — `compact_boundary` / context-window meta
  normalisation shared across envelopes.
- `context_usage.go` — the `get_context_usage` outbound control_request
  and its response type (`Session.GetContextUsage` /
  `ParseContextUsage`). This is the canonical `/context` breakdown, read
  ON DEMAND from a live session and never persisted — the always-on
  meter stays on the passive `message_delta.usage` signal in
  `parse_stream.go`. Category names pass through as data, not an enum,
  and `isDeferred` rows are excluded from `totalTokens` (summing every
  row overcounts). See claude-wire.md §`get_context_usage` and the
  `context_usage_control_20260803` fixture.
- `approvals.go` — approval-response encoding for the SDK.
- `live_update.go` — `PlanLiveUpdate` / `ApplyLiveUpdate`: the
  restart-free path for config changes a live session can adopt. Model
  and permission mode ride control_requests (`set_model`,
  `set_permission_mode`); effort and fast mode ride the CLI's own
  `/effort` / `/fast` slash commands (uuid-stamped,
  `AllowClaudeSlashCommand` sends), whose confirmation arrives later as
  an `EventCommandResult` carrying the uuid — the app side
  (`app_claude_live_config.go`) settles those. Context-window changes
  ride the `[1m]` marker on `set_model`. `PlanLiveUpdate` returns false
  (restart) for everything else, and `ApplyLiveUpdate` validates every
  axis before ANY side effect — including refusing the command axes
  while the transcript needs the resume-at repair — so a restart-bound
  update never half-applies. Its `preSend` hook fires between
  validation and the first wire write: the caller registers its
  pending-confirmation state there, BEFORE the CLI can possibly
  answer. See claude-wire.md §"Live config commands".
- `session.go` — process lifecycle + read loop that feeds ParseLine.
  The read loop's control_response pre-handler also flags the parser's
  interrupt-ack state (`pendingControlRequests` entries carry
  `isInterrupt`; a successful ack calls `Parser.MarkInterruptAcked`
  before any later `ParseLine` — the CLI writes ack before result,
  verified 6/6 on 2.1.170).
- `message_content.go` — `buildUserMessageBlocks`: shapes the outbound
  `user` message into ordered Anthropic content blocks, placing each
  image at its composer `[Image #N]` marker (inline placement) via the
  shared `provider.SplitContentByImageMarkers`. Headless Claude inlines
  image bytes as base64 (no local-path source on the Messages API).
  Applies `slash_guard.go` before the image split.
- `slash_guard.go` — the outbound slash guard. The CLI routes any user
  message whose FIRST word is command-shaped to its own command router
  and the model never sees it (an unknown one answers "Unknown command:
  /x" with `num_turns: 0`), so AO prefixes a single `"\n"` unless the
  caller opted in via `SendOptions.AllowClaudeSlashCommand`. Default-off
  is load-bearing: AO's own composer commands (`/workflow`) and injected
  wake prompts are prose, not CLI commands. A word with an interior
  slash (`/etc/hosts …`) is not command-shaped and passes through
  untouched. See claude-wire.md §"Slash commands (provider-executed)".
- `commands_wire.go` — decoders + bounds for the two discovery surfaces
  (`initialize` control_response `commands[]`, `system/commands_changed`)
  and the name/plugin arrays on `system/init`. Parsing only; the cache
  lives in `internal/claudecommands`. Names and hints are cut without an
  ellipsis — they are identifiers, not prose.
- `sessionleaf.go` / `sessionleaf_branch.go` /
  `sessionleaf_resumefilters.go` — cold-resume leaf reconstruction for
  `--resume-session-at`. `claudeLeafTracker` (shared with the live wire
  path) picks the leaf in FILE order; `claudeBranchIndex` validates
  that pick against what the CLI's resume will actually accept — the
  ACTIVE BRANCH (parentUuid walk from the file's last transcript row)
  run through a conservative mirror of the CLI's resume
  deserialization filters (dangling client tool_uses from a crash
  mid-tool, orphaned thinking-only rows, whitespace-only rows + the
  user-run merge) — and repairs rejected picks to the deepest
  surviving row. `ResumeAtOnActiveBranch` is the exported spawn-time
  validator, same screen. Row admission for the branch walk IS
  `sessionfork.TranscriptTypes` (one exported set shared with the fork
  transform — no copy to drift). See invariant 28, claude-wire.md
  §"active-branch semantics" and §"resume deserialization filters".
- `models_wire.go` — the `models` array the `initialize` response
  carries (`WireModel`), its canonicalization (`CanonicalSlug` —
  resolvedModel over alias, then `provider.NormalizeModelSlug`, which
  owns both the `[1m]` context-marker trim and the alias folding), and
  the decoder `probe.go` feeds `ProbeConfig.OnModels`. Parsing only;
  the merge policy lives in `internal/claudemodels`. See
  claude-wire.md §`initialize` control_response — `models[]`.
  `DeclaresExtendedContext` is the one place the marker is read as
  evidence rather than trimmed: PRESENCE proves a model can run the 1M
  tier, absence proves nothing.
- `json_helpers.go` — tiny JSON-inspection utilities.
- `options.go` / `probe.go` — non-parser subsystems (session options,
  binary probe).
- `ratelimits_probe.go` — out-of-band HTTP probe of Anthropic's OAuth usage
  endpoint. Reads a bounded, regular native credential file, preserves every
  dynamically returned limit bucket, and falls back to
  `anthropic-ratelimit-unified-*` headers for older servers. Triggered from
  `app_claude_ratelimits.go` (startup, plus a 2-minute poll that runs only
  when a turn completed since the last poll — the endpoint's 429 throttle
  is per-bearer and shared across machines, so turn completion marks
  activity rather than probing); emits go through the standard
  `provider:usage` channel.
- `mcpstatus.go` — ephemeral MCP status fetcher (`MCPStatusFetcher`,
  driven by `claude mcp list`) plus the `system/init` → unified status
  projectors (`MCPStatusFromRaw`, `MCPStatusFromListLine`) consumed by
  `internal/mcpstatus` via the shared `Fetcher` interface.
  `sanitizeChildStderr` lives here too for bounding child-process
  stderr in user-facing errors.
- `sessionfork/` — subpackage. The fork transform over an existing
  transcript, plus the shared reading surface (`TranscriptTypes`,
  `ParseTranscript`, `ResolveParent`, `ResolveLogicalParent`,
  `SessionIDFromPath`) that both the live resume path and the importer
  read transcripts through. Has its own subarea guide.
- `sessionimport/` — subpackage. Read-only reader for
  `~/.claude/projects/…` behind session import: the lite lister (a stat
  plus two 64 KB reads per file, no parse), the conversation DAG and its
  leaf enumeration, the subagent join, and the transcript →
  `internal/importir` event projection. Spawns nothing, writes nothing,
  and never resolves the Claude home itself. It deliberately does NOT
  reuse `claudeBranchIndex` — that answers "which chain will
  `claude --resume` accept" and returns exactly one; import needs every
  leaf, and bending the live resume path to answer both is not worth the
  blast radius (invariant 28). Has its own subarea guide.

Parser state method names are part of the contract:

- `take*` / `consume*` methods read and clear parser state. Use them
  only when there is exactly one lifecycle owner for that value, and
  document that owner on the method.
- `peek*`, `has*`, `is*`, and `lookup*` methods are read-only. If a
  second same-boundary reader appears for a `take*` value, add a
  `peek*` companion rather than smuggling reads through the consuming
  method.
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
  via `<task-notification>` inside an `isReplay:true` user envelope. One
  envelope can coalesce a `<task-notification>` per just-finished task
  (several backgrounded tasks completing together), so the path extracts
  every routable block via `ExtractAllTaskNotificationFields`, not just
  the first.
- `assistant` — text deltas, tool_use, thinking, exit_plan_mode,
  usage. Subagent messages identified by top-level
  `parent_tool_use_id`.
- `user` — `tool_result` blocks. Six variants: standard inline,
  backgrounded placeholder, TaskOutput, async `local_agent` launch ack
  (claude-wire.md §E5 — bare "Async agent launched successfully.",
  `isAsync`/`status:"async_launched"`, no `run_in_background` at
  launch and no `backgroundTaskId` on the wire; discriminated from an
  ordinary inline agent completion, which also carries `agentId` but
  never `isAsync`/`async_launched`), Monitor watch-task launch ack
  (§E7 — `tool_use_result.{taskId, timeoutMs, persistent}`, a
  background `local_bash` launch; `taskId` ALONE is not the
  discriminator — TaskCreate/TaskUpdate task-list acks carry one
  too), and ScheduleWakeup ack (§E8 —
  `{clampedDelaySeconds, scheduledFor, wasClamped[, stopped]}`).
  All emit `EventToolComplete` for their own `tool_use_id` (universal
  tool-lifecycle invariant); TaskOutput ADDITIONALLY emits
  `EventBackgroundTaskTerminal`, and the ScheduleWakeup ack
  ADDITIONALLY emits `EventSessionWakeup` (the pending in-process
  wakeup timer has NO task lifecycle — this event is the only signal
  keeping the idle reaper off a session that will resume itself).
- `stream_event` — streaming deltas (requires
  `include_partial_messages: true`).
- `result` — **turn-complete signal**. Emits `EventTurnComplete`.
- `control_request`: inbound from the CLI carries `CanUseTool` and
  `exit_plan_mode`. Outbound from us carries `interrupt` (abort the
  current turn — `Session.Interrupt`), `stop_task` (kill a
  backgrounded Bash / Task subagent by `task_id` —
  `Session.StopTask`), `set_permission_mode`, `set_model` (live model
  switch, `live_update.go` — acked mid-turn, applies from the next
  turn; verified 2.1.205), the four MCP control
  subtypes (`mcp_set_servers` / `mcp_authenticate` /
  `mcp_oauth_callback_url` / `mcp_status`, all in `mcp.go`),
  `get_context_usage` (the canonical `/context` breakdown,
  `context_usage.go` — answered out of band, consumes no turn and makes
  no API call), and
  more. Every outbound subtype shares a single `sendControlRequest`
  helper that owns the allocate/register/marshal/write/await-response
  state machine; each caller adds its own response interpretation
  (or per-success side effect, in `setPermissionMode`'s case).
  Failure to ack within the timeout surfaces as a wrapped error — we
  do NOT kill the session as a fallback (a kill would also reap
  backgrounded tasks, inverting the documented foreground-only
  interrupt behaviour and silently masking a CLI bug). The
  `mcp_status` subtype is the read-only poll Claude exposes for
  post-OAuth state — used by `app_mcp_auth.go:pollClaudeMCPAfterOAuth`
  to mirror Codex's `mcpServer/oauthLogin/completed` notification on
  Claude. See
  [`claude-wire.md §control_request`](../../../docs/references/claude-wire.md#control_request)
  for the full schema and the verified `stop_task` / `mcp_status`
  flows.
- `rate_limit_event` — rate-limit state.
- `command_lifecycle` — per-message delivery ack for stdin user
  messages (`queued` / `started` / `completed` / `cancelled`). Live UI
  state, never history; older CLIs emit none, so nothing may depend on
  its presence.

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
  channel normal completion uses, routed by task_id. Resuming an idle
  async agent (the harness's SendMessage tool) rebinds `task_started`
  onto the resuming tool_use — AO makes that tool_use the resumed
  round's background carrier (marked backgrounded, Summary rewritten
  to the agent's identity) rather than fighting the rebind; see
  claude-wire.md §E6.
- **Turn lifecycle** — `result` envelope remains authoritative for
  the turn's accounting payload (token usage + cost, emitted as
  per-turn deltas by `usage_accounting.go` because the wire values
  are session-cumulative; raw `terminal_reason` stays wire-reference
  only). The final
  `assistant_message_id` is tracked from the last in-stream assistant
  `message.id`; it is NOT carried on `result`. `result` is also NOT
  the only source of `EventTurnComplete`: when the parent message ends with a
  "model has stopped" stop_reason (`end_turn` / `stop_sequence` /
  `refusal`) and `parent_tool_use_id` is null, `parse_stream.go`
  emits a typed `provider.SoftRoundCloseMeta` `EventTurnComplete`
  immediately so the working indicator clears even when the CLI
  withholds `result` (it does this whenever a `local_agent` subagent
  is still in flight). The trailing `result` envelope, when it
  eventually arrives, folds in the accounting payload via
  `persistLateTurnPayload` — see
  [`invariants.md §27`](../../../docs/architecture/invariants.md#27-soft-round-close-from-message_deltastop_reason-is-wire-typed)
  and the
  [`local_agent_outlives.ndjson`](../../../docs/references/fixtures/claude/local_agent_outlives.ndjson)
  fixture. Claude 2.1.154+ also emits this soft-close at *intermediate*
  message boundaries (one logical turn split into multiple wire
  messages, resumed with a fresh parent `message_start` and no
  intervening `result`/`system.init`); the parser still just emits the
  soft `EventTurnComplete` per segment — triage owns re-lighting the
  working indicator on the parent resume (`maybeReopenSettledRound`,
  invariant 27 "Parent-content resume re-arm"). No parser change is
  needed for that case.

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
- `docs/references/fixtures/claude/local_agent_async_launch.ndjson` —
  `local_agent` (Agent tool) launched with NO `run_in_background`,
  run asynchronously anyway: the bare "Async agent launched
  successfully." ack (claude-wire.md §E5), then `system/task_updated`
  + `system/task_notification`. Authoritative for the
  `toolResultAsyncLaunch` discriminator (`isAsync`/
  `status:"async_launched"`, never mere `agentId` presence — an
  inline agent's real completion also carries `agentId`) and the
  `rememberTaskToolUse` re-seed on reconnect.
- `docs/references/fixtures/claude/local_agent_async_resume.ndjson` —
  an E5 async agent resumed via the harness's SendMessage tool
  (claude-wire.md §E6): the CLI rebinds `system/task_started` onto
  SendMessage's own `tool_use_id` carrying the ORIGINAL agent's
  `description`, and the SendMessage ack has no async markers at all.
  Two full rounds back to back. Authoritative for the resume-rebind
  detection in `parse_system.go`'s `task_started` case and the
  `isAgentLaunchToolName` reconnect fallback.
- `docs/references/fixtures/claude/advisor_context_usage_20260522.summary.json`
  — sanitized summary across three captures (no advisor, one advisor,
  two advisors). Documents the `message_delta.usage.iterations[]` shape
  and confirms top-level is the cumulative sum across parent
  iterations. Wire shape only — read with the
  `advisor_pretokens_correlation_20260523` fixture (below) which
  supplies the ground-truth anchor.
- `docs/references/fixtures/claude/advisor_pretokens_correlation_20260523.summary.json`
  — five production compactions correlating the trailing
  `message_delta.usage` top-level against the following
  `system.compact_boundary` `compactMetadata.preTokens`. Top-level
  matches preTokens within 1-2% across both no-advisor and advisor
  turns; iter[-1] is ~50% off on advisor turns. Authoritative for the
  current `parse_stream.go` direct top-level read and the
  `TestParseStreamEventMessageDeltaUsesTopLevel*` regression set.
- `docs/references/fixtures/claude/resume_no_assistant_replay_20260624.summary.json`
  — sanitized two-turn spike (claude 2.1.170): a `--resume`d process does
  NOT re-emit prior turns' assistant content on stdout; it streams only
  the new turn (identical envelope counts turn-1 vs turn-2, zero ALPHA in
  turn-2). Backs the snapshot-recovery safety argument for the
  `streamedMessageIDs` discriminator (`parser.go`) and never-streamed
  recovery (`parse_assistant.go`): a bare `assistant` snapshot is always
  an in-turn CLI retry, never replayed history. Documentation summary, not
  a replayed wire log — the Go regression guard is
  `TestAssistantEnvelopeDoesNotDuplicateStreamedText`. See
  `claude-wire.md §"Resume does not re-emit assistant content"`.
- `docs/references/fixtures/claude/multiturn_cost_cumulative_20260703.ndjson`
  — three trivial turns in one stream-json session. Proves
  `result.total_cost_usd` + `result.modelUsage` are SESSION-CUMULATIVE
  while flat `result.usage` is per-turn. Authoritative for the
  snapshot-delta accounting in `usage_accounting.go`
  (`TestParseResult_ModelUsageCumulativeToDelta`).
- `docs/references/fixtures/claude/subagent_usage_inclusion_20260703.ndjson`
  — one turn launching a Task agent. Proves flat `result.usage` is
  PARENT-ONLY while `modelUsage` includes sidechain tokens + per-model
  `costUSD` (`TestParseResult_ModelUsagePreferredOverFlatUsage`).
- `docs/references/fixtures/claude/initialize_models_20260802.json` —
  the `initialize` control_response of a real 2.1.219 probe, trimmed to
  `models` + `account` + the fast-mode keys (identity anonymised).
  Authoritative for `models_wire.go` and for
  `internal/claudemodels`'s merge policy: the alias/`[1m]`
  inconsistency, the two rows that resolve to one model, and Haiku's
  missing effort support all come from here.
- `docs/references/fixtures/claude/local_command_20260803.ndjson` — the
  full envelope sequence of a provider-executed local slash command on
  2.1.219 (`/usage`), hand-written from a zero-token live probe:
  `command_lifecycle{queued,started}` → `system/init` (with
  `slash_commands` / `skills` / `plugins`) → the synthetic `assistant`
  (`message.model: "<synthetic>"`, `stop_reason: "stop_sequence"`, zero
  usage) → the `user{isReplay:true}` `<command-name>` metadata echo →
  `result` (which repeats the output verbatim in `result.result`) →
  `command_lifecycle{completed}` → a `system/commands_changed` push.
  Pins the two regressions worth pinning: exactly ONE persisted row for
  the whole sequence, and the metadata echo staying off the timeline.
- `docs/references/fixtures/claude/effort_live_20260812.ndjson` — three
  live-config command sequences on 2.1.219 (2026-08-12 spike, sanitized):
  `/effort low` success, `/effort bogus` rejected as `is_error:false`
  text ("Invalid argument: …"), and `/fast on` declined immediately with
  "Fast mode unavailable: Fast mode is not available in the Agent SDK"
  (`fast_mode_disabled_reason: "sdk_opt_in_required"`). Replayed by
  `TestEffortLiveFixtureCorrelatesCommandResults` (command-window
  correlation end to end) and `TestAdvertisedCommandsFromWireInit`.
  Authoritative for the /effort reply texts and the fast-mode FAILURE
  replies; the fast-mode SUCCESS texts ("Fast mode ON"/"OFF") are NOT in
  this capture — the spike account had no fast access — and come from
  2.1.219 binary strings (claude-wire.md §"Live config commands").
- `docs/references/fixtures/claude/context_usage_control_20260803.summary.json`
  — sanitized capture of a `get_context_usage` control_response on
  2.1.219: the full key set, the `categories[]` rows, and the
  arithmetic the UI leans on (deferred rows excluded from
  `totalTokens`; non-deferred rows sum to `rawMaxTokens`). Also records
  the drift from the 2.1.88 SDK schema. Go regression guards are the
  `TestParseContextUsage_*` set in `context_usage_test.go`.
- `docs/references/fixtures/claude/monitor_wakeup_20260728.summary.json`
  — sanitized shape summary for the Monitor watch-task launch ack
  (§E7: `{taskId, timeoutMs, persistent}` — background `local_bash`
  launch, both persistent variants, the string-valued error ack, and
  the TaskCreate/TaskUpdate `taskId` collision guard) and the
  ScheduleWakeup ack pair (§E8: `scheduledFor` epoch-ms schedule +
  `stopped:true` stop). Documentation summary from a live AO session
  transcript; Go regression guards are the four
  `TestAppendToolResultBlock_{MonitorLaunch…,TaskListAck…,
  ScheduleWakeup…,StringToolUseResult…}` tests in `parse_user_test.go`.

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
