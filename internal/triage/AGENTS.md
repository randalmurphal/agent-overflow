# internal/triage/

Classifies provider events and decides what ships to the frontend vs
what writes to SQLite. The single most important rule is that triage
has **no derived state** — it is a pure function of the current event
plus a narrow, bounded set of per-thread correlation state.

## Layout

The package is split by concern so each file owns a narrow slice of the
routing pipeline. New routing logic belongs in whichever file most
closely matches its concern; create a new file (and list it here) if
none fits.

- `router.go` — entry point. `Router` struct, the `Handle` /
  `HandleSynthetic` pair (`Handle` drops every event for a stopped
  thread; `HandleSynthetic` is the host-event carve-out that bypasses
  that gate — see "Stopped-thread routing" below), the private
  `dispatch` switch, `persistItem` / `emitThreadUpdated` shared
  helpers, and the top-level error / session-status / token-usage /
  rate-limit routers.
- `session_status.go` — `EventSessionStatus` classifier:
  `classifySessionStatus` maps content + meta → `ProviderStatusEventKind`
  (rate-limit / unauthenticated / transient retry / ok), plus the
  `logUnknownSessionStatusOnce` capped log throttle that keeps novel
  status strings from polluting steady-state logs.
- `approvals.go` — approval-request lifecycle: pending-approval map,
  approval-resolved fan-out, decision → item projection.
- `user_inputs.go` — structured user-input request lifecycle and
  provider:user_input frontend event fan-out.
- `turn_lifecycle.go` — per-turn and per-thread correlation state
  (open turns, interrupt queue, stopped-thread markers, turn span
  bookkeeping, cleanup paths).
- `live_state.go` — refresh/reconnect snapshot of backend-owned live
  session state (active wire round, queue, interactive prompts,
  effective fallback model)
  copied under one router lock for the App transport DTO. The todo list is
  deliberately NOT here: it is durable thread state
  (`threads.live_todo`, migration v65) that `GetThreadLiveState` reads
  straight from the store.
- `timeline_notifications.go` — notification rows plus the todo write path.
  `projectTodoSnapshot` is the one funnel every producer of the activity-rail
  list goes through (`handleTodoUpdate`, `handleTaskCreate`,
  `handleTaskUpdate`): it persists to `threads.live_todo` BEFORE emitting
  `provider:todo_update`, so a refresh racing the push can never read older
  state than the frame it just saw. A failed persist still emits — the live
  pane is what the user is looking at — and returns the error. Empty steps
  clear the column and emit the empty-steps clear only when something was
  stored.
- `permission_notices.go` — Claude's permission-notice family
  (`permission_denied` / `permission_retry`), told apart by `meta.kind` the
  way the model-fallback family is. It forwards the notice's fields onto the
  persisted notification meta as a WHITELIST, never a second set of bounds:
  the parser already truncates every field
  (`claude/parse_system.go#maxClaudePermission*`), and a duplicate copy of
  those numbers here was only a way for the two to disagree.
  `TestPermissionNoticeKindsMatchTheProviderConstants` pins the shared
  discriminator vocabulary across the provider boundary — triage may not
  import a provider package for a term, so the pin parses the source instead.
  A `permission_denied` also cross-references the tool_call row it explains
  (`annotateDeniedToolCall`): meta plus `items.decision = declined`, never a
  status write, because the denied tool still gets a real tool_result.
- `model_fallback.go` — provider classifier/safety fallback handling:
  persists the bounded warning reason, projects the session-scoped effective
  model without overwriting the requested thread model, and clears that live
  projection when the provider session ends. Three wire subtypes share the
  event and the row shape but NOT the persisted kind — `model_fallback`
  (unavailable), `model_consent_fallback` (credits/consent, which also carries
  `choice` + `persistedAsDefault`) and `model_refusal_fallback` (safety) are
  forwarded verbatim, because the row's whole job is to report the CAUSE. An
  unknown subtype falls back to the refusal kind: the frontend has no branch
  for it, and under-reporting a safety refusal is the worse mistake.
- `tool_lifecycle.go` — tool-call launch/completion rows,
  background-task pairing (Claude), summary/status derivation.
- `background_task_notifications.go` — Claude's
  `system/task_notification` attention signal: the per-event
  `notification` row, the stash drain that writes the `tool_completion`
  sibling, and the `output_file` payload read/enrichment. See "Task
  notifications" below.
- `codex_background.go` / `codex_background_exec.go` /
  `codex_background_subagents.go` — Codex-specific background projection.
  `codex_background.go` holds what both halves share: the
  per-thread correlation state, its constructor/accessor, the tray-changed
  emit, and the two tool-lifecycle entry points (`observeCodexToolStart` /
  `observeCodexToolComplete`) that dispatch into either half.
  `codex_background_exec.go` owns unifiedExec: commands are transient
  running-tray state, typed completions clear live state and persist the
  normal command row using the original item id only while a Codex wire round
  is active, and terminal-wait carriers persist only waited/interacted marker
  rows while a tracker is live. Pending unifiedExec commands are tray-visible
  before a typed wait but only become backgrounded after that wire-typed wait
  signal. `codex_background_subagents.go` owns spawns: spawn-agent starts are
  tracker-only, terminal spawn completions create the visible transcript row
  and may later use background sibling completion rows, `wait_agent`
  resolution lands there, and so do the persisted-launch lookups every other
  spawn path resolves through. Authorized by the
  wire-typed signals enriched onto Meta in
  `internal/provider/codex/protocol.go` (see invariant 25).
- `codex_background_mailbox.go` — the mailbox half of the spawn projection:
  Codex's injected `<subagent_notification>` closure signal. A FINAL_ANSWER
  delivery records terminal status on the owning launch and synthesizes the
  transcript completion row once every child in that spawn is terminal; a
  `MESSAGE` progress beat must do NEITHER and lands as a sub-line instead.
  `codexMailboxCompletionID` is the row identity one delivery gets, and the
  launch resolution both paths share lives here too.
- `codex_background_interactions.go` — the bounded ordered list the spawn
  card renders as sub-lines (`codex_collab_interactions`), its ids-only
  eviction ledger, the per-child resume-generation counter every per-child
  ordinal is minted from, and the `send_input` completion path that appends
  to it. See "Collab interactions" below.
- `codex_background_meta.go` — the pure decoders and formatters the three
  projection files share: `codexItemMeta` / `codexSubagentSignalMeta` and
  their decoders, the child terminal-status readers, and the completion
  status / summary / outcome builders. No Router receiver, no store.
- `terminal_interaction.go` — Codex-specific "Waited for background
  terminal" row persistence. Handles `EventTerminalInteraction` for
  the empty-stdin (polling) variant emitted when the model calls
  `write_stdin` against a backgrounded unified-exec PTY. Non-empty
  stdin persists an "Interacted with background terminal" marker without
  storing stdin bytes.
- `command_lifecycle.go` — Claude-only stdin delivery acks. Resolves a
  `command_lifecycle` ack's `command_uuid` to the AO row it belongs to
  (via the pending-send registry, read-only) and classifies `started`
  as a mid-turn drain or a new turn by comparing the wire round open at
  ENQUEUE time against the one open at pickup. Emits
  `provider:command_lifecycle`; persists nothing. The correlation map
  is send-time carry-over, released on the terminal ack, bounded per
  thread, and swept by `cleanupThread` AND `MarkThreadActive`.
- `fast_mode.go` — `provider:fast_mode`, the live per-thread projection
  of the provider's own fast-mode report (from `system/init` and the
  wire `result`). Emitted only when the wire carried one, because
  absence is "unknown" and an empty frame would render as a denial. No
  state, no persistence.
- `compaction_status.go` — `provider:compacting`, the live per-thread
  flag that the provider is summarizing this thread's context right now
  (drives the activity rail's "Working" → "Compacting" label swap).
  Opened by `EventCompactionStatus` Active frames (idempotent under
  Claude's remote keep-alive repeats), closed by the explicit close
  frame, the compact boundary, or turn completion — the turn boundary
  is load-bearing because a failed Codex compaction abandons its item.
  One bounded map (`compactingSinceByThread`, swept by `cleanupThread`
  AND `MarkThreadActive`); nothing persists. Snapshot-carried in
  `LiveStateSnapshot` because the window spans minutes of wire silence,
  so a reconnect inside it has no frame to re-learn from.
- `provider_commands.go` — `provider:commands`, the live per-thread
  projection of the provider's slash-command list. Fed by `system/init`
  (names only) and by `EventCommandsChanged` (rich entries). Follows
  `fast_mode.go` exactly: live-only, emitted only when the wire carried
  a list, nothing persisted, no state. `Replace` is always true and on
  the wire anyway because it is the contract: both producers restate the
  whole set, so a consumer must never merge one frame into another, and a
  future producer that can only report a delta has to say so rather than
  silently change what an existing frame means. The provider name is read
  from the thread row, so claude-tui replaying the same shapes is
  labelled correctly.
- `command_result.go` — output of a provider-executed (local) slash
  command as its own `command_result` item kind: role `system`, status
  `completed`, never an assistant bubble. Output under
  `commandResultInlineRunes` lives in the summary; larger output moves
  to a payload with a bounded preview. Idempotent on the CLI's
  `message.id`, so the `result` envelope's verbatim repeat of the same
  text can never become a second row.
- `session_wakeup.go` — Claude-only pending-harness-wakeup record.
  Handles `EventSessionWakeup` (the ScheduleWakeup ack, claude-wire.md
  §E8): stores the fire time per thread, clears on the stop ack, and
  exposes `PendingWakeupAt` for the idle-session reaper — the wakeup
  timer is in-process CLI state with no task lifecycle, so this map is
  the only record that an idle-looking session will resume itself.
  Swept by `cleanupThread` AND `MarkThreadActive` (a replacement
  process never inherits the timer).
- `tool_result_file_change.go` — `file_change` tool-result normalisation
  (inline diff projection, unified patch assembly).
- `tool_paths.go` — file-tool predicates and path normalisers shared by
  the `file_change` tool-result dispatch and the command inline-diff
  pipeline: recognises Claude `Edit`/`Write`/`MultiEdit`/`NotebookEdit`
  and Codex `fileChange` items and normalises their paths to
  workspace-relative form.
- `tool_result_diff_upgrade.go` — late-arriving diff upgrades that
  attach a richer payload onto a previously persisted tool result.
- `command_inline_diff_capture.go` / `command_inline_diff_parser.go` /
  `command_inline_diff_runtime.go` / `command_inline_diff_persist.go` —
  command-execution inline-diff pipeline, split by phase
  (capture → parse → runtime match → persist).
- `payload_items.go` — diff / command output / thinking / plan payload
  writers. Codex command-output deltas route through the stream-persist
  buffer (one payload append + one wire upsert per flush window instead
  of per chunk); the Replace snapshot discards the pending buffer.
- `stream_items.go` / `stream_state.go` / `block_events.go` —
  streaming text / thinking block lifecycle and the content-block
  index bookkeeping they depend on.
- `compaction_reasoning.go` — routes claudetui's compaction-summarizer
  reasoning (EventThinking / EventContentBlockStop carrying the reserved
  `provider.CompactionReasoningScope`, dispatched ahead of the normal
  handlers in `router.go`) to a top-level `compaction_reasoning` streaming
  row — the live "compact" tail that settles just ABOVE the `compaction`
  divider. Reuses the thinking streaming machinery (active-block maps,
  tail-bounded persist, async settle) under the reserved scope; turn
  resolution is `currentTurnIndex` (the sentinel is not a real subagent
  parent), and the row is ParentID="" (top-level, never nested).
- `usage_compaction.go` — context-window usage normalisation and
  compaction boundary persistence. `extractCompactionSummary` /
  `buildCompactionPayload` lift the committed summary into an on-demand
  `compaction` payload (raw text in data, like thinking) — summary-only,
  because the summarizer's reasoning streamed separately as its own row.
- `turn_events.go` — frontend-facing payload shapes for
  `provider:turn_started` / `provider:turn_completed`, plus the
  canonical stop-reason normaliser.
- `usage_ledger.go` — projects the per-model per-turn usage deltas on
  turn-complete meta into append-only `usage_ledger` rows
  (`appendUsageLedger`, called from BOTH settle paths — settleTurnRow
  and persistLateTurnPayload — which is additive-correct because
  providers emit deltas). Attribution (provider family, project) is
  read from the thread row at write time. The optional app-wired live workflow
  resolver stamps `work_item_id`; registrations exist only while a phase turn
  can settle, so ordinary threads remain unattributed. Ledger append failure
  is error-logged, never dropped silently.
- `shape.go` — the exported, Router-free row-shape surface: wire meta
  in, `store.Item` / `store.Payload` field values out. Tool-call summary
  / status / result-payload builders, the `ToolStartMeta` /
  `ToolCompleteMeta` decoded wire shapes, and the deterministic row-id
  constructors. See "Exported shape surface" below before editing.
- `meta.go` — shared JSON-inspection helpers.
- `dev_server_url.go` — `DetectDevServerURL`: finds the loopback URL a
  command announced ("Local: http://localhost:5173"), so a collapsed
  command row can offer "open in browser" without loading the output
  blob. Called from `ExtractCommandOutputMeta`, which already makes one
  full pass over the same bytes for `lineCount` — the scan is a second
  single pass anchored on `strings.Index(…, "://")`, no regex, first
  match wins with an early exit, and it allocates only on a hit
  (~4.5 GB/s on non-matching output). Wildcard binds (`0.0.0.0`, `::`)
  are rewritten to `localhost`; non-loopback hosts and URLs embedded in
  stack frames, markdown links, JSON, or env assignments are rejected.
  The `devServerUrl` field inherits `command_output` meta's streaming
  behavior: it reflects the current flush window while the row streams
  and becomes cumulative at the completion rebuild, so the row smooths
  it (see the chat area guide). Detection is a CANDIDATE generator, not
  proof a server exists — output that merely mentions a loopback URL
  (a `tail` of a test file) carries the same meta as a startup banner.
  The frontend renders the chip only after `ProbeDevServerURL`
  (`internal/devserverprobe`) confirms a listener on the port.
- `maps.go` — generic map utilities (currently just `deleteByPrefix`).

## Routing table

| Event kind | Destination |
|---|---|
| Text delta | Frontend (passthrough). |
| Tool-use start/complete | Frontend event + item in SQLite on completion. |
| Approval request | Frontend event with `request_id` preserved. |
| Diff | SQLite payload + meta to frontend. Full diff is on-demand. |
| Command output | SQLite payload + meta to frontend, buffered per flush window (100ms / 64KB / lifecycle boundary). |
| Thinking block | SQLite payload + preview to frontend. |
| Thinking block w/ `CompactionReasoningScope` | Top-level `compaction_reasoning` streaming row (the live "compact" tail above the divider). Reuses thinking streaming machinery; dispatched ahead of `handleThinking` / `handleContentBlockStop`. See `compaction_reasoning.go`. |
| Compaction boundary | `compaction` divider row + on-demand summary payload (`usage_compaction.go`). |
| Turn metadata (cost/tokens) | Per-turn deltas from the provider: aggregate onto `turns.token_usage_json` (first-write-wins for display) + one `usage_ledger` row per model on every settle event (`usage_ledger.go`). |
| Context-window usage | Frontend context meter + `threads.last_token_usage`. |
| Background task terminal (Claude) | `tool_completion` sibling row upsert (idempotent). See `turn-lifecycle.md`. |
| Task notification (Claude) | One `notification` row per NOTIFICATION EVENT — id `task-notification:<taskID>:<uuid>` — plus the `output_file` enrichment onto the `tool_completion` sibling. Never a lifecycle source (invariant 21). See "Task notifications" below. |
| Command lifecycle (Claude) | Live-only `provider:command_lifecycle` keyed onto the AO row id; nothing persists. Older CLIs emit no acks, so no routing decision may depend on them. See `command_lifecycle.go`. |
| Fast-mode report (Claude) | Live-only `provider:fast_mode` from `system/init` and the wire `result`; nothing persists. Absence is unknown, never "off". See `fast_mode.go`. |
| Compaction status | Live-only `provider:compacting` window per thread (open on Active frames, closed by close frame / compact boundary / turn completion); nothing persists. Snapshot-carried for reconnect. See `compaction_status.go`. |
| Slash-command list (Claude) | Live-only `provider:commands` from `system/init` and `commands_changed`; nothing persists. Absence is silence, never an empty palette. See `provider_commands.go`. |
| Todo list (Claude TodoWrite / Task\*, Codex update_plan) | `provider:todo_update` to the frontend + the whole list onto `threads.live_todo` (v65); no timeline row ever. SQLite is its source of truth — it survives session teardown and app restart, and `GetThreadLiveState` reads it from the store, not from triage. Empty steps clear the column and emit a clear only when something was stored. See `timeline_notifications.go`. |
| Permission notice (Claude) | `notification` row per `meta.kind` (`permission_denied` / `permission_retry`) with the notice's own fields forwarded; a denial ALSO stamps `permissionDenied` meta + `items.decision = declined` onto the tool_call row it explains, never its status. See `permission_notices.go`. |
| Model fallback (Claude) | `notification` row keyed on the WIRE SUBTYPE (`model_fallback` / `model_consent_fallback` / `model_refusal_fallback`) + the session-scoped effective-model projection. Never flattened to one kind — the cause is what the row reports. See `model_fallback.go`. |
| User echo with an external origin (`origin: external-queue`) or peer provenance (`cross_session_message`) | A real `user_text` row with a named author, NOT "Injected provider context". These reach the top-level wire-only branch for a structural reason — the producer minted the uuid, so no pending send can match — but their provenance is POSITIVELY known. Everything else unmatched stays injected context. See `handle_user_text.go`. |
| Command result (Claude local command) | `command_result` item (role `system`, status `completed`) + on-demand payload above the inline bound. Idempotent on the provider message id so the `result` echo does not duplicate it. See `command_result.go`. |
| Session wakeup (Claude) | Per-thread pending-wakeup fire time in router state only — nothing persists, nothing emits. Consumed by the idle reaper via `PendingWakeupAt`. See `session_wakeup.go`. |
| Codex unifiedExec / spawn_agent | unifiedExec starts are transient running-tray state; typed command completions clear live state and persist normal command rows using the original item id only while a Codex wire round is active. Spawn-agent starts are pending-only; terminal spawn completions persist the visible row and use sibling `tool_completion` rows. See `codex_background.go` + invariant 25. |
| Codex terminal interaction | Empty stdin persists/reuses one visible `terminal_interaction` wait carrier on the current open turn while the PTY tracker is live. Non-empty stdin first flushes any active wait for that process, then persists an interaction marker without storing stdin bytes. See `terminal_interaction.go`. |
| Turn start/complete | Write `turns` row; emit `provider:turn_*` to frontend; force-close orphan tool_calls on complete. |
| Error `result`, no open round/turn | Orphan error item attributed to the pending-send head (else last turn index); queued-send flush suppressed. Settled turns route to `persistLateTurnPayload` instead. See `turn-lifecycle.md §Error routing` path 5. |
| Error | Distinct event kind; frontend renders as status/alert. |
| Unknown | Log with full context, do not drop silently. |

## Stopped-thread routing (invariant 29)

`CleanupThread` marks the thread stopped; `Handle` then drops EVERY
wire event for it — `EventInit` included. The marker is cleared only by
the host's session-start funnel calling `MarkThreadActive` pre-spawn
(`app_session.go`); no wire event may clear it, because a replacement
session that dies during startup emits its only diagnostics pre-init
(2026-06-10 incident). Host-synthesized events (send-failure synthetic
turn-completes, `emitErrorToThread`) are not stale wire frames — they
route through `HandleSynthetic`. Errors that a wire event *triggers*
on the read loop (discussion-sync failures) use the app's
`emitWireErrorToThread`, which routes through `Handle` and stays
gated. Approval/user-input resolutions stay on `Handle`: they're only
reachable with a live session.

`MarkThreadActive` also clears the thread's `settledTurns` prefix (the
repair-restart path skips `CleanupThread`; a stale settlement marker
would misroute a replacement session's orphan error result into
`persistLateTurnPayload`) and bumps the thread's reactivation epoch.
Asynchronous teardowns capture `ThreadEpoch` BEFORE unregistering
their session and clean up via `CleanupThreadIfEpoch`, which no-ops
once a replacement start has bumped the epoch — the registry token
guard can't cover that window because the replacement's spawn runs for
seconds between `MarkThreadActive` and re-registration. Epoch entries
are never deleted (a reset-to-zero would let a stale captured 0
match). See
[`invariant 29`](../../docs/architecture/invariants.md#29-stopped-thread-event-routing-is-host-controlled).

## Lifecycles we route

Authoritative mental model:
[`turn-lifecycle.md`](../../docs/architecture/turn-lifecycle.md).
Keep this guide to local editing rules; do not duplicate the full
lifecycle spec here.

- **Tool lifecycle** — `EventToolStart` / `EventToolComplete` keyed by
  the provider tool id. Triage upserts `tool_call` rows; Claude
  background placeholders and Codex `spawn_agent` child completions
  have their separate sibling-row rules in the lifecycle doc.
- **Task lifecycle (Claude only)** — host process exit and agent
  observation are deliberately decoupled. `task_updated` can hide a
  launch from the tray before chat gets the later observed
  `tool_completion` row.
- **Codex background projection** — `unifiedExecStartup` starts are
  transient tray-visible live state; typed item completion clears the
  tracker and persists command history only while the Codex wire round
  is active. Empty `write_stdin` waits and `spawn_agent` child state
  are the only authorization signals for `is_background=true`. See
  [`invariant 25`](../../docs/architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics).
- **Turn lifecycle** — `EventTurnStart` inserts a `turns` row and
  `EventTurnComplete` settles it. Frontend activity is pushed per wire
  round, while persistence settles per logical turn. See
  [`turn-lifecycle.md §Wire-round vs logical-turn cadence`](../../docs/architecture/turn-lifecycle.md#wire-round-vs-logical-turn-cadence)
  and
  [`invariant 27`](../../docs/architecture/invariants.md#27-soft-round-close-from-message_deltastop_reason-is-wire-typed).

Load-bearing reminders:

- `task_notification` is not a completion source.
- Turn activity on the frontend is wire-pushed only.
- Do not infer turn state from session liveness probes.
- Re-round paths (`maybeReopenSettledRound`) must not call
  `setOpenTurn`; that would reset id-allocating counters and collide
  with rows already persisted under the same logical turn.

### Task notifications (`background_task_notifications.go`)

`system/task_notification` is an ATTENTION SIGNAL, never a completion
source (invariant 21). Four rules govern what it writes:

- **The row id is per-EVENT, not per-task.** `nextTaskNotificationID` is
  `task-notification:<taskID>:<uuid>`, where `uuid` is the notification
  envelope's own top-level id (for the synthetic-XML channel, the
  wrapping user envelope's — see `claude/parse_user_replay.go`). A
  persistent Monitor (claude-wire.md §E7) fires one notification per
  output event of the stream it watches, all under ONE `task_id`, and
  Claude sees each as a distinct message; a task-only id made each event
  upsert over the last so only the newest survived. The uuid is what
  makes them distinct AND what keeps them idempotent — the id stays
  deterministic, so a reconnect replaying an envelope re-upserts its own
  row rather than appending a duplicate. A CLI that carries no uuid (and
  the `claudetui` reconstruction, which synthesizes these envelopes and
  has no per-notification id to offer) falls back to the legacy
  task-only id, which is the pre-existing upsert-in-place behavior. For
  an ordinary background task — one notification, one task — both forms
  produce exactly one row.
- **`meta.watch_task` is copied from the launch onto every notification
  row.** The frontend's `filterRedundantNotifications` hides a
  notification whose task has a completed lifecycle row; for a watch
  task that would erase the whole interim event history at the moment
  the stream ended, so the filter keeps rows that carry this marker. It
  is stamped HERE, at write time, rather than derived at render time,
  because the launch row may be windowed out of the pane long before its
  notifications are. Written only as `true`; an absent key means "not a
  watch task", which is also what every row persisted before the field
  existed says.
- **The summary survives the hide as a caption on the sibling — but
  only on the sibling's FIRST write, and never for a watch task.** The
  frontend hides an ordinary task's notification row only when the
  sibling has ABSORBED it (`meta.notification_summary` present, or the
  two summaries are equal text), so the text Claude itself saw
  ("Background command … completed (exit code 0)") is never silently
  lost: either the sibling carries it as a caption or the notification
  row stays visible. The caption's one chance is the sibling's first
  write — a mounted card must not grow a line (chat AGENTS.md row-shell
  contract) — enforced at the one writer
  (`writeBackgroundCompletionSibling` clears
  `meta.NotificationSummary` and passes `""` through
  `captionForSiblingWrite` when a persisted sibling exists) and by the
  enrich path never stamping at all. Both first-write orders produce
  it: sibling-first through `backgroundTaskTerminalMeta.
  NotificationSummary` (internal, never-on-the-wire, set by the stash
  drain), notification-first through `captionForSiblingWrite` reading
  the persisted notification row. Watch tasks never get a caption:
  their notification rows are exempt from the hide, so a caption would
  say the same text twice on adjacent rows. The frontend renders the
  caption as one muted line and skips it when it repeats the row's own
  summary.
- **No resolvable launch is a DROP, and the drop is logged.** Hidden
  subagent work has no parent-thread row a notification could hang off,
  so nothing is written — but the stash drain still runs and the drop
  names its `task_id` and summary, because a silently vanished
  notification is indistinguishable from one that never arrived.

The foreground skip (`!launch.IsBackground`) stays: the launch's own
status flip is the completion signal and a second row would be
redundant. It still drains the stash.

## Exported shape surface

Triage is no longer the only writer of AO's timeline rows.
`internal/sessionimport` replays historical provider sessions
(`~/.claude` transcripts, `~/.codex` rollouts) straight into SQLite and
deliberately does NOT drive `Router`: the Router has live-only side
effects (session-ref updates, activity marks, `now()`-stamped usage,
async settle goroutines) and would persist imported prompts as "Injected
provider context" notifications. The only thing keeping an imported
thread from rendering differently than a live one is that both call the
same shaping code, so the pure helpers are exported rather than
duplicated.

The surface, and the rules that come with it:

- `shape.go` — `ToolStartMeta` / `ToolCompleteMeta` +
  `DecodeToolStartMeta` / `DecodeToolCompleteMeta`,
  `StoredToolCallMeta`, `BuildToolCallSummary`,
  `BuildCompletionSummary`, `CompletionBaseSummary`,
  `CompletionSuffix`, `CompletionStatus`, `CompletionPayloadForTool`,
  `CommandCompletionPayload`, and the row-id constructors `TextItemID`,
  `ThinkingItemID`, `ErrorItemID`, `ToolCompletionID`,
  `AssistantTextPayloadID`, `ThinkingPayloadID`.
- `tool_meta_rules.go` — `ShapeToolItemMeta(item, now)`, the pure core.
  The `(*Router).shapeToolItemMeta` wrapper adds nothing but the
  log-and-keep-going policy for a shaping error; import owns its own.
- Pure helpers that stayed in the lifecycle file that owns their
  concern, because their private twin sits right next to them:
  `CompactionItemID` / `NormalizeProviderCompactionID` +
  `ExtractCompactionSummary` / `BuildCompactionPayload`
  (`usage_compaction.go`), `CanonicalStopReason` (`turn_events.go`),
  `BuildCommandResultRow` / `CommandResultItemID`
  (`command_result.go`), `ForceCloseSummary` (`turn_lifecycle.go` —
  invariant 23's marker; the import applies the same settle at its own
  turn boundaries, since an imported thread has no live session to
  settle a stuck tool_call later), `APIErrorEnum` +
  `ClampErrorSummary` (`router.go`), `SummarizeToolResult` /
  `ToolResultPayloadID` / `ExtractFileChangeToolResult`
  (`tool_result_file_change.go`), `IsFileChangeItemType` /
  `IsClaudeFilePathTool` (`tool_paths.go`), `NotificationItemID`
  (`timeline_notifications.go`), `CommandFromLaunch`
  (`background_task_notifications.go`), `MergeStoredToolCallMeta`
  (`tool_lifecycle.go`), `BuildPayloadMeta` (`payload_items.go`), and
  `ThinkingSummaryPreview` (`stream_items.go`). Same rules as
  `shape.go`: pure, and a change to one changes both writers.
- `InterruptedSummary` (`stream_state.go`) is the same rule for a
  SECOND non-live writer: the mid-turn fork settle
  (`app_thread_fork.go` → `store.SettleForkedThreadAsInterrupted`)
  applies the interrupted treatment to a fork's cloned rows and must
  produce exactly the suffix a live truncated turn-complete and the
  boot crash sweep write. A fork thread has never had a session, so
  the Router has no state for it and driving it would be the same
  mistake the importer exists to avoid — sharing the suffix function
  is what keeps the three settle paths from drifting.
- Already exported and reused as-is: `ExtractDiffMeta`,
  `ExtractCommandOutputMeta(WithError)`, `ExtractThinkingMeta`,
  `ExtractCompactionMeta`, `ExtractProposedPlanMeta`, `ToolResultMeta`,
  `ToolInlineDiff(File)`, and the Claude file-change extractors
  `ExtractClaudeFileChangeToolResult` / `ExtractClaudeLaunchFilePath`.

Editing rules:

- These stay PURE — no Router receiver, no store access, no clock, no
  logging. A helper that needs correlation state belongs in the
  lifecycle file that owns that state; only its pure core belongs here.
- Changing an id format or a summary/status rule changes BOTH writers at
  once. That is the point. `internal/sessionimport`'s parity test routes
  synthetic events through the real `Router` and through the import
  writer and asserts identical rows modulo ids and timestamps, so it is
  the gate that catches a shape change made on only one side.
- Do not "fix" a shape for import alone. If import needs a different
  row, that is a routing decision and belongs in the importer, not in a
  branch inside a shared helper.

## Responsibility boundary

- What BELONGS here:
  - Classify a single event → zero or more (persist + emit) decisions.
  - Bounded per-thread transient correlation state with explicit
    cleanup paths (see below).
  - Shared helpers for `persistItem` and `emitThreadUpdated`.
- What does NOT belong here:
  - Cross-turn derivations — do them in the frontend or as a persisted
    projection, not as an in-memory map here.
  - Provider-specific types. Provider packages normalize before handing
    events to triage.
  - Business decisions about when to fork/resume a thread; that's
    `app.go`.

## Correlation state (bounded, not derived)

Router maps exist only to correlate adjacent provider events; they are
not a cache of store or provider-session data. Every map needs a clear
owner and cleanup path.

Use these categories when adding or moving state:

- **Per-turn flow-control** (`openTurns`, streaming block flags,
  approvals, user-inputs, pending inline diffs): clean in
  `clearOpenTurn` or the correlated resolver.
- **Id-allocating counters** (`segmentIndexByScope`,
  `blockIndexByScope`, `errorSeqByScope`, `terminalInteractionSeq`):
  clean in `CleanupThread`, not at turn boundaries. These allocate
  thread-lifetime `items.id` values.
- **Logical-turn settlement** (`settledTurns`): survives wire-round
  boundaries and is reset by a fresh `setOpenTurn`.
- **Durable user-visible state**: persist it as soon as it becomes
  known instead of keeping it in a router map. The activity-rail todo
  list is the worked example — it spent a while as `latestTodoByThread`
  and was erased by every restart and every `CleanupThread` until v65 put
  it on the thread row. The `tasksByThread` map beside it stays in memory
  on purpose: that one is Claude Task\* id correlation, which the durable
  projection is derived FROM, not the durable state itself. But because
  the projection — and the PROVIDER's own task list, which a plain
  `--resume` keeps alive — both outlive the map, a cold map must be
  re-seeded from the column before a Task\* event applies
  (`seedTasksFromStoredTodo`): a resumed session updates and deletes ids
  minted before the restart, and a nil map would drop those events and
  freeze the durable list in a state the provider has moved past, with
  no event that could ever clear it. The seed is a one-shot re-derivation
  at the session boundary, not a store cache — the store is read only
  while the map is nil. An all-completed stored list seeds nothing: the
  CLI (≥2.1.233) deletes a fully-completed list's task files shortly
  after completion, so those ids name tasks the provider no longer has,
  and seeding them would resurrect a finished list into the next one.
  Its soundness otherwise leans on an app-side contract:
  paths that start a thread's next session from scratch on the SAME row
  (rollback, provider switch) call `ResetThreadTodo` so a dead list's
  ids can never seed against a session that will mint them again.

### Collab interactions on the Codex spawn card

MultiAgentV2 has no per-interaction transcript row, so a parent -> child
message, a child -> parent progress beat, and a child resume are recorded ON
the owning spawn launch as a bounded ordered list under
`codex_collab_interactions` (`codex_background_interactions.go`). Rules:

- **The three `kind` values are wire constants.** They are persisted in
  `items.meta` and consumed verbatim by the frontend's
  `COLLAB_INTERACTION_KINDS`; a rename on either side silently BLANKS every
  stored sub-line rather than erroring, so
  `TestCodexCollabInteractionKindsMatchTheFrontendMirror` parses the TS.
- **`mergeCodexCollabInteraction` is the one upsert-and-bound rule.** Both
  writers go through it — the persisting wrapper and the resume path, which
  folds its entry into a larger meta merge. Entries are idempotent by id.
- **The stored cap (`maxCodexCollabInteractions`) is deliberately larger than
  what the card renders.** Everything past it is gone from SQLite for good, so
  the headroom is what lets the visible window grow without a migration.
- **Idempotency outlives the cap, and its horizon is DECOUPLED from the
  rendered one.** The upsert only sees the retained tail, so a duplicate
  arriving after its original was trimmed (reconnect replay, duplicate
  completion leg) would append as the NEWEST sub-line and evict a live one.
  `codex_collab_interactions_evicted` is the bounded ledger of what fell off;
  an entry named there is dropped, not re-recorded. It stores a 4-byte digest
  of each id (`codexCollabInteractionEvictedDigest`) and reads BOTH that and
  the raw ids older rows hold, which is what pays for a horizon four times the
  retained cap at less meta than the raw-id ledger it replaced.
  `maxCodexCollabInteractionsEvicted` must stay strictly greater than
  `maxCodexCollabInteractions`: tying them together (it was `=` once) means the
  entry `maxCodexCollabInteractions + 1` positions back is in NEITHER structure,
  so the exact failure the ledger exists to prevent simply reappears at a higher
  count. A digest collision costs one dropped sub-line, never a wrong one — the
  direction the ledger already errs in. Past the horizon a replay is
  unrecognisable again; that is the bound's price, not an oversight.
- **A PLAINTEXT progress note keeps its body; an encrypted one has none.** The
  raw carrier is projected as an internal event and nothing else persists that
  text, so an empty `text` means "no body on the wire", never "the child said
  nothing".
- **Every per-child ordinal comes from the durable generation counter, never
  from a list position.** The `resumed` sub-line's id is
  `resumed:<child>:<codex_child_resume_generations[child]>`. Counting the
  retained `resumed` entries instead walks BACKWARDS after the first trim, so
  every later resume re-mints one id and the upsert folds them onto a single
  sub-line.
- **Row identity for a mailbox delivery mixes the child's resume generation.**
  `codexMailboxCompletionID` is otherwise a pure content hash, which collapsed
  a child that legitimately answered identically twice. The generation is
  durable launch meta (`codex_child_resume_generations`), never an in-memory
  counter, so both carriers of ONE delivery still agree.

If a new map represents user-blocking live state, add it to
`HasPendingWork` in `interactive_requests.go` and cover it in
`interactive_requests_test.go`.

Async streaming settlement is intentionally off the provider read loop.
Keep the synchronous state flip under `r.mu`, and keep the
`streamingItemCounts` decrement plus interrupt-queue drain inside the
settle goroutine so the `0 -> drain` transition happens after SQLite
has the row. See `stream_state.go` and `multi_result_test.go` before
changing the cleanup cadence. Two counters move in lockstep
(`incStreamingCounts`/`decStreamingCounts`): the thread-wide
`streamingItemCounts` gates the interrupt-queue DRAIN; the per-scope
`streamingScopeCounts` gates the QUEUE decision, so a new mid-stream row
defers only behind a SAME-scope stream (invariant 11). A new
streaming-block kind must bump both via those helpers.

## Raw chat content

Triage persists raw item summaries and raw payload data only. It must not
render markdown, ANSI, Mermaid, KaTeX, or code blocks. The frontend owns
chat rendering because it knows which rows are mounted and visible.

Streaming text/thinking rows create a row on first content, then emit all
timeline row mutations on the ordered `provider:item_event` channel:
`action=upsert` for row creation/lifecycle snapshots and `action=delta`
for follow-up raw text. SQLite receives the same raw text through the
stream persistence buffer. Streaming command output has no wire delta
channel: chunks accumulate in the same buffer and each flush window
lands as one payload append plus one row upsert (the upsert's
`updatedAt` bump is what refreshes an expanded output view). Do not
split streaming text across separate UI event channels, and do not add
another rendered cache column or a server-side kind-to-renderer
dispatch table.

Top-level text/thinking blocks recovered from a never-streamed
assistant snapshot (CLI-internal retry) persist as completed rows but
reuse the streaming wire shape — upsert(streaming, blank summary) →
delta(full content) → patch(completed) — so they animate instead of
mounting wholesale (`persistCompletedBlockEmitStreaming`); subagent
recoveries keep the single completed upsert.

## App-layer observers and enrichers

The router exposes two optional observers plus one enricher
(nil-disabled function fields, wired by `newTriageRouter` in
`app_flush_queue.go` — every router construction site must go through
it). All may fire on the provider read loop, so implementations must
not block:

- `SetAssistantTextStreamObserver` — a streaming assistant_text row's
  full accumulated summary at each persistence flush window
  (final=false) and once at settle with the final model text
  (final=true). Backs the remote-client highlight seed push
  (`app_highlight_seed.go`).
- `SetDiffPayloadObserver` — a just-persisted diff-bearing payload with
  COMPLETE content: tool results (`persistToolResult` and the
  summary_only→exact upgrade) carry payloadID + preview patches + the
  full unified patch; diff-kind payload full writes carry payloadID +
  patch (the append branch never notifies — its content is a delta).
  Backs highlight span persistence (`payloads.preview_spans` /
  `payloads.spans` columns) and the remote diff seed push
  (`app_highlight_diff_seed.go`).

The observers are observation taps, not routing decisions: they must
never influence what triage persists or emits. (The diff payload
observer's app-side worker writes span columns back through the store,
but asynchronously and never through router state.)

- `SetCodeSpanEnricher` (`code_spans.go`) is the one deliberate
  enrichment contract: a settled assistant text's fence spans, merged
  into `items.meta` under `codeSpans` at every assistant_text persist
  site — the same version-keyed derived-metadata precedent as
  `pathRefs`. It must remain a pure function of the text: no router
  state, no influence on any other routing decision.

## Extension points

- To add routing for a new event kind: pick or create the matching
  `*_lifecycle.go` / `*_items.go` file, add a `Handle` switch case in
  `router.go`, write the routing-decision test FIRST. See
  `docs/architecture/how-to.md#add-a-new-event-kind`.
- To add a new persisted payload kind: extend `payload_items.go`,
  update `docs/architecture/schema.md`.

## Anti-patterns

- Do NOT cache store data here. No caching of store data. Transient
  correlation state only. Cross-turn derivation forbidden beyond the
  interrupt queue.
- Do NOT put preview content in the payload data blob. Meta is cheap,
  data is heavy — preview/stats in `meta`, full content in `data`.
- Do NOT combine or split events across boundaries. One event in, zero
  or more routing decisions out.
- Do NOT reach back into provider-specific types. If you need a detail
  the normalized event doesn't carry, fix the normalization upstream.

## Testing

- Every routing decision has a unit test with a representative event.
- When a new provider event type is added upstream, the routing
  decision is the first test — not the last.

## References

- `docs/architecture/data-flow.md` — end-to-end pipeline diagram.
- `docs/architecture/triage-routing.md` — detail on per-kind decisions.
- `docs/architecture/schema.md` — payload / item column reference.
