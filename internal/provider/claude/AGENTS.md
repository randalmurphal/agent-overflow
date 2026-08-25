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

`--forward-subagent-text` is passed unconditionally. Without it, an agent
launched with an explicit `run_in_background: false` — an INLINE, awaited
agent — has its prose, thinking and final answer dropped by the
synchronous Task path before they reach the parent stream, so its card
and pane show tool rows and nothing else. The flag only relaxes that
filter, and the CLI has carried it since 2.1.211. See claude-wire.md
§"Subagent stream forwarding".

Structured output is session-sticky: `Config.OutputSchema` is passed as
inline JSON through `--json-schema` when the process starts. Per-turn
`provider.SendOptions.OutputSchema` is deliberately ignored by Claude.

`Config.SystemPrompt` goes out as `--system-prompt-file <path>`, never as
an argv value: the two flags are wire-identical (2.1.234), and the file
avoids both `MAX_ARG_STRLEN` (a long rendered prompt would make every
spawn fail E2BIG) and `/proc/<pid>/cmdline` exposure. `NewSession` writes
the 0600 temp file (`WriteSystemPromptFile`); `Close` and every
failed-spawn path remove it. `buildArgs` therefore takes the PATH — it
never reads `cfg.SystemPrompt` itself.

`WriteSystemPromptFile` / `RemoveSystemPromptFile` and
`SanitizeDisallowedTools` are exported for `internal/provider/claudetui`,
which passes the same two flags on its PTY launch (the interactive TUI
honors both identically — spike-verified 2.1.234). One writer and one
argv-safety pass mean the two Claude transports cannot drift on the temp
file's mode/removal contract or on what counts as one safe CLI argument.
`mergeDisallowedTools` stays unexported: the read-only mode strip it
unions in is headless-only, because `EnforcesRuntimeMode` is false on
claude-tui.

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
  the model fallback family, api_error,
  task_started / task_progress / task_updated / task_notification,
  `background_tasks_changed`,
  `commands_changed`, `status`, `permission_denied`, `permission_retry`). `commands_changed` is an undocumented push
  whose contract is REPLACE-the-cached-list, so it emits
  `EventCommandsChanged` with the whole list; an empty `commands` array
  is a real replacement while an ABSENT key is dropped. `system/init`
  additionally carries the `slash_commands` / `skills` / `plugins`
  discovery arrays onto `SessionInfo` — `slash_commands` is the only
  surface listing MCP prompt commands (`mcp__server__prompt`) — plus
  `output_style` (the CLI's echo of the style this session actually
  launched with; the literal `"default"` is a real value, and the echo
  is the only statement of a style a settings FILE contributed rather
  than AO's `--settings` block), `mcp_server_errors` (2.1.237) and
  `capabilities` (see §Capabilities below).
  `system/status` maps `status:"compacting"` and the `compact_result`
  close onto `EventCompactionStatus`; the per-API-request
  `status:"requesting"` noise is dropped. See claude-wire.md
  §`system/status`.
- `parse_assistant.go` — `assistant` envelopes (text deltas, tool_use
  blocks, thinking blocks, usage). Dispatches each content block to
  `appendRecoveredBlockEvent` / `appendToolUseEvent` /
  `appendExitPlanModeEvent` / `appendAssistantUsageEvent`.
- `parse_user.go` — `user` envelopes carrying `tool_result` blocks,
  split into `appendTaskOutputCompletion` (Task-tool background path)
  and `appendToolResultCompletion` (standard inline path). Plus the one
  prose case this layer claims: a SCOPED envelope with no tool_result in
  it is the subagent's own conversation — the task prompt the CLI handed
  the agent — and becomes an `EventUserText` under the launch
  (`subagentPromptEvents`). Unparented prose belongs to the replay echo
  and is dropped here. See claude-wire.md §"Subagent stream forwarding".
- `parse_transcript_mirror.go` — `transcript_mirror` envelopes enabled by
  the always-on `--session-mirror` launch flag. It projects the two gaps in
  ordinary stdout: a direct slash command's attributed Skill fork and the
  tail of a foreground agent after `background_tasks` detaches it. The
  shared session-import projector preserves message ordinals and compaction
  pairing across batches. Classification shallow-decodes each entry; only a
  claimed mirror gets the one full Row decode used by projection and nested
  task binding. Unattributed prefixes are bounded by file count, entry count,
  per-file bytes, and total bytes. Crossing a bound emits one visible
  degradation warning for the command. A fork's outer synthetic answer is
  held until terminal and emitted once as a top-level sourced command result.
  Projection, task, owner, and dedupe state is
  released at terminal. Ordinary async sidechains keep using stdout and their
  mirror copies are ignored.
- `parse_control.go` — `control_request` envelopes: CanUseTool
  approvals and the exit_plan_mode signal. `parseControlRequest` is a
  `*Parser` METHOD because a subagent's ask arrives here as
  `request.agent_id` (its task id) with no `parent_tool_use_id`
  anywhere on the envelope — the parser's task_id ↔ tool_use_id map is
  the only state that can turn that into the launch scope that nests
  the approval under the agent's card. The resolved launch lands on
  both the `ApprovalRequest` / `UserInputRequest` payload
  (`ParentToolUseID`) and the `ProviderEvent`; an unresolvable
  `agent_id` leaves it EMPTY (triage owns the row-lookup fallback)
  rather than guessing a scope. See claude-wire.md §"Subagent
  approvals carry `agent_id`".
- `parse_command_lifecycle.go` — `command_lifecycle` envelopes: the
  CLI's delivery ack for a user message we wrote to stdin, keyed by
  the client-minted `command_uuid`. Send-side correlation lives in
  triage (the pending-send registry those ids belong to); this parser
  additionally tracks the `started`→`completed`/`cancelled` window as
  `Parser.activeCommandUUID` so `parse_assistant.go` can stamp a
  provider-executed command's output (`EventCommandResult`) with the
  command uuid it answers (`provider.CommandResultMeta`) — the
  confirmation channel for `/effort` / `/fast` live applies. A user-issued
  native slash command also gets a running Command row at `started`; a
  mirrored `attributionSkill` update changes that same row to Skill. Internal
  `/effort`, `/fast`, and `/rename` commands remain row-suppressed. Windows can
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
- `get_settings.go` — the `get_settings` outbound control_request and the
  `SettingsSnapshot` it answers with (`Session.GetSettings` /
  `ParseSettingsSnapshot`). Its `applied` object is the CLI's own
  STRUCTURED statement of the model/effort/advisor/ultracode a turn will
  actually run with, which is why the live-config settle path prefers it
  to matching the English reply text of `/effort` and `/fast`. Read on
  demand — once per apply that needs confirming, never on a timer — and
  never persisted. An older CLI answers "unsupported control request
  subtype"; that latches on the session (`GetSettingsUnsupported`) so the
  settle path falls back to the reply text instead of re-asking every
  time. Fast mode is deliberately NOT read from here: `applied` does not
  carry it. `ProjectOverrides` is the second consumer — the `sources`
  array is how a project-scoped `.claude/settings.json` that silently
  overrides AO's model or effort becomes a user-visible notice.
- `approvals.go` — approval-response encoding for the SDK.
- `live_update.go` — `PlanLiveUpdate` / `ApplyLiveUpdate`: the
  restart-free path for config changes a live session can adopt. Model
  and permission mode ride control_requests (`set_model`,
  `set_permission_mode`); effort and fast mode ride the CLI's own
  `/effort` / `/fast` slash commands (uuid-stamped native command sends),
  whose confirmation arrives later as
  an `EventCommandResult` carrying the uuid — the app side
  (`app_claude_live_config.go`) settles those. Context-window changes
  ride the `[1m]` marker on `set_model`. Extended thinking rides its own
  `set_max_thinking_tokens` control_request (`LiveUpdate.Thinking`):
  `max_thinking_tokens: 0` disables thinking, a positive value sets an
  explicit budget (which only binds on models that TAKE one — on adaptive
  models only `thinking_display` applies), and `thinking_display` alone is
  a legal request. `max_thinking_tokens: null` is accepted and does
  NOTHING, so returning to the CLI's own choice has no wire form at all
  and `PlanLiveUpdate` reports that one direction as a restart. Version
  floor 2.1.214, same posture as the system-prompt axis — an unknown
  version counts as too old. `PlanLiveUpdate` returns false
  (restart) for everything else, and `ApplyLiveUpdate` validates every
  axis before ANY side effect — including refusing the command axes
  while the transcript needs the resume-at repair — so a restart-bound
  update never half-applies. Its `preSend` hook fires between
  validation and the first wire write: the caller registers its
  pending-confirmation state there, BEFORE the CLI can possibly
  answer. See claude-wire.md §"Live config commands".
- `session.go` — the `Session` struct itself (every field with the
  ownership comment that explains which lock covers it), `NewSession`,
  `Close`, and the small state accessors (`SessionID`, `PID`,
  `CanonicalLeafUUID`, `RequiresResumeAtBeforeUserSend`). Process
  lifecycle only; the five files below own what a live session does.
- `session_spawn.go` — how the process is launched: the `Config`
  struct, `buildArgs`, the child environment (`claudeSpawnEnv` /
  `claudeSpawnUnsetEnv` and the `CLAUDE_CODE_*` defaults
  `withClaudeSessionEnvDefaults` applies), and the
  `WriteSystemPromptFile` / `RemoveSystemPromptFile` pair
  `--system-prompt-file` rides on.
- `session_readloop.go` — the stdout pump. `readLoop` feeds
  `ParseLine`, routes the three prefix-gated control envelopes to
  their handlers, and folds `system/init` facts onto the session:
  `noteCLIVersion` / `CLIVersion` and `replaceAdvertisedCommands` /
  `supportsSlashCommand`, whose only writer is this loop.
- `session_control.go` — the outbound control_request channel.
  `sendControlRequest` owns the allocate/register/marshal/write/
  await-response state machine, `interpretControlResponse` the
  standard reply reading, and `handleControlResponseLine` the
  read-loop side that matches a reply back to its waiter (and flags
  the parser on an interrupt ack). The permission-mode axis lives
  here too — `setPermissionMode`, `SetInteractionMode`,
  `normalizeClaudePermissionMode` — because it is one more subtype on
  the same channel. `set_model` / `set_max_thinking_tokens`
  (`live_update.go`), `get_settings`, `get_context_usage` and the MCP
  subtypes (`mcp.go`) all ride the same helper from their own files.
- `session_send.go` — what AO writes onto stdin to drive a turn:
  `Send` (the user-message envelope, its canonical-uuid check, and the
  issued-command-uuid note that keeps our own send from reading as a
  peer's), `Interrupt`, `StopTask`, `BackgroundTask` (the Ctrl+B
  equivalent — see the control_request list below), and the
  replay-parent expectations
  `Send` records so `--replay-user-messages`' echo can be checked
  against the leaf the message was sent from (`verifyReplayParent`).
- `session_approvals.go` — the inbound control_request side and the
  approval state it drives: the `can_use_tool` handlers (full-access
  auto-allow, `ExitPlanMode` plan capture), `control_cancel_request`
  cleanup, `RespondToApproval` / `RespondToUserInput`, the
  pending/dedup registry and its close-time drain, and the
  `AskUserQuestion` answer projection.
- `session_peer.go` / `peername.go` — cross-session messaging: the
  issued-command-uuid ledger that tells a peer-started turn from one of
  ours, `RenamePeerSession`, and the mirror of the CLI's own `--name`
  normalizer. See §Cross-session messaging.
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
  /x" with `num_turns: 0`). Native routing is the default for every direct
  command-shaped send, independent of asynchronous command discovery. AO
  prefixes a single `"\n"` only when the caller sets
  `SendOptions.GuardClaudeSlashCommand`, which is reserved for an
  AO-expanded composer command whose rendered prompt still starts with a
  slash. A word with an interior slash (`/etc/hosts …`) is not
  command-shaped and passes through untouched. See claude-wire.md §"Slash
  commands (provider-executed)".
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
  **Sidechain rows never advance the leaf, and the two feeds spell
  "sidechain" differently.** A transcript row on disk carries
  `isSidechain`; the live stdout wire carries top-level
  `parent_tool_use_id` and never `isSidechain`, on `user` envelopes
  (a subagent's tool_results) exactly as on `assistant` ones. BOTH
  ingest paths therefore gate on `parent_tool_use_id`. Without the
  `user` gate the tracker's leaf became a sidechain uuid for the whole
  duration of a Task, and every consumer resolves that leaf against the
  FILE — where the fork transform and the branch walk have already
  filtered sidechains out — so the lookup could only miss.
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
  binary probe). `inlineSettingsForCLI` builds the `--settings` JSON
  block, which is the ONLY delivery route for a setting the CLI reads
  from its settings file rather than a flag. The block's precedence is
  the reason it is used at all: the CLI resolves
  `policySettings > flagSettings > userSettings` and REAPPLIES its own
  settings `env` block over the inherited environment at init, so an
  env-backed axis handed to the subprocess as a plain variable is
  silently discarded. `--settings` currently carries `fastMode`,
  `outputStyle`, `crossSessionInbound`, and an `env` map holding the
  auto-compact pair plus `CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH` /
  `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS` /
  `CLAUDE_CODE_TOOL_MEMORY_LIMIT`. Every one of those names is in
  `provider.ReservedEnvNames`, precisely so a user's custom environment
  cannot set a value the CLI would then ignore.
  The flag itself is emitted only when the block would be non-empty —
  which, since `crossSessionInbound` states its refusal even when the
  feature is off (§Cross-session messaging), is now every Claude spawn.
  All of these axes are SPAWN-TIME ONLY and therefore deliberately NOT
  on `provider.SessionOptions`: `PlanLiveUpdate` diffs
  `ConfigFromOptions(prev)` against `ConfigFromOptions(next)`, so a
  field the options struct cannot carry is structurally "next sessions
  only" — no live-update branch and no reconcile pin is needed for it.
  The app stamps them in `spawnProviderSession` from
  `settings.ClaudeSessionAxesForProvider`.
- `rotation.go` — the rule that keeps the account probe from destroying
  the login it is probing. The CLI fires its OAuth refresh at startup as
  a DETACHED task and answers `initialize` from cached local state
  without awaiting it, so the probe's answer says nothing about whether
  the rotation reached disk — and under `--max-turns 0` the CLI exits on
  stdin EOF, which is what `Close` does first. Anthropic retires the old
  refresh token the moment the request is processed, so the gap is a
  permanently dead login, not a retry. `ProbeConfig.ReadCredential` arms
  a watch when the credential is at or inside the CLI's five-minute
  proactive-refresh buffer; teardown then holds until the credential
  actually changes. Measurements, the failure rates, and why a fixed
  delay is not a fix are on `rotationWatch`.
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
  `system/init.mcp_server_errors` (2.1.237) is the second half of that
  projection and lands on `SessionInfo.MCPServerErrors`: servers whose
  config entry the CLI REFUSED are absent from `mcp_servers[]`
  entirely, so without this array a rejected server is
  indistinguishable from one that was never configured. The two arrays
  never name the same server, which is why `ingestClaudeInitMCPStatus`
  can loop both without a collision, and every Put it makes from the
  error array carries a non-empty `Error` — the mcpstatus merge matrix
  stores those verbatim and only ever carries a prior explanation
  forward onto an error-LESS ephemeral fetch, so nothing has to be
  forced.
  Version floor worth knowing when reading old reports: before 2.1.221
  a `--mcp-config` server was not connected before the first
  print-mode turn, so a session's first turn ran with none of them
  available and `system/init` said so. AO does not gate on this — the
  supported-version floor is above it — but a stale bug report
  describing "MCP servers missing on the first message only" is that,
  not this projection.
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
  `task_progress`, `task_updated`, `task_notification`,
  `background_tasks_changed`,
  `session_state_changed`, `api_retry`, `api_error`,
  `permission_denied`, `permission_retry`, and the model
  fallback family — `model_refusal_fallback`, `model_fallback`,
  `model_consent_fallback`, `model_refusal_no_fallback`.
  `tool_progress` is intentionally dropped.
- `system.api_error` is the wire TWIN of `api_retry`: the CLI emits both
  for one retryable failure. It is normalized onto the SAME
  `EventAPIRetry`, because triage upserts the per-turn id
  `retry:<turnIndex>` and a second surface would double-render one
  failure. The twin is the richer half — it nests `error.formatted`
  (the CLI's own display string, preferred over `error.message`) and
  `error.status`, and spells its counters `retryAttempt` / `maxRetries`
  / `retryInMs` where `api_retry` spells them `attempt` / `max_retries`
  / `retry_after_ms`. Both spellings are read.
- The model fallback family is three notices and one error. The three
  notices (`model_refusal_fallback` — the API refused this request;
  `model_fallback` — the model was unavailable or blocked;
  `model_consent_fallback` — a credits/consent choice moved the session)
  all mean "the turn continues on another model", so all three emit
  `EventModelFallback` and update the parser's current model;
  `meta.kind` is what tells them apart, and the consent variant adds
  `choice` + `persistedAsDefault` (was this written back as the account
  default, or only for this session). `model_refusal_no_fallback` is the
  one member whose turn produces NOTHING — refused with no fallback
  route — so it is `EventError`, per core principle 5. Its meta
  deliberately carries no top-level `fatal` bool and no top-level
  `error` string: those are how triage recognises a dead process and an
  SDK error enum respectively, and only the TURN died here.
- Field spellings in this family are read case-insensitively across
  snake_case and camelCase (`readRawStringAny`). This is not defensive
  padding: the wire emits snake_case (`fallback_model`,
  `api_refusal_category`, verified in the 2.1.214 / 2.1.219 / 2.1.237
  serializers) while the CLI's internal object is camelCase, and reading
  only the latter made every REAL envelope fail with "empty
  fallback_model".
- `system.permission_denied` / `system.permission_retry` — the
  permission family, both `EventNotification` with a `meta.kind`
  discriminator (`permission_denied` / `permission_retry`), following
  the model-fallback family's shape rather than inventing an event
  kind. **Live-wire only**: 2.1.237's two persistence paths BOTH drop
  these envelopes (`{type:"ignored"}` / a bare `continue`), so neither
  appears in a transcript — a resumed, imported or forked thread has no
  record of them, and nothing may be inferred from their absence.
  - `permission_denied` is emitted where the CLI auto-denies a tool
    call BEFORE it could ask (the pre-ask gate), which is exactly the
    case where the timeline would otherwise show a tool that silently
    never ran. Fields: `tool_name`, `tool_use_id`, `decision_reason`
    (the deciding component's own sentence, preferred for display —
    the CLI's own debug renderer prefers it), `message`, `agent_id`,
    and `decision_reason_type`, the discriminator of the CLI's
    `PermissionDecisionReason` union: `rule`, `mode`, `workingDir`,
    `permissionPromptTool`, `subcommandResults`, `hook`, `other`,
    `sandboxOverride`, `safetyCheck`, `asyncAgent`. Triage attaches
    the notice to the tool call — the notice's row id is namespaced
    (`permission-denied:<tool_use_id>`) so it cannot collide with the
    `tool_call` row, whose id IS the bare tool_use_id — and annotates
    that row with `Decision="declined"` plus a `permissionDenied` meta
    block. It deliberately does NOT touch the row's Status: a row that
    has left `statusRunning` makes `persistToolCallCompletion` drop the
    real completion, so a denial must never pre-settle the row.
  - `decision_reason_type:"workingDir"` is the ONLY workspace-boundary
    signal a denial carries, and the distinction is load-bearing in the
    copy: the CLI answers a boundary refusal with `addDirectories`
    suggestions, never a `Bash(...)`-style tool rule, so telling the
    user to add a permission rule would be advice that fixes nothing.
    (`blocked_path` itself rides `control_request/can_use_tool`, not
    this envelope.)
  - `permission_retry` carries NO `tool_use_id` and no attempt count —
    its only 2.1.237 producer is the interactive REPL's
    `onRetryDenials` dialog, which reports by command NAME after a
    permission-mode change. Parsed defensively as a plain timeline
    notice with an optional bounded `commands` list; do not build a
    tool correlation on it.

- `system.task_started` — meta-only `EventToolStart` emission that
  records the `task_id ↔ tool_use_id` mapping into `items.meta`.
  Fires for EVERY Bash/Task — not just backgrounded ones.
- `system.task_progress` — the per-round live tick for a running
  `local_agent` (never a forked skill). Emits `EventSubagentProgress`
  keyed by the LAUNCH tool_use, with the launch's own parent as
  `ParentToolUseID` so a depth-2 agent attributes without a store
  lookup. An envelope carrying both ids RE-SEEDS the task binding (same
  reconnect insurance `task_started` and the §E5 ack provide); a tick
  whose launch cannot be resolved is dropped with a log line rather
  than emitted with an empty ItemID. Live UI state only — triage holds
  the latest per launch in memory and persists the final numbers at the
  launch's terminal.
- `system.task_updated` (terminal `patch.status` in
  `{completed, failed, killed}`) — emits
  `EventBackgroundTaskTerminal` keyed by task_id + resolved
  tool_use_id.
- `system.task_updated` with a NON-terminal patch carrying
  `is_backgrounded: true` — the reply to Ctrl+B / AO's
  `background_tasks` control_request. Emits `EventSubagentBackgrounded`
  on the launch row: the only typed statement that a FOREGROUND agent's
  ordinary sidechain forwarding stopped here. The transcript mirror keeps
  later rows live under the same launch. It deliberately does not clear the
  liveness flag — signal (5) must stay armed for the §E5 ack that follows.
  Every other non-terminal patch stays a no-op.
- `system.background_tasks_changed` — the LEVEL set of live background
  tasks, REPLACE semantics. Emits `EventBackgroundTasksChanged` with
  each member's launch tool_use resolved through the task map when
  known. `tasks: []` is a real empty set and is forwarded; an ABSENT or
  unreadable `tasks` key is dropped (unreadable is not evidence that
  nothing is running). Foreground agents and forked skills are never in
  the set.
- `system.task_notification` — **NOT a completion source**. Emits
  `EventBackgroundTaskNotification` so triage can persist a distinct
  notification row and optionally ingest `output_file` into SQLite. A
  `local_agent` completion additionally carries
  `usage{total_tokens, tool_uses, duration_ms}` — the run's
  AUTHORITATIVE final counters — which ride the event meta under the
  key `usage` as a `provider.SubagentProgressMeta` object so triage can
  fold them onto the launch row without depending on a live tick having
  survived. A usage-less notification (every `local_bash` bookend)
  stamps nothing, so a zeroed object can never overwrite real numbers.
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
- `<cross-session-message from="...">` — the OTHER injected-user
  wrapper, and the one exception to "an injected replay envelope is
  provider context, drop it". Claude's cross-session inbox (2.1.224+)
  delivers a peer session's `SendMessage` as a user-role turn wrapped in
  this tag, so the body is a MESSAGE someone sent into this thread, not
  context the CLI pasted in. `parse_user_replay.go` therefore checks
  `ExtractCrossSessionMessage` (the peer sibling of
  `ExtractAllTaskNotificationFields`) BEFORE the injected-content drop:
  the row survives with the wrapper stripped, `meta.cross_session_message
  = true`, and `meta.cross_session_from` carrying the sender, which is
  what a peer-message row renders from. `sessionfork`'s
  `InjectedUserContentWrappers` still lists the tag, for the different
  question that list answers — a fork must not land its cursor ON an
  injected row.
  Whether such a message is delivered at all is the user's
  `claudeCrossSession` setting — see §Cross-session messaging below.
- `assistant` — text deltas, tool_use, thinking, exit_plan_mode,
  usage. Subagent messages identified by top-level
  `parent_tool_use_id`.
- `user` — `tool_result` blocks. Six variants: standard inline,
  backgrounded placeholder, TaskOutput, async `local_agent` launch ack
  (claude-wire.md §E5 — bare "Async agent launched successfully.",
  `isAsync`/`status:"async_launched"`, no `run_in_background` at
  launch and no `backgroundTaskId` on the wire; discriminated from an
  ordinary inline agent completion, which also carries `agentId` but
  never `isAsync`/`async_launched`. On a SIDECHAIN line — a subagent
  launching its OWN async agent, `parent_tool_use_id` set — the CLI
  omits `tool_use_result` ENTIRELY, so all four signals miss and the
  ack TEXT is the only evidence; see §E5b and `asyncLaunchAckAgentID`,
  whose three conjunctive gates are what keep the text test from
  firing on anything else), Monitor watch-task launch ack
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
  A seventh variant carries no lifecycle of its own: the FORKED SKILL
  completion (§E9 — `tool_use_result.{success, commandName,
  status:"forked", agentId, result}`). It is the only wire statement
  that a `Skill` row was an agent, so `toolResultSkillFork` stamps
  `meta.skillFork = {agentId, commandName}` onto the ordinary
  `EventToolComplete` and binds `agentId → the Skill tool_use` so a
  later `can_use_tool.agent_id` from inside the fork resolves to that
  row. `status:"forked"` is the entire discriminator — an INLINE skill
  answers `{success, commandName}` with no status and is left
  unstamped, as is a `forked` result with no `agentId` (the id is what
  the stamp is for). Never marked `is_background`: the main turn was
  blocked for the whole fork.
- `stream_event` — streaming deltas (requires
  `include_partial_messages: true`).
- `result` — **turn-complete signal**. Emits `EventTurnComplete`.
- `control_request`: inbound from the CLI carries `CanUseTool` and
  `exit_plan_mode`. Outbound from us carries `interrupt` (abort the
  current turn — `Session.Interrupt`), `stop_task` (kill a
  backgrounded Bash / Task subagent by `task_id` —
  `Session.StopTask`), `background_tasks` (the OPPOSITE direction:
  detach an in-flight FOREGROUND Bash / subagent from the turn instead
  of killing it, keyed by `tool_use_id` — `Session.BackgroundTask`,
  bound as `App.BackgroundClaudeTask`. The reply's
  `response.backgrounded` is verified rather than trusted: the CLI
  answers `subtype:"success"` for a well-formed request that matched no
  live foreground task, and reporting that as done would flip a
  still-streaming row in the UI. AO never sends the id-less
  "background everything" form),
  `set_permission_mode`, `set_model` (live model
  switch, `live_update.go` — acked mid-turn, applies from the next
  turn; verified 2.1.205), the five MCP control
  subtypes AO wraps (`mcp_toggle` / `mcp_reconnect` /
  `mcp_authenticate` / `mcp_oauth_callback_url` / `mcp_status`, all in
  `mcp.go`),
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
  messages (`queued` / `started` / `completed` / `cancelled` /
  `discarded`). `discarded` (2.1.224+) means the session ended with the
  message still queued; the parser closes the started-window on it
  exactly as it does for `cancelled`, and it stays a distinct state only
  so the cause reaches the timeline. Live UI state, never history; older
  CLIs emit none, so nothing may depend on its presence.

`parent_tool_use_id` on tool events correlates subagent (`Task`) work
back to the parent tool call.

## Capabilities (`system/init.capabilities`)

`system/init` carries a `capabilities` array of opaque feature tokens.
`parse_system.go` normalizes it onto `provider.SessionInfo.Capabilities`
(trimmed, empties dropped, first-occurrence-wins dedupe preserving wire
order, capped at 64 tokens × 64 runes) and `session_capabilities.go`
holds it on the session's live state behind `HasCapability(name)` /
`Capabilities()`. Nothing is keyed on it yet.

Known tokens, from the CLI's own schema text:

| Token | Meaning |
|---|---|
| `interrupt_receipt_v1` | interrupt control requests are acknowledged with a receipt |
| `interrupt_cancel_queued_v1` | an interrupt also cancels queued messages |
| `msg_lifecycle_v1` | `command_lifecycle` acks for queued/started/completed messages |
| `queued_notifications` | schema-named only; no emit site observed in 2.1.237 |

Rules that come with it:

- **Prefer a capability check to CLI version parsing when a token
  exists for the behaviour.** A token is the CLI's own statement about
  the build that is actually running; a version string has to be
  matched against a changelog and is wrong for forks, canaries and
  vendored builds.
- **Absence is not a denial.** On 2.1.237 the stream-json engine
  advertises a two-element constant (`interrupt_receipt_v1`,
  `msg_lifecycle_v1`); the three-element list including
  `interrupt_cancel_queued_v1` is declared in the bundle and never
  referenced, and `queued_notifications` appears only in schema text. A
  gate must therefore LIGHT UP a newer path when a token is present and
  never REFUSE an older path when it is missing — otherwise a build that
  simply under-reports loses working behaviour.
- **`system/init` is re-emitted before EVERY turn**, so init handling
  must be idempotent and must not re-fire one-shot work.
  `noteCapabilities` replaces the set under the live-state lock and logs
  once per session (`capabilitiesLogged`); an empty array returns early
  rather than clearing, since a build that says nothing is not a build
  that revoked everything. Anything new hung off `EventInit` needs the
  same treatment — regression:
  `TestParseSystem_RepeatedInitIsIdempotent`.

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

## Cross-session messaging

Claude Code 2.1.224+ gives sessions on one host a peer inbox: they find
each other with `ListAgents` and address each other with `SendMessage`,
and a delivered message becomes a USER-ROLE TURN in the receiver that
nobody at the keyboard asked for. Full wire detail is
[`claude-wire.md §Cross-session messaging`](../../../docs/references/claude-wire.md#cross-session-messaging-harbor-kite-21224--verified-21237);
what lives here is the part that shapes this package.

**Four spawn pieces, three files.** `internal/settings`'s
`claudeCrossSession` (enabled + inbound policy) reaches
`ConfigFromOptions` through `provider.SessionOptions`. `options.go`
turns it into the `CLAUDE_CODE_HARBOR_KITE=1` environment gate
(`withClaudeCrossSessionEnv`), the `--name` flag (`peerSessionNameArgs`),
and the `crossSessionInbound` key in the `--settings` block. The
peer-visible NAME rides `Config.PeerSessionName` instead, stamped by
the app layer.

That split is deliberate and load-bearing. `PlanLiveUpdate` blanks the
live-appliable axes and then `DeepEqual`s the two Configs, so anything
reaching `Config` THROUGH `ConfigFromOptions` and left un-blanked becomes
a deferred restart automatically, while anything stamped OUTSIDE it is
structurally invisible to that diff:

- The inbox binds once during the CLI's setup and nothing rebinds it, so
  it rides `SessionOptions` — and a settings change converges by
  restart, which is what the user is told.
- The name is live-changeable for free, so it must NOT be able to queue
  one. It is stamped past `ConfigFromOptions` (`app_session.go`) and
  converges through `RenamePeerSession`.

**`hold` is never emitted, and neither is silence.** The CLI's schema has
three inbound values; AO offers two. A held message waits for an approval
a headless session cannot present, and both `hold` and an ABSENT key
(mode parity) discard it with nothing on stdout. `internal/settings`
refuses `hold` at the save and resolves an enabled-but-empty policy to
`accept`.

**Off states the refusal.** `claudeCrossSessionInbound` returns `refuse`
when the setting is off, so EVERY Claude spawn carries the key and the
`--settings` flag is now always present. The reason is that AO's gate is
not the only one: `tengu_harbor_kite` is a remote GrowthBook flag that
can bind the inbox for a user who never enabled it here, and
`tengu_harbor_kite_mode_emit` — which has NO environment override —
turns on the permission-class attestation that the unset-key default
reads. Absent key + both remote flags live = a class-matching peer
auto-delivers, starting a turn in a thread whose user never opted in.
Off has to mean off regardless of remote flags, so the refusal is stated
rather than assumed. The env gate cannot carry it: the CLI checks the
override for truthiness and falls through to the flag when it is unset,
so no environment value can express "off".

**Renaming is `/rename`, never `rename_session`.** The control request
moves the session TITLE and leaves the peer registry alone, so it would
report success while every peer kept addressing the old name.
`RenamePeerSession` sends `/rename <name>` as an ordinary stdin user
message with a client-minted uuid. Native command-shaped sends route to
Claude by default; only AO-expanded composer commands set
`GuardClaudeSlashCommand` and receive the newline guard. The uuid lets
triage's pending-send correlator consume the send instead of stranding it.
The command costs no model turn (`result.num_turns: 0`).

**A peer-started turn is identified by a uuid we never minted.**
`session_peer.go` holds the ledger; `parse_command_lifecycle.go` consults
it and stamps `Meta.origin = "peer-session"` on every frame of an
unaccounted bracket, releasing the entry on the terminal frame. The
classifier is fail-safe in one direction only: an unknown session or an
overflowed ledger reads as OURS. Mislabelling a peer turn costs a missing
label; mislabelling the user's own turn puts "from another Claude
session" on a message they typed.

**The peer's text is on the wire, not in the transcript.** AO always
spawns with `--replay-user-messages`, so the delivery also arrives as a
`user{isReplay,isSynthetic}` envelope whose top-level `origin` object
carries the peer's name and body and whose `uuid` equals the bracket's
`command_uuid`. `parse_user_replay.go` prefers that structured object and
falls back to the `<cross-session-message>` wrapper parse.

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

- `docs/references/fixtures/claude/task_progress_20260822.ndjson` — one
  async `local_agent` (2.1.237, 2026-08-22 spike, sanitized) running a
  40-second Bash, from launch to notification: `system/task_started`,
  two `system/task_progress` snapshots (LEVEL — cumulative
  `usage{total_tokens, tool_uses, duration_ms}` plus `description` /
  `last_tool_name`), the NESTED `local_bash` task the agent spawned,
  two `system/background_tasks_changed` pushes (one member, then the
  empty array), both terminals and both notifications — the agent's
  carrying the same `usage` block — and the inline `tool_result`.
  Authoritative for `parseTaskProgressEvent`, for the nested task
  carrying the AGENT's `parent_tool_use_id` rather than the parent
  turn's, and for `task_notification`'s `meta.usage`.
- `docs/references/fixtures/claude/background_tasks_control_20260822.ndjson`
  — the full `background_tasks` control round trip against a live async
  agent: our outbound `control_request{subtype:"background_tasks",
  tool_use_id}`, the `system/background_tasks_changed` listing the
  `local_agent`, the `system/task_updated{patch:{is_backgrounded:true}}`
  push, and the `control_response{backgrounded:true}` — in that order,
  i.e. the pushes land BEFORE the reply. Authoritative for
  `Session.BackgroundTask`'s reply verification and for
  `parseTaskBackgroundedPatch` (a patch with no `status`, which must not
  clear the task's liveness — the §E5 ack that follows still carries
  `is_background:true`).
- `docs/references/fixtures/claude/can_use_tool_agent_id_20260822.ndjson`
  — a backgrounded agent asking for a Write approval from inside the
  sidechain: `control_request{subtype:"can_use_tool", agent_id}` with NO
  `parent_tool_use_id` anywhere on it, our deny `control_response`, and
  the resulting error `tool_result`. Authoritative for the `agent_id` →
  launch-`tool_use` resolution in `parse_control.go` (the fixture's
  `agent_id` is the `task_started` id three lines above it).
- `docs/references/fixtures/claude/forked_skill_20260822.ndjson` — a
  `Skill` invocation that the CLI ran as a FORKED agent (§E9), captured
  from a real AO session's provider-event log: the `Skill` tool_use, two
  sidechain assistant/user pairs carrying the fork's own work, a
  `tool_progress` heartbeat, and the completion
  `tool_use_result{success, commandName, status:"forked", agentId}`.
  There is NO task lifecycle anywhere in it — no `task_started`, no
  `task_updated`, no `task_notification` — which is the whole reason the
  `skillFork` meta stamp exists. Long strings are truncated with
  `…[trimmed for fixture]`.

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
  Enforced: an envelope `type` no `ParseLine` case claims reaches
  `warnUnknownEnvelopeType` (`parser.go`), which logs it once per type per
  parser lifetime — the peer of codex's `warnUnclaimedNotification`. It is
  log-and-continue by design (a CLI release adding an envelope must not break
  a live session) and bounded at 64 distinct types, after which one overflow
  line is logged and further unknowns are silent.
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
