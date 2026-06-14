package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// handleTurnStart opens the per-turn span, seeds bookkeeping, writes the
// `turns` row for this turn (completed_at NULL), and emits
// `provider:turn_started` so the frontend flips `pane.activeTurn` on.
//
// Wire-round vs logical-turn cadence: this handler is the canonical
// start of round 1 of every logical turn. It both seeds per-turn
// flow-control state (setOpenTurn) AND opens a fresh wire round
// (setOpenRound). The `provider:turn_started` payload carries the
// per-round id as TurnID — the frontend treats each round as its own
// active turn for indicator / composer / Stop-button purposes.
// Subsequent rounds within the same logical turn are opened in
// handleInit's re-round branch, which only calls setOpenRound (not
// setOpenTurn) so id-allocating counters survive the multi-result-
// per-turn cascade. See internal/triage/AGENTS.md "Wire-round vs
// logical-turn".
func (r *Router) handleTurnStart(evt provider.ProviderEvent) error {
	turnIndex := r.resolveTurnIndexOnStart(evt)
	r.setOpenTurn(evt.ThreadID, turnIndex)
	r.openTurnSpan(evt, turnIndex)

	startedAt := eventTimestampMillis(evt)
	turnID := resolveTurnID(evt, turnIndex)
	r.upsertTurnRow(store.Turn{
		TurnID:    turnID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	})

	// Open the first wire round for this logical turn. Frontend
	// idempotency keys on TurnID, so a fresh roundID per round means a
	// duplicate EventTurnStart for the same logical turn (Codex retry,
	// Claude system.init replay before any complete) replaces the prior
	// active-turn entry rather than accumulating stale state.
	snapshot := ActiveTurnSnapshot{
		ThreadID:  evt.ThreadID,
		TurnID:    uuid.NewString(),
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	}
	r.setOpenRoundSnapshot(snapshot)
	r.emit("provider:turn_started", TurnStartedEvent(snapshot))
	return nil
}

// resolveTurnIndexOnStart picks the right turn index for an incoming
// EventTurnStart / EventInit-promoted-to-start. Three sources, in
// priority order:
//
//  1. evt.TurnIndex when the wire carries it directly (Codex
//     turn/started, recovery replays).
//
//  2. The pending-send FIFO head when the wire carries no index but
//     an AO send is awaiting echo. The dispatcher
//     (RegisterPendingSend / RegisterPendingFlushSend) stamps the
//     AO-decided turn index BEFORE the provider write, so the FIFO
//     head carries the authoritative answer for the new turn. The
//     peek is non-destructive — the pop is owned by handleUserText
//     when the matching user_text echo arrives.
//
//  3. store.LastTurnIndex as the no-pending-send fallback (idle
//     session attach via EventInit; tests that exercise this handler
//     directly).
//
// For queue dispatches, source 3 returns the PREVIOUS turn because
// the deferred user_text for the new turn has not been persisted yet
// (handleUserText persists on echo, which lands AFTER system.init).
// Resolving on source 2 first is what stops setOpenTurn from
// re-wiping the previous turn's id-allocating counters and silently
// overwriting its trailing text via UpsertItem. See
// queue_dispatch_turn_test.go.
func (r *Router) resolveTurnIndexOnStart(evt provider.ProviderEvent) int {
	if evt.TurnIndex != 0 {
		return evt.TurnIndex
	}
	if pending, ok := r.peekPendingSendHead(evt.ThreadID); ok {
		return pending.TurnIndex
	}
	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		log.Printf("triage: last turn index on turn start: %v", err)
		return 0
	}
	return turnIndex
}

// resolveTurnID builds the `turns` table primary key for the incoming
// turn. Codex fills evt.TurnID directly from `turn/started`; Claude
// has no wire-level turn_id so triage synthesizes one from
// `<threadID>:<turnIndex>`. Both forms are deterministic so a re-sent
// EventTurnStart for the same (thread, turn) maps back to the same row
// (the store upsert path then skips a redundant insert).
func resolveTurnID(evt provider.ProviderEvent, turnIndex int) string {
	if id := strings.TrimSpace(evt.TurnID); id != "" {
		return id
	}
	return fmt.Sprintf("%s:%d", evt.ThreadID, turnIndex)
}

// upsertTurnRow inserts the turns row for a freshly-opened turn or
// preserves the existing row if the provider re-sends EventTurnStart.
// An existing row means we've seen this turn before (re-init after
// interrupt, recovery replay) and the original started_at is the
// authoritative wall time — don't overwrite it.
// Errors are logged but not propagated: turn-start emission must fire
// even if persistence hiccups, so the frontend's working indicator
// still tracks the wire signal.
func (r *Router) upsertTurnRow(turn store.Turn) {
	_, found, err := r.store.GetTurn(turn.TurnID)
	if err != nil {
		log.Printf("triage: turn start get %s: %v", turn.TurnID, err)
	}
	if found {
		// A re-sent EventTurnStart for the same (thread, turn). The
		// existing row is the authoritative one — preserve started_at
		// and completed_at. Refreshing started_at would show a
		// later clock on the working indicator each time the
		// provider re-initialises.
		return
	}
	if err := r.store.InsertTurn(turn); err != nil {
		log.Printf("triage: turn start insert %s: %v", turn.TurnID, err)
	}
}

// openTurnSpan begins a turn.lifecycle span for the incoming turn. Any
// existing span for the thread is closed first — the provider sometimes
// re-sends EventTurnStart (e.g. after a Claude interrupt/re-init) and we
// don't want to leak orphan spans. turnIndex is the value the caller has
// already resolved via resolveTurnIndexOnStart so the span's `turn.index`
// attribute matches what setOpenTurn / upsertTurnRow wrote; querying
// LastTurnIndex here would diverge for queue-dispatched turns where the
// deferred user_text hasn't been persisted yet.
func (r *Router) openTurnSpan(evt provider.ProviderEvent, turnIndex int) {
	r.mu.Lock()
	tracer := r.tracer
	if existing, ok := r.turnSpans[evt.ThreadID]; ok {
		delete(r.turnSpans, evt.ThreadID)
		r.mu.Unlock()
		existing.End()
		r.mu.Lock()
	}
	thread, err := r.store.GetThread(evt.ThreadID)
	r.mu.Unlock()
	if err != nil {
		// We don't know the provider/model without the thread; drop the
		// span rather than record misleading attributes.
		return
	}
	_, span := tracer.Start(context.Background(), "turn.lifecycle",
		trace.WithAttributes(
			attribute.String("thread.id", evt.ThreadID),
			attribute.String("provider", thread.Provider),
			attribute.String("model", thread.Model),
			attribute.Int("turn.index", turnIndex),
		),
	)
	r.mu.Lock()
	r.turnSpans[evt.ThreadID] = span
	r.mu.Unlock()
	r.metrics.TurnsStarted.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("provider", thread.Provider),
		),
	)
}

func (r *Router) handleTurnComplete(evt provider.ProviderEvent) error {
	flushQueueAtBoundary := true
	defer func() {
		if flushQueueAtBoundary {
			r.maybeFlushQueueAtBoundary(evt.ThreadID)
		}
	}()

	// Two-cadence shape (see internal/triage/AGENTS.md "Wire-round vs
	// logical-turn"):
	//
	//   1. Per-WIRE-ROUND emission (top of this handler).
	//      `provider:turn_completed` fires once per `result` envelope so
	//      the frontend's working indicator, Stop button, and composer
	//      block correctly reflect "model is engaged right now."
	//      takeOpenRound clears the round slot in the same critical
	//      section it returns it; an empty slot means a synthetic
	//      complete already raced ahead (handleError fatal path) — skip
	//      the second emit so the frontend sees exactly one
	//      turn_completed per round.
	//
	//   2. Per-LOGICAL-TURN persistence (claimTurnSettlement-gated below).
	//      The `turns` row UPDATE, streaming-item settlement, and
	//      force-close-orphans pass run once per
	//      (thread, turn_index). A second wire complete for the same
	//      logical turn — the multi-result-per-turn pattern, where
	//      Claude's CLI synthesizes a `type:"user"` envelope from a
	//      task_notification and emits another `result` — folds late
	//      token usage onto the existing turns row and otherwise
	//      no-ops.
	//
	// Known sources of a second wire complete on an already-settled
	// logical turn:
	//
	//   1. Claude task_notification → CLI synthesizes a `type:"user"`
	//      envelope → model emits another response → second `result`
	//      envelope. Both `result`s belong to one logical agent-overflow
	//      turn; persistence settles on the first. The wire-round emit
	//      at the top fires for the second too (each round gets its
	//      own indicator on/off).
	//   2. handleError synthesizes an EventTurnComplete with TruncatedTurnCompleteMeta,
	//      then a real wire EventTurnComplete arrives anyway because the
	//      subprocess kept streaming. takeOpenRound returns "" on the
	//      second, so the wire-round emit is suppressed.
	//
	countsAsActivity := turnCountsAsThreadActivity(evt)
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	meta, metaErr := turnCompleteMetaFromEvent(evt)
	if metaErr != nil {
		log.Printf("triage: %v", metaErr)
		if err == nil {
			err = metaErr
		}
	}
	now := eventTimestampMillis(evt)

	// Per-round emission. Always runs before the claimTurnSettlement gate
	// so a second `result` envelope for the same logical turn still
	// emits the round-end signal. takeOpenRound is a read-and-clear:
	// at most one emit per round, regardless of synthesis races.
	round, hasRound := r.takeOpenRound(evt.ThreadID)
	if hasRound && err == nil {
		r.emit("provider:turn_completed", r.buildRoundCompletedEvent(evt, round.TurnID, turnIndex, now, meta))
	}

	// Orphan error result: an error turn-complete with no open round, no
	// open turn, and no prior settlement means the provider failed before
	// the turn machinery ever engaged — e.g. Claude dying pre-init when
	// its --resume-session-at cursor is unusable (the resulting
	// result{error_during_execution} is the only wire output such a
	// session ever produces). There is no round to emit and no turns row
	// to settle, so without this branch the error reaches nothing the
	// user can see: persist a system error item instead. The
	// provider:item_event upsert it emits is the surface the frontend
	// already clears its optimistic pending-send "Working" state on.
	//
	// The isTurnSettled guard — checked against the event's NATURAL turn
	// (currentTurnIndex fallback) — keeps the legitimate late-fold case
	// flowing past this branch: a trailing result{is_error} after a soft
	// close settled the turn must reach persistLateTurnPayload below, not
	// mint an error item. The attributed turn prefers the pending-send
	// head when one exists (the dispatcher-stamped index for the send
	// that provoked the failing start; deferred flush items are not in
	// SQLite yet, so LastTurnIndex would point one turn early). The head
	// is NOT consumed — CleanupThread or the wire user echo owns the pop.
	if err == nil && meta.Error != "" && !hasRound && !r.isTurnSettled(evt.ThreadID, turnIndex) {
		if _, open := r.openTurnIndex(evt.ThreadID); !open {
			attributedTurn := turnIndex
			if head, ok := r.peekPendingSendHead(evt.ThreadID); ok {
				attributedTurn = head.TurnIndex
			}
			// A session that just reported a hard failure cannot serve
			// queued sends — leave the flush queue for the next session.
			flushQueueAtBoundary = false
			return r.persistProviderErrorItem(evt.ThreadID, attributedTurn, meta.Error, nil, "", "", now)
		}
	}

	if err == nil && !countsAsActivity {
		return nil
	}

	if err == nil {
		// claimTurnSettlement is sticky on partial failure (same trade-off
		// as markTurnCaptured): once the FIRST handleTurnComplete reaches
		// here, subsequent invocations for the same (thread, turn) skip
		// lifecycle settlement. Late usage on a duplicate is still folded
		// into the existing turn row below so accounting is not lost.
		if !r.claimTurnSettlement(evt.ThreadID, turnIndex) {
			r.persistLateTurnPayload(evt, turnIndex, meta)
			return nil
		}
	}
	var persistErr error
	truncated := turnCompleteIsTruncated(meta)
	if truncated {
		flushQueueAtBoundary = false
	}
	if err != nil {
		persistErr = err
	} else {
		// On truncation, drain the interrupt queue as errored BEFORE
		// settling the streaming items. settleTurnStreaming fires
		// drainInterruptQueueIfIdle on the last streaming close, which
		// would otherwise persist the queued items as-is (status
		// completed, no " — interrupted" suffix). Draining first means
		// the subsequent idle-drain finds an empty queue and the
		// queued rows correctly reflect the interrupted turn. For
		// non-truncated completions we keep the original order —
		// settle → (idle-drain persists normally) → forced drain no-op.
		if truncated {
			if err := r.drainInterruptQueue(evt.ThreadID, true); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil {
			status := statusCompleted
			if truncated {
				status = statusErrored
			}
			if err := r.settleTurnStreaming(evt.ThreadID, turnIndex, status); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil && truncated {
			if err := r.markTurnItemsErrored(evt.ThreadID, turnIndex, now); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil {
			// For the non-truncated path this drains any queued events
			// that settled outside settleTurnStreaming's idle-drain
			// window (rare but possible). For the truncated path the
			// queue is already empty so this is a cheap no-op.
			if err := r.drainInterruptQueue(evt.ThreadID, truncated); err != nil {
				persistErr = err
			}
		}
	}

	// Codex background-terminal turn-boundary cleanup. Turn completion settles
	// visible terminal-wait carriers and drops pending spawn-agent starts that
	// never reached a terminal spawn completion. UnifiedExec trackers stay
	// transient after turn close because Codex `Op::Interrupt` does not kill
	// the PTY; typed item/completed later clears the live tracker, and only
	// persists transcript history if a Codex wire round is active at that time.
	if persistErr == nil && err == nil {
		r.observeCodexTurnComplete(evt.ThreadID)
	}

	// Invariant 23: force-close orphan running non-background tool_calls.
	// Backgrounded launches are exempt (invariant 24). Runs after the
	// truncation flip so we don't double-flip items that
	// markTurnItemsErrored already settled — the check targets status=
	// running tool_calls only, which persistItem-via-markTurnItemsErrored
	// has already moved to status=errored in the truncated path.
	if persistErr == nil && err == nil {
		if fcErr := r.forceCloseOrphanToolCalls(evt.ThreadID, turnIndex, now); fcErr != nil {
			persistErr = fcErr
		}
	}

	// Settle the turns row even on persist error so the persisted
	// `completed_at` reflects whatever clock we have at the point the
	// wire said the turn was over. Persistence failures are logged
	// inside settleTurnRow; we keep persistErr as the return so
	// upstream (observability, error reporting) sees the real failure.
	// The frontend's working indicator was already cleared by the
	// per-round `provider:turn_completed` emission at the top of this
	// handler.
	//
	// Checkpoint capture does NOT happen here. Message checkpoints are
	// captured by app_send.go before provider stdin/RPC dispatch so the
	// snapshot maps directly to "before this user message".
	r.settleTurnRow(evt, turnIndex, now, meta, persistErr)

	r.clearOpenTurn(evt.ThreadID)
	r.closeTurnSpan(evt.ThreadID, persistErr)
	r.FlushUsageEmitThrottle(evt.ThreadID)

	// Opportunistic WAL passive checkpoint at the idle boundary. PASSIVE
	// is non-blocking: it reclaims whatever pages the WAL can free
	// without stalling readers, and skips the rest. We only fire it
	// when this thread has no remaining streaming items — the counter
	// is the freshest signal that "the stream burst just ended" — and
	// we run the actual PRAGMA on a goroutine so we never block the
	// provider read-loop on its syscall. The autocheckpoint at 1000
	// pages is still the steady-state mechanism; this is an extra
	// nudge at the natural quiet point.
	r.mu.Lock()
	thisThreadIdle := r.streamingItemCounts[evt.ThreadID] == 0
	r.mu.Unlock()
	if thisThreadIdle {
		threadID := evt.ThreadID
		go func() {
			if cpErr := r.store.PassiveCheckpoint(); cpErr != nil {
				log.Printf("triage: passive checkpoint after turn complete (%s): %v", threadID, cpErr)
			}
		}()
	}

	return persistErr
}

// turnCompleteIsTruncated reports whether the turn ended via interruption /
// truncation.
func turnCompleteIsTruncated(meta turnCompleteMeta) bool {
	return meta.Truncated || meta.Aborted
}

// forceCloseOrphanToolCalls flips every status=running +
// is_background=0 tool_call row in (threadID, turnIndex) to errored
// with a synthesized completion summary. This is the safety net for
// invariant 23: the provider/parser can drop a tool_result and leave a
// row stuck at running; a clean turn-complete should settle the
// timeline regardless. Backgrounded launches are exempt (invariant 24
// — they legitimately outlive their turn); the exemption is pushed
// into the SQL accessor so we don't deserialize settled or
// already-exempt rows at all.
//
// The DB writes are batched inside store.ForceCloseRunningToolCallsInTurn
// (one TX, one thread-touch) to cut per-orphan roundtrips; the
// frontend-bound `provider:item_event` upserts stay per-row so the
// UI still updates each card independently.
func (r *Router) forceCloseOrphanToolCalls(threadID string, turnIndex int, now int64) error {
	flipped, err := r.store.ForceCloseRunningToolCallsInTurn(threadID, turnIndex, forceCloseSummary, now)
	if err != nil {
		return fmt.Errorf("force-close orphan tool calls: %w", err)
	}
	for _, item := range flipped {
		r.emitItemUpsert(item)
		r.metrics.ItemsPersisted.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("kind", item.Kind)))
	}
	return nil
}

const forceCloseSuffix = " — turn ended with tool unresolved"

// forceCloseSummary adds the force-close marker to the tool_call's
// summary. Idempotent — a second call leaves the string unchanged, so
// a repeated turn-complete (Claude re-init → re-complete) doesn't
// accumulate suffixes.
func forceCloseSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "Turn ended with tool unresolved"
	}
	if strings.HasSuffix(summary, forceCloseSuffix) {
		return summary
	}
	return summary + forceCloseSuffix
}

// turnCompleteFields holds the small set of derived values both
// settleTurnRow (persistence) and buildRoundCompletedEvent (wire
// payload) compute from the same EventTurnComplete inputs. Extracted
// because the two consumers run in different cadences (logical-turn
// vs wire-round) but the projection rules — stop_reason fallback to
// "error" on persistErr, assistant_message_id resolution across
// provider shapes, and error_message derived from meta or persistErr
// — are exactly the same. logicalTurnID is caller-supplied because
// completion paths may need to reconcile an event without TurnID back
// to the already-persisted (thread_id, turn_index) row.
type turnCompleteFields struct {
	logicalTurnID      string
	stopReason         string
	assistantMessageID string
	errorMessage       string
}

func decodeTurnCompleteFields(evt provider.ProviderEvent, logicalTurnID string, meta turnCompleteMeta, persistErr error) turnCompleteFields {
	stopReason := canonicalStopReason(meta)
	if persistErr != nil && stopReason == "" {
		stopReason = "error"
	}
	errorMessage := meta.Error
	if errorMessage == "" && persistErr != nil {
		errorMessage = persistErr.Error()
	}
	return turnCompleteFields{
		logicalTurnID:      logicalTurnID,
		stopReason:         stopReason,
		assistantMessageID: meta.AssistantMessageID,
		errorMessage:       errorMessage,
	}
}

func (r *Router) persistedTurnID(evt provider.ProviderEvent, turnIndex int) string {
	if id := strings.TrimSpace(evt.TurnID); id != "" {
		return id
	}
	if existing, found, err := r.store.GetTurnByThreadIndex(evt.ThreadID, turnIndex); err == nil && found {
		return existing.TurnID
	} else if err != nil {
		log.Printf("triage: lookup turn %s/%d: %v", evt.ThreadID, turnIndex, err)
	}
	return resolveTurnID(evt, turnIndex)
}

// settleTurnRow updates the persisted `turns` row at logical-turn
// granularity. Persists on best-effort: missing turn rows (e.g. a
// turn_complete with no matching turn_start because the first event
// arrived mid-crash-recovery) are tolerated — the UPDATE's
// sql.ErrNoRows is logged and the function returns. The frontend-
// facing `provider:turn_completed` emission has already fired by the
// time this runs (see handleTurnComplete: takeOpenRound + emit at the
// top, then claimTurnSettlement gate, then this).
//
// Top-level turn settle bumps thread activity through
// Store.MarkThreadActivity. Nested/internal turns only update turn state,
// so subagent completions do not reorder the sidebar. Synthesized
// session-died / abort flavors flow through this same path when they
// belong to the top-level turn.
func (r *Router) settleTurnRow(evt provider.ProviderEvent, turnIndex int, now int64, meta turnCompleteMeta, persistErr error) {
	fields := decodeTurnCompleteFields(evt, r.persistedTurnID(evt, turnIndex), meta, persistErr)

	usageJSON := ""
	if len(meta.Usage) > 0 {
		usageJSON = string(meta.Usage)
	}

	if err := r.store.UpdateTurnCompleted(fields.logicalTurnID, now, fields.stopReason, fields.assistantMessageID, usageJSON, fields.errorMessage); err != nil {
		log.Printf("triage: update turn %s: %v", fields.logicalTurnID, err)
	}
	if turnCountsAsThreadActivity(evt) {
		r.bumpThreadActivity(evt.ThreadID, now, "turn settle")
	}
}

func turnCountsAsThreadActivity(evt provider.ProviderEvent) bool {
	return strings.TrimSpace(evt.ParentToolUseID) == ""
}

// buildRoundCompletedEvent builds the frontend-facing
// TurnCompletedEvent payload for a single wire round. The TurnID is
// the per-round uuid (allocated in handleTurnStart / handleInit
// re-round) — the frontend treats each round as a distinct active
// turn, replacing its activeTurn entry on each round-start and
// clearing on the matching complete. StartedAt prefers the persisted
// `turns` row's started_at (logical-turn clock) and falls back to
// `now` when no row exists yet (mid-crash-recovery edge case).
//
// Pure projection — does not write the `turns` row. settleTurnRow
// owns the UPDATE and runs once per logical turn under the
// claimTurnSettlement gate.
//
// No persistErr parameter: this fires at the TOP of handleTurnComplete
// before any persistence runs, so there is no upstream persistence
// error to fold into the payload's stop_reason / error_message
// fields. If a persistence failure occurs later in handleTurnComplete,
// it surfaces through `persistErr` on the function return rather than
// retroactively rewriting the wire-round emission the frontend has
// already received.
func (r *Router) buildRoundCompletedEvent(
	evt provider.ProviderEvent,
	roundID string,
	turnIndex int,
	now int64,
	meta turnCompleteMeta,
) TurnCompletedEvent {
	fields := decodeTurnCompleteFields(evt, r.persistedTurnID(evt, turnIndex), meta, nil)
	startedAt := now
	if existing, found, err := r.store.GetTurn(fields.logicalTurnID); err == nil && found {
		startedAt = existing.StartedAt
	}
	return TurnCompletedEvent{
		ThreadID:            evt.ThreadID,
		TurnID:              roundID,
		TurnIndex:           turnIndex,
		StartedAt:           startedAt,
		CompletedAt:         now,
		StopReason:          fields.stopReason,
		AssistantMessageID:  fields.assistantMessageID,
		TokenUsage:          meta.Usage,
		ErrorMessage:        fields.errorMessage,
		Aborted:             meta.Aborted || meta.Truncated,
		RevertedUserMessage: r.consumeRevertedTurn(evt.ThreadID),
		CountsAsActivity:    turnCountsAsThreadActivity(evt),
	}
}

// persistLateTurnPayload folds late-arriving payload onto the existing
// turn row when handleTurnComplete fires after the turn was already
// settled. Two known sources:
//
//  1. Multi-result cascade — Claude's CLI emits a second `result`
//     envelope for the same logical turn after a task_notification
//     re-round. Cumulative `usage` lands on the second envelope; the
//     amid points to the final round's assistant message.
//  2. Soft round-close — `parse_stream.go` emits an
//     soft EventTurnComplete from `message_delta.stop_reason` before the
//     wire `result` envelope. The soft event carries the
//     parser's PEEKED last assistant_message_id (the consume happens
//     when the trailing `result` calls takeLastAssistantMessageID).
//     The trailing `result` carries cumulative usage that the soft
//     event lacks.
//
// Per-column semantics (different intentionally, see store
// UpdateTurnLatePayload):
//
//   - token_usage_json: first non-empty wins. The first settle's
//     usage stays on the row; later folds skip if already populated.
//   - assistant_message_id: last non-empty wins. Each subsequent
//     round overwrites so the persisted column is the FINAL
//     assistant message of the turn — what
//     `SettledTurn.assistantMessageId` is documented to be.
//   - stop_reason / error_message: late error wins. A soft
//     message_delta close can settle a turn as `end_turn` before the
//     trailing wire `result{is_error:true}` arrives; the error must
//     still be visible in persisted history.
//
// Folded as a single UPDATE so the common case (both fields arrive
// on the trailing `result`) pays one autocommit boundary.
func (r *Router) persistLateTurnPayload(evt provider.ProviderEvent, turnIndex int, meta turnCompleteMeta) {
	turnID := r.persistedTurnID(evt, turnIndex)
	usageJSON := ""
	if len(meta.Usage) > 0 {
		usageJSON = string(meta.Usage)
	}
	amid := meta.AssistantMessageID
	stopReason, errorMessage := lateErrorTurnPayload(meta)
	if usageJSON == "" && amid == "" && stopReason == "" && errorMessage == "" {
		return
	}
	if err := r.store.UpdateTurnLatePayload(turnID, store.LateTurnPayload{
		TokenUsageJSONIfEmpty:       usageJSON,
		AssistantMessageIDOverwrite: amid,
		StopReasonOverwrite:         stopReason,
		ErrorMessageOverwrite:       errorMessage,
	}); err != nil {
		log.Printf("triage: update turn %s late payload: %v", turnID, err)
	}
}

func lateErrorTurnPayload(meta turnCompleteMeta) (string, string) {
	if canonicalStopReason(meta) != "error" && meta.Error == "" {
		return "", ""
	}
	return "error", meta.Error
}

// closeTurnSpan ends the live turn span for the thread, flagging it as
// errored when persistErr is non-nil. Safe to call with no active span.
func (r *Router) closeTurnSpan(threadID string, persistErr error) {
	r.mu.Lock()
	span, ok := r.turnSpans[threadID]
	if ok {
		delete(r.turnSpans, threadID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	if persistErr != nil {
		span.RecordError(persistErr)
		r.metrics.TurnsErrored.Add(context.Background(), 1)
	} else {
		r.metrics.TurnsCompleted.Add(context.Background(), 1)
	}
	span.End()
}

func (r *Router) currentTurnIndex(threadID string) (int, error) {
	r.mu.Lock()
	if turnIndex, ok := r.openTurns[threadID]; ok {
		r.mu.Unlock()
		return turnIndex, nil
	}
	r.mu.Unlock()
	return r.store.LastTurnIndex(threadID)
}

// openTurnIndex returns the router's currently-open turn for threadID,
// or (_, false) when no turn is in flight. Distinct from
// currentTurnIndex: this does NOT fall back to the store's
// LastTurnIndex, so callers that must drop rather than attach a live
// event to a closed turn can do so without a separate is-turn-open
// check.
func (r *Router) openTurnIndex(threadID string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turnIndex, ok := r.openTurns[threadID]
	return turnIndex, ok
}

// hasInFlightTurnOrRound reports whether the router still has any
// turn or wire-round state for threadID worth synthesizing a
// truncated turn-complete against. Used by CleanupThread and
// handleSessionDied so the round-2+ re-round case (handleInit's
// maybeEmitReRoundOnInit branch sets currentRoundByThread WITHOUT
// calling setOpenTurn — counters survive the multi-result boundary
// by design) still produces a frontend `provider:turn_completed`
// when the session dies mid-round-2. Without the currentRoundByThread
// arm of this check, the openTurns map alone misses the round-2+
// case because clearOpenTurn cleared it at the end of round 1.
func (r *Router) hasInFlightTurnOrRound(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.openTurns[threadID]; ok {
		return true
	}
	if _, ok := r.currentRoundByThread[threadID]; ok {
		return true
	}
	return false
}

func (r *Router) setOpenTurn(threadID string, turnIndex int) {
	r.mu.Lock()
	r.openTurns[threadID] = turnIndex
	key := scopeCounterKey(threadID, turnIndex, "")
	r.segmentIndexByScope[key] = -1
	r.blockIndexByScope[key] = -1
	r.clearActiveStreamBlocksForTurnLocked(threadID, turnIndex)
	r.streamingItemCounts[threadID] = 0
	deleteByPrefix(r.streamingScopeCounts, threadID+"|")
	delete(r.errorSeqByScope, key)
	// Clear the settled marker so a re-init (Claude resend system.init
	// after an interrupt; Codex resend turn/started) can settle the
	// same turn again. The multi-result-per-turn case does NOT re-fire
	// EventTurnStart between the two completes, so the marker survives
	// there and the second complete returns early.
	delete(r.settledTurns, settledTurnKey(threadID, turnIndex))
	r.mu.Unlock()
}

func (r *Router) clearActiveStreamBlocksForTurnLocked(threadID string, turnIndex int) {
	prefix := fmt.Sprintf("%s|%d|", threadID, turnIndex)
	deleteByPrefix(r.activeTextBlocks, prefix)
	deleteByPrefix(r.activeThinkingBlocks, prefix)
	deleteByPrefix(r.activeTextBlockRefs, prefix)
	deleteByPrefix(r.activeThinkingBlockRefs, prefix)
	for key := range r.streamPersistBuffers {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if buffer := r.streamPersistBuffers[key]; buffer != nil && buffer.timer != nil {
			buffer.timer.Stop()
		}
		delete(r.streamPersistBuffers, key)
	}
	// streamingPathRefsLast is keyed by streamPersistKey
	// (threadID|itemID). Assistant_text ids carry the turn index in
	// their suffix (textItemID → "text:<turnIndex>:[scope:]<n>"), so
	// the prefix "threadID|text:<turnIndex>:" sweeps every entry this
	// turn allocated — scoped (subagent) variants share the same
	// turn-index segment because scope appears AFTER it.
	deleteByPrefix(r.streamingPathRefsLast, threadID+"|text:"+fmt.Sprintf("%d:", turnIndex))
}

// claimTurnSettlement records that handleTurnComplete has begun logical-turn
// settlement for (threadID, turnIndex). It returns true only for the first
// claimant; later callers should take the duplicate/late-payload path.
func (r *Router) claimTurnSettlement(threadID string, turnIndex int) bool {
	key := settledTurnKey(threadID, turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settledTurns[key] {
		return false
	}
	r.settledTurns[key] = true
	return true
}

// isTurnSettled reports whether logical-turn settlement has already run
// for (threadID, turnIndex). Read-only companion to claimTurnSettlement
// for callers that need to discriminate "late result for a settled
// turn" from "result for a turn that never started" without claiming
// the settlement slot themselves.
func (r *Router) isTurnSettled(threadID string, turnIndex int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settledTurns[settledTurnKey(threadID, turnIndex)]
}

func settledTurnKey(threadID string, turnIndex int) string {
	return fmt.Sprintf("%s|%d", threadID, turnIndex)
}

// setOpenRoundSnapshot records the active wire-round snapshot for threadID. A round
// begins on either:
//
//   - handleTurnStart (per user-typed send) — the canonical first
//     round of every logical turn.
//   - handleInit when the prior round of the same logical turn already
//     settled — the re-round case for Claude's multi-result-per-turn
//     wire pattern, where the CLI synthesizes a `type:"user"` envelope
//     from a task_notification and emits a fresh `system.init`
//     followed by another `result`. Each `result` is its own wire
//     round from the model's POV.
//
// Round ids are uuids; the frontend treats each round as a distinct
// active turn for indicator / composer / Stop-button purposes. The
// persisted `turns` row id stays on `resolveTurnID` (logical-turn
// granularity); only the wire payload's TurnID field carries the round
// id. See internal/triage/AGENTS.md "Wire-round vs logical-turn" for
// the full mental model.
//
// Overwrites any prior round snapshot for the thread — a leaked round (e.g.
// if currentTurnIndex failed in the prior handleTurnComplete and the
// emit at the top was skipped) cannot survive into the next round.
// CleanupThread also sweeps this map.
func (r *Router) setOpenRoundSnapshot(snapshot ActiveTurnSnapshot) {
	if snapshot.ThreadID == "" || snapshot.TurnID == "" {
		return
	}
	r.mu.Lock()
	r.currentRoundByThread[snapshot.ThreadID] = snapshot
	r.mu.Unlock()
}

// takeOpenRound returns the active wire-round snapshot for threadID and
// clears the slot in the same critical section. Empty string when no
// round is open — the caller (handleTurnComplete) skips the per-round
// `provider:turn_completed` emission in that case. The
// "read-and-clear" semantics handle the synthetic-truncate-then-real
// race naturally: the synthetic complete takes the slot and emits;
// the real complete finds it empty and skips, so the frontend sees
// exactly one `provider:turn_completed` per round even when handleError
// synthesizes ahead of the wire's own turn-complete.
func (r *Router) takeOpenRound(threadID string) (ActiveTurnSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	round, ok := r.currentRoundByThread[threadID]
	delete(r.currentRoundByThread, threadID)
	return round, ok
}

// openRoundID returns the active wire-round id for threadID without
// clearing the slot. Read-only counterpart to takeOpenRound for
// callers that observe the round id mid-round (handleToolStart's
// flush trigger). Returns "" when no round is open — the caller
// short-circuits in that case (a tool_start with no open round means
// the wire is mid-init and the trigger would have nowhere to anchor
// its "fired this round" marker).
func (r *Router) openRoundID(threadID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentRoundByThread[threadID].TurnID
}

// ActiveTurnSnapshot returns the current live wire-round snapshot for a
// thread without clearing it. Most production hydration goes through
// LiveStateSnapshotForThread; this narrow accessor remains for tests and
// diagnostics that only need the active-turn slot.
func (r *Router) ActiveTurnSnapshot(threadID string) (ActiveTurnSnapshot, bool) {
	if r == nil || threadID == "" {
		return ActiveTurnSnapshot{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, ok := r.currentRoundByThread[threadID]
	return snapshot, ok
}

// activeRoundTurnIndex returns the turn index for the frontend-visible active
// wire round. Unlike currentTurnIndex, this never falls back to the newest
// stored turn; callers use it when an event should be dropped from chat history
// once the user-facing task indicator is no longer running.
func (r *Router) activeRoundTurnIndex(threadID string) (int, bool) {
	if r == nil || threadID == "" {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, ok := r.currentRoundByThread[threadID]
	if !ok {
		return 0, false
	}
	return snapshot.TurnIndex, true
}

// clearOpenTurn drops per-turn flow-control state at EventTurnComplete.
//
// Three distinct lifecycles intersect here and MUST stay separate (see
// internal/triage/AGENTS.md "Correlation state" for the full taxonomy):
//
//   - **Per-turn flow-control state** swept HERE: openTurns,
//     interruptQueue, streamingItemCounts, activeTextBlocks/Thinking,
//     pendingCommandDiffs, pendingApprovals (and siblings). These maps
//     answer "what's mid-turn right now."
//   - **Id-allocating counters** (segmentIndexByScope, blockIndexByScope,
//     errorSeqByScope, compactionSeqByScope, terminalInteractionSeq):
//     cleared at CleanupThread, with a selective re-init reset in
//     setOpenTurn. Wiping HERE would
//     cause id collisions when the wire emits two `result` envelopes
//     for one logical turn (Claude task_notification → CLI-synthesized
//     `user` envelope → second result, fatal-error synthetic-truncate
//     then real wire complete) — see the regression coverage in
//     multi_result_test.go.
//   - **Logical-turn settlement state** (`settledTurns`) survives
//     turn-end by design. It clears in setOpenTurn (so a re-init can
//     re-settle) and CleanupThread.
//
// activeTextBlocks/Thinking ARE per-turn flow-control (they guard
// against re-creating a row mid-stream), so they stay swept here as a
// safety net for any block that didn't settle through the normal close
// path.
func (r *Router) clearOpenTurn(threadID string) {
	r.mu.Lock()
	turnIndex, ok := r.openTurns[threadID]
	if ok {
		r.clearActiveStreamBlocksForTurnLocked(threadID, turnIndex)
		// pendingCommandDiffs is keyed by `<threadID>:<itemID>` and
		// stages an inline-diff preview between EventToolStart and
		// EventToolComplete for command_execution rows. If the matching
		// completion never arrived in this turn (interrupted, crashed),
		// the entry would otherwise leak until CleanupThread.
		for key, pending := range r.pendingCommandDiffs {
			if pending.ThreadID == threadID {
				delete(r.pendingCommandDiffs, key)
			}
		}
		deleteByPrefix(r.pendingToolPaths, threadID+"|")
		// pendingApprovals / pendingApprovalItems / pendingUserInputs
		// are keyed by `<threadID>:<requestID-or-itemID>`. Approvals are
		// inherently mid-turn — the model issues a control_request, the
		// user resolves, the model continues. If EventTurnComplete fires
		// while one of these is still pending, the turn ended without
		// resolution (subprocess died, fatal error, model declined to
		// emit the resolved meta). Sweep them so the next turn doesn't
		// inherit a stale request id.
		approvalPrefix := threadID + ":"
		deleteByPrefix(r.pendingApprovals, approvalPrefix)
		deleteByPrefix(r.pendingApprovalItems, approvalPrefix)
		deleteByPrefix(r.pendingUserInputs, approvalPrefix)
		delete(r.pendingApprovalOrder, threadID)
		delete(r.pendingUserInputOrder, threadID)
	}
	delete(r.openTurns, threadID)
	delete(r.interruptQueue, threadID)
	delete(r.streamingItemCounts, threadID)
	deleteByPrefix(r.streamingScopeCounts, threadID+"|")
	// openAPIRetryRows tracks "thread has a running api_retry row that
	// still needs flipping". By turn-end the row was either flipped
	// already or the turn closed without forward progress; either way
	// the next turn starts with a clean flag.
	delete(r.openAPIRetryRows, threadID)
	// revertedTurns is normally read-and-cleared inside
	// buildRoundCompletedEvent. Defensive sweep here covers the case
	// where MarkTurnReverted was set but no turn-completed actually
	// fires (rare — e.g. the thread had no open turn so no
	// synthesizeTruncatedTurnComplete ran).
	delete(r.revertedTurns, threadID)
	r.mu.Unlock()
}

func isFatalProviderError(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	fatal, _ := meta["fatal"].(bool)
	return fatal
}

func (r *Router) markTurnItemsErrored(threadID string, turnIndex int, now int64) error {
	return r.flipTurnItemsErrored(threadID, turnIndex, now, interruptedSummary)
}

// flipTurnItemsErrored flips every running/streaming item in the turn
// to errored and rewrites its summary via summaryFn. The function-over-
// string indirection lets the user-interrupt path ("— stopped") and
// the truncated-turn path ("— interrupted") share the same iteration
// logic without either branch learning about the other.
//
// Backgrounded tool_call launches are EXEMPT (invariant 24): their work
// legitimately outlives the launching turn, and the sibling
// tool_completion row (written by EventBackgroundTaskTerminal) is the
// thing that carries the interrupted/stopped marker when the task
// settles. Mirrors the exemption in forceCloseOrphanToolCalls.
func (r *Router) flipTurnItemsErrored(
	threadID string,
	turnIndex int,
	now int64,
	summaryFn func(string) string,
) error {
	if err := r.flushStreamingThread(threadID); err != nil {
		return fmt.Errorf("error flip flush streaming buffers: %w", err)
	}
	items, err := r.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return fmt.Errorf("error flip list turn items: %w", err)
	}
	for _, item := range items {
		if item.Status != statusRunning && item.Status != statusStreaming {
			continue
		}
		if item.IsBackground && item.Kind == itemKindToolCall {
			continue
		}
		item.Status = statusErrored
		item.Summary = summaryFn(item.Summary)
		item.UpdatedAt = now
		if err := r.persistItem(item, nil); err != nil {
			return fmt.Errorf("error flip item %s: %w", item.ID, err)
		}
	}
	return nil
}

// stoppedSummary is the user-interrupt sibling of interruptedSummary.
// Spec wording: "stopped" for a user-initiated interrupt, "interrupted"
// for a fatal crash / truncation. Both suffixes are idempotent so
// re-applying leaves the summary unchanged.
func stoppedSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "Stopped"
	}
	const suffix = " — stopped"
	if strings.HasSuffix(summary, suffix) {
		return summary
	}
	return summary + suffix
}

// MarkUserInterrupt is the public chokepoint for the app-layer
// interrupt flow. It flips every streaming/running item in the current
// turn to errored with a " — stopped" suffix and records a new
// "Stopped by user" system error row so the timeline carries the
// explicit user-facing signal.
//
// Returns the error id that was persisted so the caller can surface
// diagnostics or correlate with downstream emissions; empty string
// when the thread has no open turn.
func (r *Router) MarkUserInterrupt(threadID string) (string, error) {
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		// Nothing to stop if we can't resolve a turn.
		return "", nil
	}
	now := time.Now().UnixMilli()
	if err := r.flipTurnItemsErrored(threadID, turnIndex, now, stoppedSummary); err != nil {
		return "", err
	}
	seq := r.nextErrorSequence(threadID, turnIndex, "")
	errID := nextErrorID(turnIndex, "", seq)
	item := store.Item{
		ID:        errID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "error",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   "Stopped by user",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.persistItem(item, nil); err != nil {
		return "", err
	}
	return errID, nil
}

// Wait blocks until every in-flight Handle call has returned and every
// fire-and-forget settle goroutine has completed, or until ctx is
// cancelled. Shutdown uses this to drain the router before flushing
// observability writers and closing the store; a completed Wait means
// the SQLite write boundary has caught up to every event the router
// classified. Callers that pass a deadlined ctx get a
// context.DeadlineExceeded when the drain runs long.
func (r *Router) Wait(ctx context.Context) error {
	if r == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		// Settle goroutines outlive their spawning Handle call (the
		// caller has already returned), so wait for them too before
		// flushing stream persistence — flushAllStreamPersistence
		// writes to SQLite and must not race a settle's persistItem.
		r.settleWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return r.flushAllStreamPersistence()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForPendingSettles blocks until every fire-and-forget settle
// goroutine spawned by settleStreamingTextAsync /
// settleStreamingThinkingAsync (and the per-scope goroutines inside
// settleTurnStreaming) has completed. Used by app shutdown so SQLite
// isn't closed underneath an in-flight settle. Unlike Wait, this does
// NOT drain in-flight Handle calls — call Wait for the full shutdown
// drain.
func (r *Router) WaitForPendingSettles() {
	if r == nil {
		return
	}
	r.settleWG.Wait()
}

// CleanupThread removes all accumulator state for a thread. Call this when a
// session ends or disconnects to prevent memory leaks. Also flags the
// thread as "stopped" so any event that arrives afterward — typically a
// readLoop line that was already in-flight when StopSession returned — is
// dropped instead of persisting under the torn-down session (Bug B5).
// The flag stays set until the host commits to a replacement session
// and calls MarkThreadActive; wire events never clear it.
//
// As a safety net, any open turn at cleanup time gets a synthesized
// truncated EventTurnComplete so the frontend working indicator clears
// even when the session ends without a wire turn-complete (clean stdout
// EOF, host-side StopSession during a turn, etc). This pairs with
// session_status.go's handleSessionDied — which path runs first
// depends on whether `EventSessionStatus{"error"}` arrives before
// or after StopSession unwinds — and is idempotent in either order
// thanks to claimTurnSettlement.
func (r *Router) CleanupThread(threadID string) {
	r.cleanupThread(threadID, nil)
}

// CleanupThreadIfEpoch is CleanupThread for asynchronous teardowns that
// can lose a race against a replacement session start. The caller
// captures ThreadEpoch BEFORE unregistering the dead session; if
// MarkThreadActive has bumped the epoch since — the host committed to a
// replacement, whose spawn can take seconds while this teardown runs on
// its own goroutine — the cleanup aborts and returns false instead of
// stopping the live thread and sweeping its state.
//
// The epoch is checked twice: once on entry (before the flush/synthesize
// preamble) and again inside the locked sweep, atomically with setting
// the stopped marker. A reactivation landing between the two checks is
// therefore still caught before anything destructive happens — in that
// window the preamble can only have touched residual state of the dead
// session, because the replacement subprocess (spawned only after
// MarkThreadActive) cannot have produced turns or streams within the
// microseconds the preamble takes.
func (r *Router) CleanupThreadIfEpoch(threadID string, epoch uint64) bool {
	r.mu.Lock()
	current := r.threadEpochs[threadID]
	r.mu.Unlock()
	if current != epoch {
		return false
	}
	return r.cleanupThread(threadID, &epoch)
}

// cleanupThread is the shared body behind CleanupThread (requireEpoch
// nil: unconditional) and CleanupThreadIfEpoch (requireEpoch set: the
// locked sweep aborts when the thread's epoch no longer matches).
// Returns false only on an epoch-abort.
func (r *Router) cleanupThread(threadID string, requireEpoch *uint64) bool {
	cleanupAt := time.Now().UnixMilli()
	if r.hasInFlightTurnOrRound(threadID) {
		if err := r.synthesizeTruncatedTurnComplete(threadID, cleanupAt); err != nil {
			log.Printf("triage: synthesize turn-complete on cleanup for thread %s: %v", threadID, err)
		}
	}
	if err := r.flushStreamingThread(threadID); err != nil {
		log.Printf("triage: cleanup flush stream buffers for thread %s: %v", threadID, err)
	}

	r.mu.Lock()
	if requireEpoch != nil && r.threadEpochs[threadID] != *requireEpoch {
		r.mu.Unlock()
		return false
	}
	// Set the stopped flag BEFORE dropping other state so Handle observes
	// a consistent snapshot: any concurrent Handle call either sees a live
	// thread with full state, or a stopped thread with no state.
	r.stoppedThreads[threadID] = struct{}{}
	var orphanSpan trace.Span
	if span, ok := r.turnSpans[threadID]; ok {
		orphanSpan = span
		delete(r.turnSpans, threadID)
	}
	delete(r.openTurns, threadID)
	delete(r.interruptQueue, threadID)
	delete(r.streamingItemCounts, threadID)
	deleteByPrefix(r.streamingScopeCounts, threadID+"|")
	delete(r.workspacePathByThread, threadID)
	for key, pending := range r.pendingCommandDiffs {
		if pending.ThreadID == threadID {
			delete(r.pendingCommandDiffs, key)
		}
	}
	deleteByPrefix(r.pendingToolPaths, threadID+"|")
	approvalPrefix := threadID + ":"
	deleteByPrefix(r.pendingApprovals, approvalPrefix)
	deleteByPrefix(r.pendingApprovalItems, approvalPrefix)
	deleteByPrefix(r.pendingUserInputs, approvalPrefix)
	delete(r.pendingApprovalOrder, threadID)
	delete(r.pendingUserInputOrder, threadID)
	prefix := threadID + "|"
	deleteByPrefix(r.segmentIndexByScope, prefix)
	deleteByPrefix(r.blockIndexByScope, prefix)
	deleteByPrefix(r.activeTextBlocks, prefix)
	deleteByPrefix(r.activeThinkingBlocks, prefix)
	deleteByPrefix(r.activeTextBlockRefs, prefix)
	deleteByPrefix(r.activeThinkingBlockRefs, prefix)
	deleteByPrefix(r.errorSeqByScope, prefix)
	deleteByPrefix(r.compactionSeqByScope, prefix)
	deleteByPrefix(r.notificationSeqByScope, prefix)
	for key := range r.streamPersistBuffers {
		if strings.HasPrefix(key, prefix) {
			if buffer := r.streamPersistBuffers[key]; buffer != nil && buffer.timer != nil {
				buffer.timer.Stop()
			}
			delete(r.streamPersistBuffers, key)
		}
	}
	deleteByPrefix(r.streamingPathRefsLast, prefix)
	deleteByPrefix(r.terminalInteractionSeq, prefix)
	deleteByPrefix(r.settledTurns, prefix)
	// currentRoundByThread is keyed by threadID — a single delete is the
	// correct primitive. Cleared every wire-complete by takeOpenRound;
	// CleanupThread is the safety net for sessions that ended without
	// a final wire turn-complete (clean stdout EOF, host-side
	// StopSession during a round).
	delete(r.currentRoundByThread, threadID)
	delete(r.latestTodoByThread, threadID)
	delete(r.tasksByThread, threadID)
	delete(r.openAPIRetryRows, threadID)
	// Drop the Codex background projector's per-thread trackers. A
	// restarted session never inherits trackers from a prior session —
	// the wire replays item/started for still-inProgress items and the
	// projector re-observes them fresh. Keeping stale trackers would
	// cause the first yield in the new session to stamp a row that
	// belongs to an entirely different process id.
	delete(r.codexBackground, threadID)
	// Pending-send registry + wire-only dedup are user-send-time
	// carry-over (see internal/triage/CLAUDE.md correlation taxonomy):
	// the queue can outlive any single turn, but a torn-down session
	// must not leak entries into a fresh one. Both sweeps share the
	// thread-key path used elsewhere in this routine.
	r.clearPendingSendsLocked(threadID)
	r.clearWireOnlyUserTextLocked(threadID)
	// Flush-queue + trigger-fired marker are session-scoped: a torn-down
	// session must not deliver yesterday's queued user text to a fresh
	// session that re-attaches to the same threadID, and a stale fired
	// marker must not block the next session's first-round trigger.
	r.clearFlushQueueLocked(threadID)
	// Safety net for the revert-on-interrupt marker. Normally consumed
	// by buildRoundCompletedEvent during the synthesized truncated
	// turn-complete that fires above; this catches the case where the
	// flag was set but no emission happened (e.g. predicate raced).
	delete(r.revertedTurns, threadID)
	pendingUsage, hasPendingUsage := r.takeUsageEmitPendingLocked(threadID)
	delete(r.usageEmitThrottles, threadID)
	r.mu.Unlock()

	if hasPendingUsage {
		r.emit("provider:usage", pendingUsage)
	}

	if orphanSpan != nil {
		// Closing outside the lock avoids self-deadlock when the tracer's
		// OnEnd hook reaches for any shared resource.
		orphanSpan.End()
	}
	if r.store != nil {
		count, err := r.store.MarkLiveCodexSubagentLaunchesInactive(threadID, cleanupAt)
		if err != nil {
			log.Printf("triage: cleanup live Codex subagent launches for thread %s: %v", threadID, err)
		} else if count > 0 {
			r.emitCodexBackgroundTasksChanged(threadID)
		}
	}
	return true
}

// ResetThreadForRollback clears session-local routing state after the provider
// has accepted a conversation rollback while the provider session remains live.
// Unlike CleanupThread, it must not mark the thread stopped and must not
// synthesize a turn-complete: any required turn completion has already arrived
// before rollback is allowed.
func (r *Router) ResetThreadForRollback(threadID string) {
	if err := r.flushStreamingThread(threadID); err != nil {
		log.Printf("triage: rollback flush stream buffers for thread %s: %v", threadID, err)
	}

	r.mu.Lock()
	var orphanSpan trace.Span
	if span, ok := r.turnSpans[threadID]; ok {
		orphanSpan = span
		delete(r.turnSpans, threadID)
	}
	delete(r.openTurns, threadID)
	delete(r.interruptQueue, threadID)
	delete(r.streamingItemCounts, threadID)
	deleteByPrefix(r.streamingScopeCounts, threadID+"|")
	delete(r.workspacePathByThread, threadID)
	for key, pending := range r.pendingCommandDiffs {
		if pending.ThreadID == threadID {
			delete(r.pendingCommandDiffs, key)
		}
	}
	deleteByPrefix(r.pendingToolPaths, threadID+"|")
	approvalPrefix := threadID + ":"
	deleteByPrefix(r.pendingApprovals, approvalPrefix)
	deleteByPrefix(r.pendingApprovalItems, approvalPrefix)
	deleteByPrefix(r.pendingUserInputs, approvalPrefix)
	delete(r.pendingApprovalOrder, threadID)
	delete(r.pendingUserInputOrder, threadID)
	prefix := threadID + "|"
	deleteByPrefix(r.segmentIndexByScope, prefix)
	deleteByPrefix(r.blockIndexByScope, prefix)
	deleteByPrefix(r.activeTextBlocks, prefix)
	deleteByPrefix(r.activeThinkingBlocks, prefix)
	deleteByPrefix(r.activeTextBlockRefs, prefix)
	deleteByPrefix(r.activeThinkingBlockRefs, prefix)
	deleteByPrefix(r.errorSeqByScope, prefix)
	deleteByPrefix(r.compactionSeqByScope, prefix)
	deleteByPrefix(r.notificationSeqByScope, prefix)
	for key := range r.streamPersistBuffers {
		if strings.HasPrefix(key, prefix) {
			if buffer := r.streamPersistBuffers[key]; buffer != nil && buffer.timer != nil {
				buffer.timer.Stop()
			}
			delete(r.streamPersistBuffers, key)
		}
	}
	deleteByPrefix(r.streamingPathRefsLast, prefix)
	deleteByPrefix(r.terminalInteractionSeq, prefix)
	deleteByPrefix(r.settledTurns, prefix)
	delete(r.currentRoundByThread, threadID)
	delete(r.latestTodoByThread, threadID)
	delete(r.tasksByThread, threadID)
	delete(r.openAPIRetryRows, threadID)
	r.clearPendingSendsLocked(threadID)
	r.clearWireOnlyUserTextLocked(threadID)
	r.clearFlushQueueLocked(threadID)
	delete(r.revertedTurns, threadID)
	pendingUsage, hasPendingUsage := r.takeUsageEmitPendingLocked(threadID)
	delete(r.usageEmitThrottles, threadID)
	r.mu.Unlock()

	if hasPendingUsage {
		r.emit("provider:usage", pendingUsage)
	}
	if orphanSpan != nil {
		orphanSpan.End()
	}
}

// isThreadStopped returns true when CleanupThread has been called for
// threadID and the host has not since re-activated it via
// MarkThreadActive.
func (r *Router) isThreadStopped(threadID string) bool {
	r.mu.Lock()
	_, stopped := r.stoppedThreads[threadID]
	r.mu.Unlock()
	return stopped
}

// MarkThreadActive clears the stopped flag so a replacement session's
// events persist again. The host calls this from the session-start
// path BEFORE spawning the new subprocess — it must not wait for a
// wire signal, because a session that dies during startup (e.g. Claude
// rejecting --resume-session-at pre-init) emits its only diagnostics
// before anything init-like, and those must route. Clearing pre-spawn
// is safe: the prior subprocess's read loop is fully drained before a
// start proceeds (provider Close blocks on read-loop exit), so no
// stale frame can slip through the freshly-cleared gate. No wire event
// may clear this flag — see the stoppedThreads field comment.
//
// Reactivation also resets the thread's logical-turn settlement ledger.
// The context-repair restart deliberately skips CleanupThread, so
// settledTurns can still hold entries from the prior subprocess; a
// replacement session reuses turn indexes, and a stale "settled" marker
// would misroute its first error result into persistLateTurnPayload
// (folding into a finished turn's row) instead of the orphan error-item
// branch the user actually needs to see. Clearing here is safe for the
// soft-close late-fold path — the fold's trailing result rides the SAME
// session's read loop, which is fully drained before any restart
// reaches this call.
//
// Each call bumps the thread's reactivation epoch (see ThreadEpoch),
// which lets asynchronous teardowns detect that a replacement session
// claimed the thread mid-teardown and abort their cleanup.
func (r *Router) MarkThreadActive(threadID string) {
	r.mu.Lock()
	delete(r.stoppedThreads, threadID)
	deleteByPrefix(r.settledTurns, threadID+"|")
	r.threadEpochs[threadID]++
	r.mu.Unlock()
}

// ThreadEpoch returns the thread's reactivation epoch: a counter bumped
// by every MarkThreadActive. Asynchronous teardowns capture it before
// unregistering a dead session and pass it to CleanupThreadIfEpoch so a
// cleanup that lost the race against a replacement session start
// no-ops instead of stopping the live thread. Epoch entries are never
// deleted — a removed entry would read as 0 again and let a stale
// teardown's captured 0 match.
func (r *Router) ThreadEpoch(threadID string) uint64 {
	r.mu.Lock()
	epoch := r.threadEpochs[threadID]
	r.mu.Unlock()
	return epoch
}
