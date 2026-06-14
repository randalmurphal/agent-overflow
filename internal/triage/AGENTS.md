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
  session state (active wire round, queue, interactive prompts, live todo)
  copied under one router lock for the App transport DTO.
- `tool_lifecycle.go` — tool-call launch/completion rows,
  background-task pairing (Claude), summary/status derivation.
- `codex_background.go` — Codex-specific background projection.
  Tracks unifiedExec commands as transient running-tray state, clears
  typed completions from live state, and persists the normal command row
  using the original item id only while a Codex wire round is active.
  Terminal interactions persist only waited/interacted marker rows while a
  unifiedExec tracker is live. Pending unifiedExec commands are tray-visible
  before a typed wait but only become backgrounded after that wire-typed
  wait signal. Spawn-agent starts are tracker-only; terminal
  spawn completions create the visible transcript row and may later use
  background sibling completion rows. Authorized by the wire-typed signals
  enriched onto Meta in
  `internal/provider/codex/protocol.go` (see invariant 25).
- `terminal_interaction.go` — Codex-specific "Waited for background
  terminal" row persistence. Handles `EventTerminalInteraction` for
  the empty-stdin (polling) variant emitted when the model calls
  `write_stdin` against a backgrounded unified-exec PTY. Non-empty
  stdin persists an "Interacted with background terminal" marker without
  storing stdin bytes.
- `tool_result_file_change.go` — `file_change` tool-result normalisation
  (inline diff projection, unified patch assembly).
- `tool_paths.go` — per-turn agent-touched-path tracking. Extracts paths
  from Claude `Edit`/`Write`/`MultiEdit`/`NotebookEdit` tool args and
  Codex `fileChange` items, normalises to workspace-relative form, and
  persists them to `thread_tracked_files`. Message checkpoints are
  captured before user sends in `app_checkpoint.go`; the tracked-path
  table scopes conversation-and-files revert and agent-only diff
  previews. Bash side effects are intentionally untracked.
- `tool_result_diff_upgrade.go` — late-arriving diff upgrades that
  attach a richer payload onto a previously persisted tool result.
- `command_inline_diff_capture.go` / `command_inline_diff_parser.go` /
  `command_inline_diff_runtime.go` / `command_inline_diff_persist.go` —
  command-execution inline-diff pipeline, split by phase
  (capture → parse → runtime match → persist).
- `payload_items.go` — diff / command output / thinking / plan payload
  writers.
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
  `provider:turn_started` / `provider:turn_completed` /
  `provider:subagent_notification`, plus the canonical stop-reason
  normaliser.
- `meta.go` — shared JSON-inspection helpers.
- `maps.go` — generic map utilities (currently just `deleteByPrefix`).

## Routing table

| Event kind | Destination |
|---|---|
| Text delta | Frontend (passthrough). |
| Tool-use start/complete | Frontend event + item in SQLite on completion. |
| Approval request | Frontend event with `request_id` preserved. |
| Diff | SQLite payload + meta to frontend. Full diff is on-demand. |
| Command output | SQLite payload + meta to frontend. |
| Thinking block | SQLite payload + preview to frontend. |
| Thinking block w/ `CompactionReasoningScope` | Top-level `compaction_reasoning` streaming row (the live "compact" tail above the divider). Reuses thinking streaming machinery; dispatched ahead of `handleThinking` / `handleContentBlockStop`. See `compaction_reasoning.go`. |
| Compaction boundary | `compaction` divider row + on-demand summary payload (`usage_compaction.go`). |
| Turn metadata (cost/tokens) | Persist on turn completion. |
| Context-window usage | Frontend context meter + `threads.last_token_usage`. |
| Background task terminal (Claude) | `tool_completion` sibling row upsert (idempotent). See `turn-lifecycle.md`. |
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
  known instead of keeping it in a router map.

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
stream persistence buffer. Do not split streaming text across separate UI
event channels, and do not add another rendered cache column or a
server-side kind-to-renderer dispatch table.

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
