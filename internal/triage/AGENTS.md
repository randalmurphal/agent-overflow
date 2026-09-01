# internal/triage/

Classifies provider events and decides what ships to the frontend vs
what writes to SQLite. Triage has **no derived state**: a handler is a
pure function of the current event plus a narrow, bounded set of
per-thread correlation state.

Mechanism and rationale live in docs, to read before changing routing:
[`triage-routing.md`](../../docs/architecture/triage-routing.md) (the
`EventKind` to handler table),
[`turn-lifecycle.md`](../../docs/architecture/turn-lifecycle.md) (the
turn / tool / task lifecycles, task notifications, backgrounding,
subagent progress, error routing),
[`agent-visibility.md`](../../docs/specs/agent-visibility.md) (the
subagent surface spec),
[`invariants.md`](../../docs/architecture/invariants.md) for every
numbered invariant below, and
[`data-flow.md`](../../docs/architecture/data-flow.md) for the pipeline
around this package.

New routing logic goes in the file whose concern it matches, and `ls` is
the file list. A `Handle` case in `router.go` makes a handler reachable.

## Rules you hit first

- **One event in, zero or more (persist + emit) decisions out.** Never
  combine or split events across handler boundaries, and log an unknown
  event with full context rather than dropping it.
- **`task_notification` is an attention signal, never a completion
  source** (invariant 21).
- **Turn activity on the frontend is wire-pushed only** (invariant 22),
  never inferred from session liveness probes.
- **Codex backgrounding follows wire-typed signals, never heuristics**
  (invariant 25), enriched onto Meta in `provider/codex/protocol.go`.
- **Live-only channels persist nothing, and absence is not a negative
  answer.** `provider:fast_mode`, `provider:commands`,
  `provider:compacting`, `provider:command_lifecycle`,
  `provider:subagent_progress` and `provider:background_tasks_changed`
  emit only what the wire carried, because an empty frame renders as a
  denial. An empty set on the wire is a real answer.
- **A re-round path (`maybeReopenSettledRound`) must not call
  `setOpenTurn`.** That resets id-allocating counters and collides with
  rows already persisted under the same logical turn.
- **Provider-specific types stop at the package boundary** (invariant
  14). If the normalized event lacks a detail, fix it upstream.
- **Persist raw content only.** No markdown, ANSI, Mermaid, KaTeX or
  code-block rendering here, no rendered cache column, no server-side
  kind-to-renderer dispatch. The frontend renders; it knows what is
  mounted.

## Subagent scope stays subagent scope

A subagent's events never reach the parent thread's timeline or its
context accounting. Everything a subagent produces is written under its
launch row, on the launch's `turn_index` (invariant 10), and none of it
moves a thread-level number. This is a recurring bug class: treat a new
subagent-aware path as guilty until it proves scope containment.

- Scoped `token_usage` is dropped (`handleTokenUsage` returns early on a
  non-empty `ParentToolUseID`). The context meter is the main agent's.
- A scoped compact boundary persists a compaction row under the launch
  and nothing else (`persistSubagentCompaction`): no compacting-window
  close, no usage-throttle reset, no context-window write.
- A replayed subagent error persists as `api_error` under the launch with
  `fatal:false` (`replayedErrorMeta`). Dispatched live it would flip the
  thread's current turn to errored for an error the parent never saw.
- Progress ticks are live only, merged into a bounded per-thread map and
  fanned out on `provider:subagent_progress`. Only a launch terminal
  persists, through `persistSubagentFinalProgress`, which folds rather
  than assigns so two terminals in any order land the same numbers.
- The notification row is the thread's bell, and a bell fires for
  top-level launches only, so a launch with a `ParentID` writes none
  (`writeBell` gates, `persistBell` writes). A watch task is exempt at
  any depth: its notification rows are event history, not a bell.
- An approval or a question raised by a subagent renders on the
  subagent's card. `resolveInteractiveScope` (`approvals.go`) is the one
  rule, first non-empty wins: the scope the parser resolved, the event
  envelope's `ParentToolUseID`, then the requested tool's persisted
  `tool_call` row `ParentID`. Unresolved stays `""`, which is top level
  and the honest record. That one resolution feeds the frontend event,
  the reconnect snapshot, the declined row and Codex's synthetic
  `request_user_input` row, so all four agree.
- Tray membership and lifecycle gates must not share a filter.
  `Store.ListLiveBackgroundTasks` lists by backgrounded ancestry at any
  depth, while the reaper and queue gates beside it in
  `store/items_lifecycle.go` stay `parent_id = ''`. Listing a nested
  background Bash — in the tray or in the cross-thread inventory — is a
  membership question; whether it blocks the flush queue is top-level
  only (invariant 24). Settlement is NOT top-level:
  `SettleBackgroundLaunchesForSessionEnd` (session close and death)
  and the boot sweep settle launches at any depth, because the gates'
  exemption is exactly what used to leave nested rows ticking forever.
- A `system/task_started` meta update can precede its launch row —
  subagent-owned shells announce on the main wire before the owner's
  transcript projection persists the row. `persistToolCallLaunch`
  holds the correlation fields (`pendingToolCorrelations`, bounded,
  swept with the threadState) and applies them when the row lands,
  draining any terminal stashed in the meantime. Never drop a
  correlation-bearing meta update just because the row is missing.

## Stopped-thread routing (invariant 29)

`CleanupThread` marks the thread stopped and `Handle` then drops every
wire event for it, `EventInit` included. Only the host's session-start
funnel clears the marker, by calling `MarkThreadActive` pre-spawn
(`app_session.go`): a replacement session that dies during startup emits
its only diagnostics pre-init, so no wire event may clear it.

Host-synthesized events (send-failure synthetic turn-completes,
`emitErrorToThread`) route through `HandleSynthetic`, the one carve-out
that bypasses the gate. An error a wire event triggers on the read loop
uses `emitWireErrorToThread` and stays gated, as do approval and
user-input resolutions.

`MarkThreadActive` also clears the thread's `settledTurns` prefix and
bumps the reactivation epoch. Async teardowns capture `ThreadEpoch`
before unregistering their session and clean up via
`CleanupThreadIfEpoch`, which no-ops once a replacement start bumped the
epoch. Epoch entries are never deleted, since a reset would let a stale
captured zero match.

## Correlation state is bounded, never derived

Per-thread state correlates adjacent provider events. It is not a cache
of store or provider-session data, and cross-turn derivation belongs in
the frontend or a persisted projection.

**Where it lives.** One `map[threadID]*threadState` on the Router
(`thread_state.go`), never a new thread-keyed map beside it, which
re-opens the leak class the struct exists to close (invariant 17).
`cleanupThread` drops that one entry, so a field added to `threadState`
is swept the day it is added. State that must survive cleanup goes on
`threadIdentity`, and only for the two reasons that type documents.
Write paths take `r.state(id)` (get-or-create); read paths take
`r.threadStateIfPresent(id)` and treat nil as the zero value, because a
read path that mints state leaks an entry per idle thread queried.

Categories, for placing new state:

- **Per-turn flow control** (open turn, streaming block flags, approvals,
  user inputs, pending inline diffs): cleaned in `clearOpenTurn` or by
  the correlated resolver.
- **Id-allocating counters** (`segmentIndexByScope`, `blockIndexByScope`,
  `errorSeqByScope`, `terminalInteractionSeq`) allocate thread-lifetime
  `items.id` values, so they clean in `CleanupThread`, not per turn.
- **Logical-turn settlement** (`settledTurns`): survives wire-round
  boundaries, reset by a fresh `setOpenTurn`.
- **Durable user-visible state**: persist it as soon as it is known. The
  activity-rail todo list lives on `threads.live_todo`, and the
  `tasksByThread` correlation map beside it is re-seeded from that column
  (`seedTasksFromStoredTodo`) before a `Task*` event applies, because a
  resumed session updates ids minted before the restart. An all-completed
  list seeds nothing, and app paths restarting a session from scratch on
  the same row call `ResetThreadTodo`.

A new map holding user-blocking live state goes into `HasPendingWork`
(`interactive_requests.go`) with matching test coverage.

**Answering an open question is arbitrated** (`interactive_claim.go`).
One backend serves several clients, each rendering the same approval
prompt and the same structured-input form, so two of them can answer
within the same second. `ClaimApprovalResponse` /
`ClaimUserInputResponse` let the first answer through and refuse the
rest; the App turns a refusal into `transport.ErrAlreadyHandled`, which
crosses the wire as a code rather than as redacted prose. Three rules
hold it together, and each one is a defect if dropped:

- It refuses only on POSITIVE evidence — an answer this router
  forwarded. A request it has no record of passes through, because the
  router's pending map is not the only authority on what is answerable
  (a Codex session keeps its own request table) and reporting "someone
  else answered" about a request nobody answered is a worse report than
  the one this closes.
- It does not consume the pending entry. `handleApprovalResolved` reads
  that entry to build the resolved tool row's summary; taking it here
  leaves the row describing the wrong input.
- A write that never reached the provider must
  `ReleaseInteractiveResponse`, or the prompt is wedged open with
  nobody — including the client that just failed — able to answer it.

## Pending sends

Every AO-initiated user message registers a FIFO entry that its wire
echo consumes. `consumeMatchingPendingSendForEcho` (`pending_send.go`)
is the one rule, with keys in strict precedence. Do not add a fallback
between them: every mispop this rule has produced came from one.

1. **Client id.** An echo carrying `clientId` consumes the entry whose
   `AOItemID` equals it, anywhere in the queue. Matching nothing consumes
   nothing, because a client id AO does not hold is not AO's message.
2. **Provider item id.** No `clientId`, and the head expects an
   `ExpectedProviderItemID`: scan for the equal id, and no match consumes
   nothing.
3. **FIFO.** No `clientId`, and the head expects no wire id: pop the
   head. An id-less echo (claude-tui) lands here too, so its diagnostics
   stay reachable.

`ExpectedClientID` subtracts from 2 and 3: an entry that announced its
echo will name it is invisible to an echo carrying no client id. Without
that, a direct-send echo pops a queued entry still waiting for its own
`clientId`, stamping one message onto the other's row and leaving the
real echo to persist as "Injected provider context".

The five `Register*WithExpectation` functions are the whole registration
surface, so a new send path states its
`PendingSendExpectation{ProviderItemID, ByClientID}` explicitly rather
than reaching a default by omission. Each also stamps the entry's
`sendShape` (`send_shape.go`): readers answer "is this a queued flush
send" with `Shape == sendShapeFlush`, never by sniffing the id for
`":flush:"`, and `assertSendShapeMatchesID` panics in a test binary when
a stamp contradicts the App layer's id grammar.

Every mutation goes through a named transition
(`pending_send_transitions.go`), whose doc comment states when it is
legal and which lock it needs. Nothing else writes a `pendingSend`
field, and `AOItemID` and `Shape` have no transition at all. Popped-copy
transitions mutate the copy the echo path was handed, so `r.mu` must NOT
be held and they must not become in-place mutation on the live registry.

## Turn index is allocated once (`turn_lifecycle.go`)

`turns.turn_index` is AO's own allocation and `UNIQUE(thread_id,
turn_index)` enforces it, so two logical turns on one index is
corruption, not a race to smooth over.

- **A foreign turn start never reads the pending-send head.**
  `resolveTurnIndexOnStart`'s peek is an attribution for a message AO
  sent and is still waiting on. A start marked `origin: external-queue`
  came off the provider's own queue, so it allocates past everything
  known (`nextTurnIndexAfterKnown`: past the last persisted turn and
  item, and past every pending send, whose deferred row is not in SQLite
  until its echo).
- **One logical turn, two id shapes.** `openQueuedEchoTurn` mints
  `<thread>:<index>` and a Codex wire start mints
  `<thread>:<providerTurnID>`, so `upsertTurnRow` probes by turn id AND
  by `(thread, index)`, adopts the standing row when a second shape hits
  an occupied index, and relocates loudly when two different provider
  turn ids collide.
- **Callers seed open-turn bookkeeping from `upsertTurnRow`'s return
  value**, the index the row occupies, which is why `handleTurnStart`
  writes the row before `setOpenTurn`.

## Streaming settlement and the interrupt queue

Async streaming settlement is deliberately off the provider read loop.
Keep the synchronous state flip under `r.mu`, and keep the counter
decrement plus the interrupt-queue drain inside the settle goroutine so
the `0 -> drain` transition happens after SQLite has the row
(`stream_state.go`, `multi_result_test.go`).

Two counters move in lockstep through `incStreamingCounts` /
`decStreamingCounts`: thread-wide `streamingItemCount` gates the
interrupt-queue drain, and per-scope `streamingScopeCounts` gates the
queue decision, so a new mid-stream row defers only behind a same-scope
stream (invariant 11). A new streaming block kind bumps both through
those helpers, and emits every timeline mutation on the ordered
`provider:item_event` channel rather than a UI channel of its own.

## Exported shape surface

`internal/sessionimport` replays historical provider sessions into
SQLite without driving `Router`, whose live-only side effects would
persist imported prompts as "Injected provider context". Sharing the
shaping helpers (`shape.go` plus the pure exports beside their lifecycle
twins, and `InterruptedSummary` for the mid-turn fork settle) is the only
thing keeping an imported thread from rendering like a different app.

- These stay pure: no Router receiver, no store access, no clock, no
  logging. A helper needing correlation state belongs in the lifecycle
  file owning that state, and only its pure core belongs here.
- Changing an id format or a summary or status rule changes both writers
  at once. `internal/sessionimport/parity_test.go` drives one synthetic
  sequence through both and asserts identical rows.
- Do not branch a shared helper for import. If import needs a different
  row, that is a routing decision and belongs in the importer.

## Subagent transcript backfill (`subagent_transcript.go`)

An agent backgrounded mid-flight streams nothing after the cut, so its
work exists only in the sidechain JSONL that `task_notification` names as
`output_file`. Triage reconciles every agent launch at its terminal
through `claude/sessionimport.ConvertSubagentTranscriptData`.

- **Replay through triage's own persist paths.** Handing the events to
  the importer's writer is a package cycle, and the two writers allocate
  subagent-scoped row ids differently, so those rows would be invisible
  to every later live lookup.
- **Dedupe keys on the identity both writers spell identically:** the
  `tool_use_id` for tool rows (a completion also requires the row to have
  left `running`), `items.meta.provider_item_id` for text, thinking and
  the agent's own prompt row, and the provider boundary UUID for a
  compaction. A compaction is reconciled by that UUID independently of
  the cut: the live mirror compaction tap (claude
  `parse_transcript_mirror.go`) normally lands it mid-run at its real
  position, and stdout omits it either way, so its absence proves
  nothing about where streaming stopped. Errors and command results are
  undecidable, so `subagentBackfillCut` replays the whole tail from the
  first decidable and missing event, carrying the undecidable ones with
  it.
- **Text and thinking bypass `handleTextDelta` / `handleThinking`**,
  which would open a streaming block and wait for a stop the transcript
  has no event for, wedging the interrupt queue behind a count that
  never decrements.
- **Failure is loud.** An unresolvable, oversized, or unprojectable file
  surfaces on the `outputState = "error"` path with its reason, since a
  silently incomplete transcript reads like a complete one.

## Codex collab activity rows (`codex_background_*.go`)

The spawn row is the immutable historical event that the child launched,
and later collaboration never appends presentation state to it: a
`send_message` / `followup_task` completion and a `MESSAGE` mailbox
delivery each persist as their own top-level `send_input` row, and child
resume status creates no row at all. Hidden operational metadata may
still be patched onto the spawn row until the child settles, since there
is no separate persisted background row. Mailbox row identity mixes the
child's durable resume generation into a content hash, so identity is
never rebuilt from an in-memory counter (`codexMailboxCompletionID`).

- `SetAssistantTextStreamObserver` — a streaming assistant_text row's
  full accumulated summary at each persistence flush window
  (final=false) and once at settle with the final model text
  (final=true). Backs the remote-client highlight seed push
  (`app_highlight.go` / `internal/highlightapp`).
- `SetDiffPayloadObserver` — a just-persisted diff-bearing payload with
  COMPLETE content: tool results (`persistToolResult` and the
  summary_only→exact upgrade) carry payloadID + preview patches + the
  full unified patch; diff-kind payload full writes carry payloadID +
  patch (the append branch never notifies — its content is a delta).
  Backs highlight span persistence (`payloads.preview_spans` /
  `payloads.spans` columns) and the remote diff seed push
  (`app_highlight.go` / `internal/highlightapp`).

`newTriageRouter` (`app_flush_queue.go`) is the one construction site,
and it wires `SetAssistantTextStreamObserver` (accumulated summary per
flush window, then the final text at settle), `SetDiffPayloadObserver`
(a just-persisted diff payload with complete content, so the append
branch never notifies) and `SetCodeSpanEnricher` (`code_spans.go`, fence
spans merged into `items.meta` under `codeSpans`). All may fire on the
read loop, so they must not block. Observers are taps, never routing
decisions, and the enricher stays a pure function of the text.

## Extension points

- New event kind: pick or create the matching file, add the `Handle`
  switch case in `router.go`, and write the routing-decision test first.
  See [`how-to.md`](../../docs/architecture/how-to.md#add-a-new-event-kind).
- New persisted payload kind: extend `payload_items.go` and update
  [`schema.md`](../../docs/architecture/schema.md). Preview and stats go
  in `meta`, full content in `data`.
