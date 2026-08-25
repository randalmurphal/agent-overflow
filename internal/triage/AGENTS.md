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
  approval-resolved fan-out, decision → item projection. Also owns
  `resolveInteractiveScope`, the one attribution rule both interactive
  families use — see "Interactive scope" below.
- `user_inputs.go` — structured user-input request lifecycle and
  provider:user_input frontend event fan-out. Scoped through the same
  `resolveInteractiveScope`.
- `subagent_progress.go` — the LIVE per-launch progress state (tool
  count, token spend, elapsed, activity line) plus the backgrounded
  stamp and the `background_tasks_changed` forward. See "Live subagent
  progress" below.
- `turn_lifecycle.go` — per-turn and per-thread correlation state
  (open turns, interrupt queue, stopped-thread markers, cleanup paths).
  Also owns turn-index allocation and the `turns`-row reconciliation
  every opener goes through — see "Turn index is allocated once" below.
- `pending_send.go` — the per-thread FIFO of AO-initiated user messages
  awaiting their wire echo, its registration surface, and the one
  consumption rule `handleUserText` pops through. See "Pending-send
  consumption" below.
- `pending_send_transitions.go` — the same registry's named state
  transitions, split out only because `pending_send.go` is at its size
  ceiling. Every pendingSend mutation reachable from another file lives
  here, each with the precondition and the lock it needs; nothing
  outside these two files writes a `pendingSend` field. See
  "Pending-send transitions" below.
- `send_shape.go` — the typed `sendShape` a registrar stamps on every
  pending entry (direct / flush / steer) — the authoritative flush
  classifier readers branch on — plus the registration-time assertion
  pinning the stamp to the App layer's id grammar. See "Send shape is
  stamped at registration" below.
- `turn_telemetry.go` — live turn-span lifecycle plus the one outcome
  classifier shared by span status and completed/error counters.
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
- `subagent_prompt.go` — the launch-time opening-prompt row for providers
  whose async child stream cannot carry that row before child output.
- `background_task_notifications.go` — Claude's
  `system/task_notification` attention signal: the per-event
  `notification` row, the stash drain that writes the `tool_completion`
  sibling, and the `output_file` payload read/enrichment. See "Task
  notifications" below.
- `subagent_transcript.go` — the transcript backfill a `local_agent`
  task_notification can trigger for an older provider process without a
  transcript-mirror marker. New sessions project the mirror live and skip
  this compatibility replay. See "The task_notification path" below.
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
  `completed`, never parent-assistant output. A typed forked-agent source
  preserves honest attribution while allowing its result to render as
  Markdown. Output under
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
- `thread_state.go` — `threadState` (the Router's whole per-thread
  correlation surface, dropped in one `delete` at cleanup) and
  `threadIdentity` (the per-thread state that must SURVIVE cleanup:
  epochs, generations, the flush-stamp ledger, interrupt marks, and the
  anchor/drain locks), plus the four accessors every call site goes
  through. Read the type docs before adding per-thread state — where a
  field lands decides whether a session teardown drops it.

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
| Task notification (Claude) | One `notification` row per NOTIFICATION EVENT — id `task-notification:<taskID>:<uuid>` — for a TOP-LEVEL launch or any watch task, plus the `output_file` enrichment onto the `tool_completion` sibling, the run's final `usage` folded onto the launch row, and a compatibility sidechain backfill only when the launch has no transcript-mirror marker. Never a lifecycle source (invariant 21). See "Task notifications" below. |
| Command lifecycle (Claude) | Live-only `provider:command_lifecycle` keyed onto the AO row id; nothing persists. Older CLIs emit no acks, so no routing decision may depend on them. See `command_lifecycle.go`. |
| Fast-mode report (Claude) | Live-only `provider:fast_mode` from `system/init` and the wire `result`; nothing persists. Absence is unknown, never "off". See `fast_mode.go`. |
| Compaction status | Live-only `provider:compacting` window per thread (open on Active frames, closed by close frame / compact boundary / turn completion); nothing persists. Snapshot-carried for reconnect. See `compaction_status.go`. |
| Slash-command list (Claude) | Live-only `provider:commands` from `system/init` and `commands_changed`; nothing persists. Absence is silence, never an empty palette. See `provider_commands.go`. |
| Todo list (Claude TodoWrite / Task\*, Codex update_plan) | `provider:todo_update` to the frontend + the whole list onto `threads.live_todo` (v65); no timeline row ever. SQLite is its source of truth — it survives session teardown and app restart, and `GetThreadLiveState` reads it from the store, not from triage. Empty steps clear the column and emit a clear only when something was stored. See `timeline_notifications.go`. |
| Permission notice (Claude) | `notification` row per `meta.kind` (`permission_denied` / `permission_retry`) with the notice's own fields forwarded; a denial ALSO stamps `permissionDenied` meta + `items.decision = declined` onto the tool_call row it explains, never its status. See `permission_notices.go`. |
| Model fallback (Claude) | `notification` row keyed on the WIRE SUBTYPE (`model_fallback` / `model_consent_fallback` / `model_refusal_fallback`) + the session-scoped effective-model projection. Never flattened to one kind — the cause is what the row reports. See `model_fallback.go`. |
| Scoped user echo (`parent_tool_use_id` set) | Nested `user_text` under the launch on the LAUNCH's turn, never the thread's current one. The opening prompt is created from the launch input as `user:subagent-prompt:<launchID>` before child output. Its first transcript row stamps `provider_item_id` onto that row in place. Later user-role deliveries use `user:wire:<provider_item_id>`. `wire_only` keeps both out of reader-authored user-text reads. See `subagent_prompt.go` and `handle_user_text.go`. |
| User echo with an external origin (`origin: external-queue`) or peer provenance (`cross_session_message`) | A real `user_text` row with a named author, NOT "Injected provider context". These reach the top-level wire-only branch for a structural reason — the producer minted the uuid, so no pending send can match — but their provenance is POSITIVELY known. Everything else unmatched stays injected context. See `handle_user_text.go`. |
| Command result (Claude local command) | `command_result` item (role `system`, status `completed`) + on-demand payload above the inline bound. Idempotent on the provider message id so the `result` echo does not duplicate it. See `command_result.go`. |
| Session wakeup (Claude) | Per-thread pending-wakeup fire time in router state only — nothing persists, nothing emits. Consumed by the idle reaper via `PendingWakeupAt`. See `session_wakeup.go`. |
| Codex unifiedExec / spawn_agent | unifiedExec starts are transient running-tray state; typed command completions clear live state and persist normal command rows using the original item id only while a Codex wire round is active. Spawn-agent starts are pending-only; terminal spawn completions persist the visible row and use sibling `tool_completion` rows. See `codex_background.go` + invariant 25. |
| Codex terminal interaction | Empty stdin persists/reuses one visible `terminal_interaction` wait carrier on the current open turn while the PTY tracker is live. Non-empty stdin first flushes any active wait for that process, then persists an interaction marker without storing stdin bytes. See `terminal_interaction.go`. |
| Turn start/complete | Write `turns` row; emit `provider:turn_*` to frontend; force-close orphan tool_calls on complete. The row write RECONCILES the index first and returns the one it landed on — see "Turn index is allocated once". |
| Turn start with an external origin (`origin: external-queue`) | Same as any turn start except the index: a foreign dispatch allocates past every known turn AND every pending send instead of reading the pending-send head, which names a message AO is still waiting on. See "Turn index is allocated once". |
| Error `result`, no open round/turn | Orphan error item attributed to the pending-send head (else last turn index); queued-send flush suppressed. Settled turns route to `persistLateTurnPayload` instead. See `turn-lifecycle.md §Error routing` path 5. |
| Error | Distinct event kind; frontend renders as status/alert. |
| Subagent progress tick | Live-only `provider:subagent_progress` + the merged latest tick per launch in a bounded map (monotonic counters take the max, `TotalTokens` takes the newest value because Claude's can dip on a subagent compaction); nothing persists until the launch's terminal folds the final numbers onto `meta.subagentProgress`. See "Live subagent progress" below. |
| Subagent backgrounded | Flips the launch to `is_background` and stamps `meta.subagentBackgroundedAt` first-wins (the moment sidechain streaming stopped). |
| Background tasks changed | Live-only `provider:background_tasks_changed`; the whole set every time, and an empty set is a real answer. |
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
- **The hide is existence-based; the caption is display enrichment,
  first-write-only, never for a watch task.** The frontend hides an
  ordinary task's notification row once a COMPLETED lifecycle sibling
  with the same task_id exists (`filterRedundantNotifications`) — the
  bell's text is the CLI's formulaic restatement of the sibling's own
  description + exit code, and the common agentic wait order (bg Bash +
  blocking TaskOutput) writes the sibling captionless first, so an
  absorption-only hide left the bell visible on every waited background
  command (user ruling 2026-08-22). The row itself always stays in
  SQLite. When the write order allows it, the sibling still carries the
  bell text as a caption (`meta.notification_summary`) rendered as one
  muted line on the card: the caption's one chance is the sibling's
  first write — a mounted card must not grow a line (chat AGENTS.md
  row-shell contract) — enforced at the one writer
  (`writeBackgroundCompletionSibling` clears
  `meta.NotificationSummary` and passes `""` through
  `captionForSiblingWrite` when a persisted sibling exists) and by the
  enrich path never stamping at all. Both first-write orders produce
  it: sibling-first through `backgroundTaskTerminalMeta.
  NotificationSummary` (internal, never-on-the-wire, set by the stash
  drain), notification-first through `captionForSiblingWrite` reading
  the persisted notification row. Watch tasks never get a caption:
  their notification rows are exempt from the hide, so a caption would
  say the same text twice on adjacent rows. The frontend skips the
  caption when it repeats the row's own summary.
- **No resolvable launch is a DROP, and the drop is logged.** Hidden
  subagent work has no parent-thread row a notification could hang off,
  so nothing is written — but the stash drain still runs and the drop
  names its `task_id` and summary, because a silently vanished
  notification is indistinguishable from one that never arrived.

The foreground skip (`!launch.IsBackground`) stays: the launch's own
status flip is the completion signal and a second row would be
redundant. It still drains the stash.

#### The task_notification path (Q11, usage, transcript backfill)

Three more things happen on this envelope, all of them because it is the
agent's TERMINAL and the last chance to say anything about the run.

- **The notification row is the thread's BELL, and a bell fires for
  top-level nodes only** (`docs/specs/agent-visibility.md` Q11). A launch
  with a `ParentID` writes NO notification row: a nested agent finishing
  updates its card, it does not raise the thread. Everything else on the
  path still runs for it — the stash drains, the output file is read, the
  completion sibling is enriched with the payload and the output state,
  and an old-process transcript is backfilled when needed — so nothing but
  the bell is lost.
  `writeBell` is the one gate and `persistBell` the one write point, so a
  new state transition cannot accidentally reintroduce the row.
  **A watch task is exempt at any depth**: its notification rows are not
  a bell at all, they ARE its event history (claude-wire.md §E7, exempt
  from the frontend's redundant-notification hide), so suppressing a
  nested one would delete content no other row carries.
- **`meta.usage` is the run's authoritative final counters** and folds
  onto the LAUNCH row through `persistSubagentFinalProgress`
  (`subagent_progress.go`), before any row work and on every path out of
  the handler — foreground launches included. It is folded, not
  assigned: the merge base is what is already persisted, so a
  `task_updated` terminal carrying a live tick and this envelope carrying
  the real numbers land the same result in either order. A usage-less
  envelope (every `local_bash` bookend) stamps nothing, so a zero object
  can never overwrite real numbers. Background Bash is not special-cased
  out — the merge is order-free and a launch with no counters is left
  untouched, so the only cost of not branching on the tool type is a
  comparison.
- **`output_file` is terminal enrichment and authoritative reconciliation**
  (`subagent_transcript.go`). New Claude processes launch with
  `--session-mirror`; a mid-flight backgrounded agent's later rows are
  projected live and stamp `transcript_mirrored` on the launch. That marker
  proves only that a prefix arrived. It cannot prove the final mirror batch
  arrived before `task_notification`, so triage reconciles every agent's
  sidechain JSONL at terminal with
  `claude/sessionimport.ConvertSubagentTranscriptData` and replays the missing
  tail. The projector reuses the bytes already read for the completion
  payload, avoiding a second file read. Only AGENT launches take this path. A `local_bash` task's
  `output_file` is captured stdout, which the command-output payload owns.

**Dedupe identity, and why the events are REPLAYED rather than written
as rows.** The obvious implementation — hand the projected events to
`internal/sessionimport`'s writer — is wrong twice over. It is a package
cycle (`sessionimport` imports `triage` for every row shape, so triage
can never import it), and, independently, the two writers do not agree
on a SUBAGENT-scoped row id: the Router allocates `text:<turn>:<scope>:<n>`
from a live per-scope counter that starts at 1 for a subagent scope
(only scope `""` is seeded to `-1`), while the importer allocates from 0.
Rows written the importer's way would be invisible to every later live
lookup. So the backfill goes through triage's OWN persist paths, which
mint live ids by construction.

That leaves dedupe, which therefore cannot key on the row id for
text/thinking. It keys on the identity BOTH writers spell identically:

| Event | Identity |
|---|---|
| tool start / complete | the `tool_use_id`, which IS the row id on both sides. A completion additionally requires the row to have left `running` — a launch the live stream left running is the tool that was in flight at the cut, and nothing else will ever settle it. |
| assistant text / thinking | `items.meta.provider_item_id` — the provider's own `<messageID>#<ordinal>`, written by the live parser (`recoveredBlockItemID`) and the importer (`nextBlockItemID`) in the same spelling, and queryable via `FindStreamItemByProviderItemID`. |
| the agent's own prompt | `items.meta.provider_item_id` on the launch-scoped row `user:subagent-prompt:<launchID>`. New live launches create the row from tool input, then transcript projection binds its uuid without changing `item_index`; import marks the first scoped user row and writes the same id. Legacy `user:wire:<uuid>` rows remain recognised. |
| compaction | the provider boundary UUID embedded by `CompactionItemID`; reconciled independently because the SDK can omit it while forwarding later rows. Its exact summary remains an on-demand payload on that divider. |
| everything else (errors, command results) | UNDECIDABLE. Their live ids are per-turn sequence numbers that say nothing about which event produced them. |

`subagentBackfillCut` therefore finds the first ordinary decidable-and-missing
event and replays the whole tail from there, which is the shape of the
wire fact: backgrounding stops a sidechain at a POINT, so everything
before it streamed and everything after it did not. A compaction boundary is
reconciled separately by provider UUID because the SDK selectively omits it;
it is not evidence of a cut. Carrying the
undecidable events along with the neighbours they arrived between is the
only way to place them at all. One store read builds the index — a
subagent's rows all carry the launch's `turn_index` (invariant 10), so
one turn read is a superset of what a replay could duplicate.

**The agent's own prompt is outside the cut entirely** (`replaySubagentEventAt`).
The CLI echoes it on ordinary stdout only for an INLINE agent
(claude-wire.md §"Subagent stream forwarding"). New launches therefore
persist `input.prompt` immediately as the first child row. Transcript
projection later binds the source uuid onto that same row. A process that
started before launch marking has no provisional row, so compatibility
backfill still creates its legacy provider-keyed prompt. Neither case says
where sidechain streaming stopped, and letting the prompt decide the cut
would replay every async agent's transcript from row zero.

Placement follows from the same invariants: rows go in at the LAUNCH's
turn (10) at the store's own `MAX(item_index)+1` (1 and 11 — `item_index`
is immutable after the first upsert and `(thread, turn, item_index)` is
UNIQUE, so there is no splicing mid-turn). Every handler the replay
dispatches into resolves its turn through `turnIndexForEvent`, so a
scoped error / notification / command result / compaction lands on the
launch's turn even when the thread has moved on. Text and thinking
deliberately bypass `handleTextDelta` / `handleThinking`: those open a
streaming block and wait for a stop the transcript has no event for,
which would leave the scope's streaming count incremented forever and
wedge the interrupt queue.

**A subagent's events never reach the main agent's thread** — not its
rows, not its context. The replay enforces two things the live handlers
would otherwise get wrong for a detached agent's history:

- an error replays with `fatal:false` (`replayedErrorMeta`). The
  converter stamps a fatal API error the way an import reads it (the end
  of the session); dispatched live it would flip the thread's CURRENT
  turn to errored and run the fatal finish for an error the main agent
  never saw. The row still persists as `api_error`, under the launch.
- a compact boundary carrying a scope persists as a compaction row under
  the launch and nothing else (`persistSubagentCompaction`): no
  compacting-window close, no usage-throttle reset, no context-window
  write. The meter is the main agent's; a subagent's compaction is
  private to it, exactly as `handleTokenUsage` drops scoped usage.

**Failure is loud.** A file that cannot be resolved, is over
`claudeTaskOutputFileMaxBytes`, or cannot be projected surfaces on the
existing `outputState = "error"` path with the reason attached. A
silently incomplete agent transcript reads exactly like a complete one,
and no second signal would ever correct it. (The size ceiling is refused
rather than truncated here even though the PAYLOAD read truncates and
succeeds: a transcript cut mid-line would replay into rows that silently
lose the tail.)

**The raw `output_file` payload stays.** It is not a duplicate of the
backfilled rows: one payload row (`tool-call-result:<launchID>`) is
referenced by both the notification and the completion sibling, and it is
the LOSSLESS artifact behind a lossy projection — the importer drops
`attachment` rows and unknown `system` subtypes. Suppressing it for agent launches would also mean editing
`tool_lifecycle.go`, whose `writeBackgroundCompletionSibling` rebuilds it
independently.

**Why not parse the JSONL in the frontend?** Because there would then be
two parsers for Claude's transcript format, and the second one would be
in TypeScript, unversioned against the reader that already exists and
unreachable from every other consumer of these rows. One parser — the
importer's — is what keeps a backgrounded agent's transcript identical to
an imported one, keeps the rows searchable/paged/windowed like every
other row (a frontend parse would produce view-only content SQLite never
sees), and keeps the frontend memory bound to the visible thread (core
principle 4) instead of a whole sidechain JSONL.

### Live subagent progress (`subagent_progress.go`)

A running subagent's counters are LIVE state, never history. Claude
emits a `task_progress` tick after every tool round and Codex a child
`thread/tokenUsage/updated` per child turn; a row per tick would be a
write per round for work the provider already records. Triage therefore
keeps the LATEST tick per launch in a bounded per-thread map, fans it
out on `provider:subagent_progress`, and persists only the FINAL numbers
onto the launch row's `meta.subagentProgress`.

- The map is coordination state in the `pendingApprovals` class, not a
  read model: nothing derives from it, every tick replaces it wholesale,
  and `CleanupThread` / `MarkThreadActive` sweep the thread's entries —
  a replacement process never carries the previous process's tasks.
- A tick MERGES into the stored entry rather than replacing it: a
  provider that cannot report a counter leaves it zero, so a replace
  would blank Claude's tool count on a tick carrying only tokens.
  `ToolUses` / `DurationMs` take the max because they are genuinely
  monotonic; `TotalTokens` takes the LATEST value instead. Codex's is
  cumulative and the two rules agree there, but CLAUDE's is latest-input
  plus all output (`provider.SubagentProgressMeta`), so a subagent that
  compacts its own context legitimately reports a SMALLER number
  afterwards — a max would pin the card to the pre-compaction peak for
  the rest of the run while Claude's own UI moved on. Latest also lets a
  terminal's authoritative usage correct an earlier, larger tick. Zero
  still means "not reported", which is what the guard is for.
- `PeekSubagentProgress` reads without consuming; `TakeSubagentProgress`
  consumes. Only the terminal paths take.
- `persistSubagentFinalProgress(launch, final)` folds the live entry
  under the caller's authoritative `final` and merges the result onto
  the launch row. It is additive and idempotent, so a second terminal
  for the same launch (the `task_notification`'s usage arriving after
  the `task_updated` terminal) merges over what is already persisted
  rather than blanking it.
- **Every terminal that settles a LAUNCH row calls it.** There are four:
  the inline tool completion (`tool_lifecycle.go`
  `persistToolCallCompletion`), the background sibling write
  (`writeBackgroundCompletionSibling`), the Codex spawn-child terminal
  (`codex_background_subagents.go` `markCodexSpawnChildTerminal`), and
  the Claude task notification (`background_task_notifications.go`).
  Every one of them is gated on a live tick EXISTING first, so an
  ordinary tool result costs zero store probes. The inline path is the
  only one that also settles ordinary tools, so it is the only one that
  additionally probes `store.IsSubagentLaunch` — a stray tick keyed on a
  non-launch id is dropped, never persisted. The other three are
  provably launch contexts by their callers.
- Ordering matters on the Codex path: the progress merge runs AFTER that
  path's own `persistItem`, so it lands on the meta that write produced
  instead of being clobbered by it.

### Interactive scope (`resolveInteractiveScope`)

An approval or a structured question raised by a SUBAGENT belongs on the
subagent's card, not the main thread's (`docs/specs/agent-visibility.md`
Q10). `resolveInteractiveScope` is the one rule, and it resolves in
three steps, first non-empty wins:

1. the scope the PARSER already resolved (Claude fills
   `ApprovalRequest.ParentToolUseID` from `can_use_tool`'s `agent_id`
   through its own task map when it can);
2. the event envelope's `ParentToolUseID`;
3. a lookup of the REQUESTED TOOL's own persisted `tool_call` row — its
   `ParentID` is the launch that owns it. This is the fallback for a
   nested agent whose `agent_id` the parser could not map.

Top level stays `""`; an unknown tool id stays `""`. The resolved value
is stored on the pending-approval state, so it reaches the frontend
event, the reconnect snapshot, the synthesized DECLINED row's
`ParentID`, and Codex's synthetic `request_user_input` row — all four
from one resolution, because a question that renders under the wrong
agent on any one of them is the same bug.

### Tray membership vs. lifecycle gates

The background tray and the reaper/queue gates ask different questions
and must not share a filter. `Store.ListLiveBackgroundTasks` lists by
BACKGROUNDED ANCESTRY (every live background launch at any depth, plus
the live agent launches descending from one), while
`HasLiveBackgroundToolCall`, `HasQueueBlockingBackgroundToolCall`,
`CountLiveRunningBackgroundToolCalls`,
`HasRunningTopLevelForegroundToolCall` and
`MarkLiveBackgroundToolCallsInactive` stay `parent_id = ''`. Whether the
tray SHOWS a nested background Bash is a display question; whether that
Bash blocks the flush queue or survives a session teardown is still
answered at the top level only. See invariant 24.

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
  `CommandCompletionPayloadObject`, and the row-id constructors `TextItemID`,
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

Per-thread correlation state exists only to correlate adjacent provider
events; it is not a cache of store or provider-session data. Every field
needs a clear owner and cleanup path.

**Where it lives.** One `map[threadID]*threadState` on the Router
(`thread_state.go`), NOT a new thread-keyed map beside it. `cleanupThread`
sweeps by dropping that one entry, so a field added to `threadState` is
swept the day it is added — by construction, with no per-map delete to
keep manually complete. Add a thread-keyed map to the Router and you
re-open exactly the class of leak the struct exists to close.

State that must SURVIVE cleanup goes on `threadIdentity` instead, and
only for one of the two reasons that type documents: a monotonic
counter whose reset would let a stale captured value match a fresh
session, or a lock whose replacement would split a serialization domain.

Write paths take `r.state(id)` (get-or-create); read paths take
`r.threadStateIfPresent(id)` and treat nil as the zero values the old
per-field maps returned for a missing key. A read path that mints state
leaks an entry per idle thread queried.

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

### Pending-send consumption (`pending_send.go`)

Every AO-initiated user message registers a FIFO entry that its wire
echo consumes. `consumeMatchingPendingSendForEcho` is the ONE rule, and
it has three keys in strict precedence. Do not add a fallback between
them — every mispop this rule has produced came from one.

1. **Client id.** The echo carries `clientId` (Codex's
   `UserMessageItem.client_id`, the `clientUserMessageId` AO dispatched
   with). It consumes the entry whose `AOItemID` equals it, ANYWHERE in
   the queue, and matching nothing consumes nothing. A client id AO does
   not hold is not AO's message.
2. **Provider item id.** No `clientId`, and the head expects an
   `ExpectedProviderItemID` (the uuid AO minted and Claude echoes
   verbatim). Scans for the equal id; no match consumes nothing.
3. **FIFO.** No `clientId`, and the head expects no wire id — a
   pre-identity Codex send. Pops the head. The id-less-echo carve-out
   (an echo with no `provider_item_id` at all, the claude-tui shape)
   also lands here so the downstream diagnostics stay reachable.

`ExpectedClientID` **subtracts from 2 and 3**, and that is the load-bearing
part. An entry that announced its echo will name it is invisible to an
echo that carries no client id: not to the head-pop, not to the
identity scan. Without that, a direct-send echo FIFO-pops a queued entry
still waiting for its own `clientId`, which stamps one message onto the
other's row and leaves the real echo to persist as "Injected provider
context" — two wrong rows from one pop (2026-08-24). It holds whether or
not every sender stamps an id, so it is not a transitional guard.

The expectation is declared at registration
(`PendingSendExpectation{ProviderItemID, ByClientID}`) and `ByClientID`
carries no value: the registrar copies the entry's own `AOItemID`, so
the dispatched `clientUserMessageId` and the expected one cannot drift.
The five `Register*WithExpectation` functions are the whole
registration surface — the pre-identity wrappers are deleted, so a new
send path must state its expectation explicitly (the zero value is the
claude-tui id-less shape, not a default to reach by omission). Each
registrar also stamps exactly one `sendShape`, which is why there are
five and not three: `RegisterPendingSteerSendWithExpectation` and
`RegisterPendingFlushResendWithExpectation` are the direct surface's
shape-carrying siblings, not new behaviour.

### Pending-send transitions (`pending_send_transitions.go`)

A `pendingSend` field is never assigned from outside `pending_send.go` /
`pending_send_transitions.go`. Every mutation another file drives goes
through a named transition whose doc comment states WHEN the transition
is legal and whether `r.mu` must be held, so the registry's invariants
stay readable in one place instead of being reconstructed from the call
sites that happen to write a field.

Two classes, with opposite locking rules:

- **Popped-copy transitions** (`stashEchoIdentity`,
  `recordFirstEchoTurnOccupancy`, `recordEchoPromotedBoundary`,
  `markAnchorRecordedAtEcho`) are methods on `*pendingSend` and mutate
  the copy `consumeMatchingPendingSendForEcho` handed the echo path.
  `r.mu` must NOT be held: no other goroutine can see that copy. The
  copy semantics are load-bearing — an entry reinserted by
  `reinsertPendingSendHead` carries exactly what the echo path stashed,
  and a successfully consumed one carries it nowhere. Do not convert
  these to in-place pointer mutation on the live registry.
- **Registry transitions** (`takeUnconfirmedFlushSendsLocked`, plus the
  interrupt-path ones that stayed in `pending_send.go`) mutate the live
  FIFO through its shared backing array and name their own lock.

`AOItemID` and `Shape` have no transition, deliberately: both are
stamped once by `registerPendingSend` and immutable afterwards, which is
what lets readers trust the shape stamp instead of sniffing the id.

### Send shape is stamped at registration (`send_shape.go`)

`pendingSend.Shape` is the AUTHORITATIVE answer to "is this entry a
queued flush send": readers branch on `Shape == sendShapeFlush`. It is
stamped once, by the registrar the send path chose, and is immutable
afterwards — which is what lets the readers trust it, because both the
stamp and the `AOItemID` are AO-authored in the same
`registerPendingSend` call, so a wire echo can never make them disagree.
(These sites used to sniff the id for `":flush:"` — an id grammar minted
a package away, `nextFlushUserItemID` in `app_flush_queue.go` being the
ONE mint site — and the stamp replaced the sniff after a release-long
comparison soak plus coverage proof that every production registration
site is test-exercised.)

Registration is therefore the whole enforcement surface:
`assertSendShapeMatchesID` panics in any test binary when a registrar's
stamp contradicts its id grammar, and all six production registration
sites are covered by the root suite, so a mis-chosen registrar fails CI
at the surface that chose it rather than misplacing a queued user
message in the timeline.

Two things the id grammar cannot express, and therefore the assertion
cannot check: it only distinguishes flush from not-flush, so
`sendShapeDirect` vs `sendShapeSteer` is stamp-only information, and a
flush row registered through the direct surface (the Codex
post-interrupt re-send) is flush-shaped despite carrying no queue item
id — hence `RegisterPendingFlushResendWithExpectation` rather than a
grammar-inference rule.

### Turn index is allocated once (`turn_lifecycle.go`)

`turns.turn_index` is AO's own allocation and `UNIQUE(thread_id,
turn_index)` enforces it, so two logical turns on one index is
corruption, not a race to be smoothed over. Three rules keep it honest:

- **A foreign turn start never reads the pending-send head.**
  `resolveTurnIndexOnStart`'s source-2 peek is an ATTRIBUTION — the
  dispatcher stamped that index for a message AO sent and is still
  waiting on. A turn start the provider marked `origin: external-queue`
  was dispatched by another producer off the provider's own queue, so it
  allocates past everything known instead (`nextTurnIndexAfterKnown`:
  past the last persisted turn/item AND past every pending send, because
  a deferred row is not in SQLite until its echo). Letting it squat is
  what produced the UNIQUE violation when AO's own echo later opened the
  turn it was promised.
- **One logical turn, two id shapes.** `openQueuedEchoTurn` mints
  `<thread>:<index>`; a Codex wire start mints
  `<thread>:<providerTurnID>` (`store.ScopedTurnID`). Either order is
  reachable, so `upsertTurnRow`'s existence probe asks by turn id AND by
  `(thread, index)`, and a second shape at an occupied index ADOPTS the
  standing row. Settle already resolves turn ids by index
  (`persistedTurnID`), so nothing downstream needs the second id.
- **A real collision relocates, loudly.** Two rows both carrying a
  provider turn id, and different ones, are provably distinct provider
  turns: the incoming one moves to the next free index and both ids are
  logged. `upsertTurnRow` RETURNS the index the row actually occupies
  and callers must seed their open-turn bookkeeping from the return
  value, never from what they asked for — which is why `handleTurnStart`
  writes the row before `setOpenTurn`.

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
recoveries keep the single completed upsert. Live transcript-mirror rows are
also complete snapshots: thinking mounts at its current tail immediately and
never enters the smoother after later tool activity has already arrived.

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
