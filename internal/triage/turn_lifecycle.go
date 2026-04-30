package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/diffsummary"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// handleTurnStart opens the per-turn span, seeds bookkeeping, and (if
// a checkpoint store is wired up) captures a baseline snapshot of the
// workspace. It also writes the `turns` row for this turn (completed_at
// NULL) and emits `provider:turn_started` so the frontend flips
// `pane.activeTurn` on. Checkpoint failure is NOT fatal to the turn —
// it surfaces as a `checkpoint:error` event the UI renders as a
// dismissible banner, and the turn proceeds.
func (r *Router) handleTurnStart(evt provider.ProviderEvent) error {
	turnIndex := evt.TurnIndex
	if turnIndex == 0 {
		var err error
		turnIndex, err = r.store.LastTurnIndex(evt.ThreadID)
		if err != nil {
			log.Printf("triage: last turn index on turn start: %v", err)
			turnIndex = 0
		}
	}
	r.setOpenTurn(evt.ThreadID, turnIndex)
	r.openTurnSpan(evt)
	r.captureBaselineForTurn(context.Background(), evt.ThreadID, turnIndex)

	// Capture the PRIOR turn's completion checkpoint at the moment the
	// user sends the next message. This matches Claude Code's behavior
	// (fileHistoryMakeSnapshot fires at user-prompt-submit, not at turn
	// end — see QueryEngine.ts:641-655 and handlePromptSubmit.ts:528).
	//
	// Why user-send-time is the right boundary:
	//
	//   1. The captured state = working tree at the moment the user
	//      committed to the new prompt. "Revert to before this prompt"
	//      maps directly to this checkpoint.
	//   2. It naturally captures the multi-result-per-turn case (Claude's
	//      task_notification → CLI-synthesized `type:"user"` envelope →
	//      second `result`). Both halves' writes have settled by the
	//      time the next user-send arrives, so the capture is
	//      cumulative without any merge gymnastics.
	//   3. It naturally captures any manual edits the user made between
	//      turns — those are part of the user's state at send-time.
	//
	// Only fires for turnIndex > 0 because turn 0's pre-state is the
	// baseline checkpoint already captured above.
	//
	// Runs synchronously rather than in a goroutine: the captured state
	// MUST reflect the working tree as it existed BEFORE any new-turn
	// tools start writing. An async capture races against tool_use
	// blocks the model emits as soon as the provider write returns, and
	// could include early new-turn writes in what's supposed to be the
	// prior-turn checkpoint. The latency cost (hundreds of ms of git
	// commands) is the price of correctness; if this becomes a UX
	// problem we'll need a different mechanism (e.g. shadow-clone of
	// the workspace) rather than racing the model.
	if turnIndex > 0 {
		r.capturePriorTurnCheckpoint(context.Background(), evt.ThreadID, turnIndex-1)
	}

	startedAt := eventTimestampMillis(evt)
	turnID := resolveTurnID(evt, turnIndex)
	inserted := r.upsertTurnRow(store.Turn{
		TurnID:    turnID,
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	})
	if inserted {
		r.markAcceptedSourceProposedPlan(evt.ThreadID, turnIndex, startedAt)
		r.markAcceptedRevisionComments(evt.ThreadID, turnIndex, turnID, startedAt)
	}

	r.emit("provider:turn_started", TurnStartedEvent{
		ThreadID:  evt.ThreadID,
		TurnID:    turnID,
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	})
	return nil
}

func (r *Router) markAcceptedRevisionComments(threadID string, turnIndex int, turnID string, startedAt int64) {
	commentIDs, found, err := r.store.RevisionSourceCommentIDsForTurn(threadID, turnIndex)
	if err != nil {
		log.Printf("triage: revision source comments for turn %s/%d: %v", threadID, turnIndex, err)
		return
	}
	if !found {
		return
	}
	source, sourceFound, err := r.store.RevisionSourceProposedPlanForTurn(threadID, turnIndex)
	if err != nil {
		log.Printf("triage: revision source plan for turn %s/%d: %v", threadID, turnIndex, err)
		return
	}
	if !sourceFound || source.ThreadID != threadID {
		return
	}
	r.markAcceptedRevisionCommentsForItem(threadID, source.ItemID, commentIDs, startedAt, turnID)
}

func (r *Router) markAcceptedSourceProposedPlan(threadID string, turnIndex int, startedAt int64) {
	source, found, err := r.store.SourceProposedPlanForTurn(threadID, turnIndex)
	if err != nil {
		log.Printf("triage: source proposed plan for turn %s/%d: %v", threadID, turnIndex, err)
		return
	}
	if !found {
		return
	}
	implementationItemID := fmt.Sprintf("user:%d", turnIndex)
	r.markAcceptedSourceProposedPlanForItem(source.ThreadID, source.ItemID, threadID, implementationItemID, startedAt)
}

// markAcceptedSourceProposedPlanForItem marks the source plan implemented
// and emits the refreshed plan item. Idempotent against re-fired
// EventTurnStart — the store dedupes via ErrProposedPlanAlreadyImplemented.
func (r *Router) markAcceptedSourceProposedPlanForItem(
	sourceThreadID, sourceItemID,
	implementationThreadID, implementationItemID string,
	implementedAt int64,
) {
	if sourceItemID == "" {
		return
	}
	item, found, err := r.store.GetThreadItem(sourceThreadID, sourceItemID)
	if err != nil {
		log.Printf("triage: validate source proposed plan %s/%s: %v", sourceThreadID, sourceItemID, err)
		return
	}
	if !found || item.Role != "assistant" || item.PayloadKind != "proposed_plan" {
		log.Printf("triage: refusing invalid source proposed plan %s/%s", sourceThreadID, sourceItemID)
		return
	}
	if err := r.store.MarkProposedPlanImplemented(sourceThreadID, sourceItemID, implementationThreadID, implementationItemID, implementedAt); err != nil {
		if !errors.Is(err, store.ErrProposedPlanAlreadyImplemented) {
			log.Printf("triage: mark proposed plan implemented %s/%s: %v", sourceThreadID, sourceItemID, err)
		}
		return
	}
	plan, found, err := r.store.GetThreadProposedPlanItem(sourceThreadID, sourceItemID)
	if err != nil {
		log.Printf("triage: refresh implemented proposed plan %s/%s: %v", sourceThreadID, sourceItemID, err)
		return
	}
	if found {
		r.emit("provider:item_event", NewItemStreamUpsert(plan))
	}
}

// markAcceptedRevisionCommentsForItem stamps the supplied comment
// IDs as sent against the source plan and emits the refreshed plan item
// so the frontend's draft/sent counters update. Counterpart helper to
// markAcceptedSourceProposedPlanForItem.
func (r *Router) markAcceptedRevisionCommentsForItem(
	sourceThreadID, sourceItemID string,
	commentIDs []string,
	sentAt int64,
	sentTurnID string,
) {
	if sourceItemID == "" || len(commentIDs) == 0 {
		return
	}
	plan, found, err := r.store.GetThreadProposedPlanItem(sourceThreadID, sourceItemID)
	if err != nil {
		log.Printf("triage: validate source plan for revision comments %s/%s: %v", sourceThreadID, sourceItemID, err)
		return
	}
	if !found {
		return
	}
	if err := r.store.MarkProposedPlanCommentsSent(sourceThreadID, sourceItemID, commentIDs, sentAt, sentTurnID); err != nil {
		log.Printf("triage: mark proposed plan comments sent %s/%s: %v", sourceThreadID, sourceItemID, err)
		return
	}
	plan, found, err = r.store.GetThreadProposedPlanItem(sourceThreadID, sourceItemID)
	if err != nil {
		log.Printf("triage: refresh proposed plan after comments sent %s/%s: %v", sourceThreadID, sourceItemID, err)
		return
	}
	if found {
		r.emit("provider:item_event", NewItemStreamUpsert(plan))
	}
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
// updates started_at if the provider re-sends EventTurnStart. An
// existing row means we've seen this turn before (re-init after
// interrupt, recovery replay) and the original started_at is the
// authoritative wall time — don't overwrite it. A missing row on
// update (sql.ErrNoRows) means a fresh insert is the right path.
// Errors are logged but not propagated: turn-start emission must fire
// even if persistence hiccups, so the frontend's working indicator
// still tracks the wire signal.
func (r *Router) upsertTurnRow(turn store.Turn) bool {
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
		return false
	}
	if err := r.store.InsertTurn(turn); err != nil {
		log.Printf("triage: turn start insert %s: %v", turn.TurnID, err)
		return false
	}
	return true
}

// openTurnSpan begins a turn.lifecycle span for the incoming turn. Any
// existing span for the thread is closed first — the provider sometimes
// re-sends EventTurnStart (e.g. after a Claude interrupt/re-init) and we
// don't want to leak orphan spans.
func (r *Router) openTurnSpan(evt provider.ProviderEvent) {
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
	turnIndex, _ := r.store.LastTurnIndex(evt.ThreadID)
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

// captureBaselineForTurn runs checkpoint capture + SQLite persistence for the
// current turn boundary. Errors are logged and surfaced as activity events so the UI
// can show "checkpoints unavailable" without blocking the turn.
//
// The operation is idempotent by design. A provider can fire EventTurnStart
// more than once for the same (thread, turn) — e.g. Claude resending
// system.init after an interrupt — and an earlier capture attempt can have
// partially succeeded (git ref written but DB insert failed, or vice versa).
// Before writing a fresh pair we tear down any stale row + ref so we never
// have to reconcile drift between git and SQLite later. If SaveCheckpoint
// fails after the new ref is written, we remove the new ref so the two
// sides stay in lockstep — either both exist, or neither does.
func (r *Router) captureBaselineForTurn(ctx context.Context, threadID string, currentTurnIndex int) {
	cap := r.checkpointStore()
	if cap == nil {
		return
	}
	if currentTurnIndex != 0 {
		return
	}
	thread, err := r.store.GetThread(threadID)
	if err != nil {
		log.Printf("triage: checkpoint load thread %s: %v", threadID, err)
		return
	}
	workspace := checkpointWorkspacePath(thread)
	if workspace == "" {
		return
	}
	if !cap.IsGitRepository(ctx, workspace) {
		r.emit("checkpoint:unavailable", map[string]any{
			"threadId": threadID,
			"reason":   "not-a-git-repo",
		})
		return
	}
	turnCount := 0
	if r.markTurnCaptured(threadID, turnCount) {
		return // already captured for this (thread, turn)
	}

	// Idempotency guard: any pre-existing row for (thread, turn) means a
	// previous capture partially succeeded or we're re-capturing. Drop both
	// sides — DB row first (SQLite write is the cheap one to roll back),
	// then the backing git ref — so the upcoming capture starts clean.
	if staleRef, hadRow, err := r.store.DeleteCheckpointByThreadTurnCount(threadID, turnCount); err != nil {
		log.Printf("triage: checkpoint stale row thread=%s turn=%d: %v", threadID, turnCount, err)
		r.unmarkTurnCaptured(threadID, turnCount)
		return
	} else if hadRow && staleRef.RefName != "" {
		if err := cap.DeleteRef(ctx, staleRef.WorkspacePath, staleRef.RefName); err != nil {
			log.Printf("triage: checkpoint delete stale ref %s: %v", staleRef.RefName, err)
		}
	}

	ref, err := cap.CaptureBaseline(ctx, workspace, threadID, turnCount)
	if err != nil {
		r.unmarkTurnCaptured(threadID, turnCount)
		r.emit("checkpoint:error", map[string]any{
			"threadId":            threadID,
			"turnIndex":           turnCount,
			"checkpointTurnCount": turnCount,
			"error":               err.Error(),
		})
		log.Printf("triage: checkpoint capture thread=%s turn=%d: %v", threadID, turnCount, err)
		return
	}
	now := time.Now().UnixMilli()
	record := store.Checkpoint{
		ID:                  uuid.NewString(),
		ThreadID:            threadID,
		TurnIndex:           turnCount,
		CheckpointTurnCount: turnCount,
		RefName:             ref,
		Status:              "ready",
		CapturedAt:          now,
		WorkspacePath:       workspace,
	}
	if err := r.store.SaveCheckpoint(record); err != nil {
		log.Printf("triage: checkpoint persist thread=%s turn=%d: %v", threadID, turnCount, err)
		// DB row never landed — don't let the git ref linger.
		if derr := cap.DeleteRef(ctx, workspace, ref); derr != nil {
			log.Printf("triage: checkpoint rollback ref %s: %v", ref, derr)
		}
		r.unmarkTurnCaptured(threadID, turnCount)
		return
	}
	r.emit("checkpoint:captured", map[string]any{
		"threadId":            threadID,
		"turnIndex":           turnCount,
		"checkpointTurnCount": turnCount,
		"refName":             ref,
		"capturedAt":          now,
	})
}

// capturePriorTurnCheckpoint reconstructs a TurnCompletedEvent from the
// stored prior-turn row and runs the completion checkpoint capture
// against it. Called from handleTurnStart at user-send time so the
// captured state reflects the working tree at the moment the user
// committed to a new prompt — including any post-turn-end writes from
// the multi-result-per-turn case and any manual edits between turns.
//
// A missing prior turn row (e.g. crash recovery resuming on a thread
// whose first turn never wrote a turns row) is non-fatal: skip the
// capture, leave the prior-turn checkpoint as whatever already exists
// (typically the baseline). The next user-send still captures whatever
// state exists at that moment for the next prior turn.
//
// Idempotent against re-fired EventTurnStart for the same turnIndex
// (Claude `system.init` resend after interrupt). The first call marks
// the prior turn captured; subsequent calls for the same prior turn
// no-op rather than re-running multiple sequential git commands and
// wastefully replacing an already-correct checkpoint. The marker
// clears at CleanupThread.
func (r *Router) capturePriorTurnCheckpoint(ctx context.Context, threadID string, priorTurnIndex int) {
	if priorTurnIndex < 0 {
		return
	}
	// Guard on (priorTurnIndex+1) — capturedTurns is keyed by
	// checkpointTurnCount (= priorTurnIndex+1), matching the keying
	// already used for baseline (turnCount=0).
	if r.markTurnCaptured(threadID, priorTurnIndex+1) {
		return
	}
	turn, found, err := r.store.GetTurnByThreadIndex(threadID, priorTurnIndex)
	if err != nil {
		log.Printf("triage: prior-turn checkpoint lookup thread=%s turn=%d: %v", threadID, priorTurnIndex, err)
		r.unmarkTurnCaptured(threadID, priorTurnIndex+1)
		return
	}
	if !found {
		r.unmarkTurnCaptured(threadID, priorTurnIndex+1)
		return
	}
	completedAt := int64(0)
	if turn.CompletedAt != nil {
		completedAt = *turn.CompletedAt
	}
	settled := TurnCompletedEvent{
		ThreadID:           threadID,
		TurnID:             turn.TurnID,
		TurnIndex:          priorTurnIndex,
		StartedAt:          turn.StartedAt,
		CompletedAt:        completedAt,
		StopReason:         turn.StopReason,
		AssistantMessageID: turn.AssistantMessageID,
		ErrorMessage:       turn.ErrorMessage,
		Aborted:            turn.StopReason == "interrupted",
	}
	if err := r.captureCompletedTurnCheckpoint(ctx, settled); err != nil {
		// Roll back the dedup mark on failure so a subsequent re-fired
		// EventTurnStart can retry the capture instead of being silently
		// skipped. This matches captureBaselineForTurn's per-failure
		// unmark pattern and preserves the prior behavior where every
		// EventTurnComplete would retry the checkpoint.
		r.unmarkTurnCaptured(threadID, priorTurnIndex+1)
	}
}

// captureCompletedTurnCheckpoint persists the post-turn checkpoint
// for `settled`. Returns nil on success (including the soft-skip cases
// where there's no checkpoint store, no workspace, or the workspace
// isn't a git repo — those are intentional no-ops, not failures).
// Returns a non-nil error when capture was attempted and failed; the
// caller (capturePriorTurnCheckpoint) uses that to roll back its
// dedup mark so a subsequent re-fired EventTurnStart can retry the
// capture instead of being silently skipped.
func (r *Router) captureCompletedTurnCheckpoint(ctx context.Context, settled TurnCompletedEvent) error {
	cap := r.checkpointStore()
	if cap == nil {
		return nil
	}
	thread, err := r.store.GetThread(settled.ThreadID)
	if err != nil {
		log.Printf("triage: checkpoint complete load thread %s: %v", settled.ThreadID, err)
		return fmt.Errorf("checkpoint complete load thread: %w", err)
	}
	workspace := checkpointWorkspacePath(thread)
	if workspace == "" || !cap.IsGitRepository(ctx, workspace) {
		return nil
	}

	turnCount := settled.TurnIndex + 1
	if turnCount < 1 {
		return nil
	}
	previous, ok, err := r.store.GetCheckpointByTurnCount(settled.ThreadID, turnCount-1)
	if err != nil {
		log.Printf("triage: checkpoint complete previous thread=%s turn=%d: %v", settled.ThreadID, turnCount-1, err)
		return fmt.Errorf("checkpoint complete previous: %w", err)
	}
	hasPrevious := ok

	if staleRef, hadRow, err := r.store.DeleteCheckpointByThreadTurnCount(settled.ThreadID, turnCount); err != nil {
		log.Printf("triage: checkpoint complete stale row thread=%s turn=%d: %v", settled.ThreadID, turnCount, err)
		return fmt.Errorf("checkpoint complete stale row: %w", err)
	} else if hadRow && staleRef.RefName != "" {
		if err := cap.DeleteRef(ctx, staleRef.WorkspacePath, staleRef.RefName); err != nil {
			log.Printf("triage: checkpoint complete delete stale ref %s: %v", staleRef.RefName, err)
		}
	}

	ref, err := cap.CaptureBaseline(ctx, workspace, settled.ThreadID, turnCount)
	if err != nil {
		r.emit("checkpoint:error", map[string]any{
			"threadId":            settled.ThreadID,
			"turnIndex":           turnCount,
			"checkpointTurnCount": turnCount,
			"error":               err.Error(),
		})
		log.Printf("triage: checkpoint complete capture thread=%s turn=%d: %v", settled.ThreadID, turnCount, err)
		return fmt.Errorf("checkpoint complete capture: %w", err)
	}
	var files []diffsummary.File
	if hasPrevious {
		files, err = cap.DiffRefToRefSummary(ctx, workspace, previous.RefName, ref)
		if err != nil {
			log.Printf("triage: checkpoint complete diff summary thread=%s turn=%d: %v", settled.ThreadID, turnCount, err)
			files = nil
		}
	}
	// Drain the per-turn committed-paths set and normalize against the
	// workspace. Deduped + sorted so the persisted JSON is deterministic.
	// Use settled.TurnIndex (the original index from the provider)
	// because path tracking keys on that, not on turnCount which is
	// settled+1. The accumulator carries the cumulative paths
	// (including any from the multi-result-per-turn second half — the
	// settleToolPaths fallback in router.go keeps them flowing into
	// committedToolPaths after openTurns is cleared) because
	// committedToolPaths is intentionally NOT swept by clearOpenTurn.
	rawPaths := r.drainCommittedToolPaths(settled.ThreadID, settled.TurnIndex)
	toolPaths := normalizeWorkspaceRelativePaths(rawPaths, workspace)
	now := time.Now().UnixMilli()
	record := store.Checkpoint{
		ID:                  uuid.NewString(),
		ThreadID:            settled.ThreadID,
		TurnIndex:           turnCount,
		CheckpointTurnCount: turnCount,
		TurnID:              settled.TurnID,
		RefName:             ref,
		Status:              checkpointStatusForTurn(settled),
		Files:               files,
		ToolPaths:           toolPaths,
		AssistantMessageID:  settled.AssistantMessageID,
		CompletedAt:         settled.CompletedAt,
		CapturedAt:          now,
		WorkspacePath:       workspace,
	}
	if err := r.store.SaveCheckpoint(record); err != nil {
		log.Printf("triage: checkpoint complete persist thread=%s turn=%d: %v", settled.ThreadID, turnCount, err)
		if derr := cap.DeleteRef(ctx, workspace, ref); derr != nil {
			log.Printf("triage: checkpoint complete rollback ref %s: %v", ref, derr)
		}
		return fmt.Errorf("checkpoint complete persist: %w", err)
	}
	r.emit("checkpoint:updated", map[string]any{
		"threadId":            settled.ThreadID,
		"turnIndex":           turnCount,
		"checkpointTurnCount": turnCount,
		"refName":             ref,
		"files":               files,
		"capturedAt":          now,
	})
	return nil
}

func checkpointStatusForTurn(settled TurnCompletedEvent) string {
	if settled.Aborted {
		return "interrupted"
	}
	if strings.TrimSpace(settled.ErrorMessage) != "" || settled.StopReason == "error" {
		return "error"
	}
	return "ready"
}

func (r *Router) checkpointStore() CheckpointCapture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkpoints
}

func (r *Router) markTurnCaptured(threadID string, turnIndex int) bool {
	key := fmt.Sprintf("%s|%d", threadID, turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capturedTurns[key] {
		return true
	}
	r.capturedTurns[key] = true
	return false
}

func (r *Router) unmarkTurnCaptured(threadID string, turnIndex int) {
	key := fmt.Sprintf("%s|%d", threadID, turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.capturedTurns, key)
}

// checkpointWorkspacePath picks the on-disk directory we should snapshot.
// WorkspacePath is the effective provider cwd and may be the project root or a
// registered worktree.
func checkpointWorkspacePath(t store.Thread) string {
	return t.WorkspacePath
}

func (r *Router) handleTurnComplete(evt provider.ProviderEvent) error {
	// Idempotent guard: a second EventTurnComplete on a turn that was
	// ALREADY settled (provider:turn_completed already emitted, checkpoint
	// already captured) is a wire-level artifact. Known sources:
	//
	//   1. Claude task_notification → CLI synthesizes a `type:"user"`
	//      envelope → model emits another response → second `result`
	//      envelope. Both `result`s belong to one logical agent-overflow
	//      turn from the user's perspective; the first close already
	//      drained streaming items, captured the per-turn checkpoint,
	//      and emitted provider:turn_completed.
	//   2. handleError synthesizes a EventTurnComplete{truncated:true},
	//      then a real wire EventTurnComplete arrives anyway because the
	//      subprocess kept streaming.
	//
	// We can't gate on "openTurns is empty" alone because handleError's
	// fatal-error branch deliberately clears the open turn BEFORE
	// synthesizing — that synthesized complete is the FIRST settle for
	// the turn and must run in full. So we track per-turn settled
	// markers separately from openTurns; this guard only fires for the
	// SECOND complete on the same (threadID, turnIndex).
	//
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	meta := decodeTurnCompleteMeta(evt.Meta)
	if err == nil {
		// markTurnSettled is sticky on partial failure (same trade-off as
		// markTurnCaptured): once the FIRST handleTurnComplete reaches
		// here, subsequent invocations for the same (thread, turn) skip
		// lifecycle settlement. Late usage on a duplicate is still folded
		// into the existing turn row below so accounting is not lost.
		if r.markTurnSettled(evt.ThreadID, turnIndex) {
			r.persistLateTurnUsage(evt, turnIndex, meta)
			return nil
		}
	}
	var persistErr error
	now := eventTimestampMillis(evt)
	truncated := turnCompleteIsTruncated(meta)
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

	// Codex background-terminal catchall: any inProgress unifiedExec
	// items or spawn_agent rows with running children that reached the
	// turn boundary without an earlier yield get stamped is_background
	// here. Must run BEFORE forceCloseOrphanToolCalls so the invariant-24
	// exemption (background rows skip force-close) kicks in on the
	// newly-stamped rows — otherwise forceClose would flip them to
	// errored a moment before we mark them backgrounded.
	//
	// Truncated turns (user-Esc) run this too, even though it looks like
	// a "termination not a yield". Reason: Codex's process model says
	// `Op::Interrupt` does NOT kill unifiedExec PTYs or spawn_agent
	// collab threads. `core/src/tasks/mod.rs:632-637` — terminating
	// PTYs is a separate `Op::CleanBackgroundTerminals` that
	// `abort_all_tasks` deliberately does not invoke (mirrors the
	// Codex TUI's Esc behaviour, which fires only `Op::Interrupt`).
	// So a pre-yield unifiedExec started this turn is genuinely still
	// running on disk after Esc; stamping it backgrounded here keeps
	// the tray accurate and lets the user kill it via the Stop All
	// button (which calls `thread/backgroundTerminals/clean`, the
	// only Codex per-thread primitive — there's no per-row stop for
	// unifiedExec until upstream adds one). spawn_agent rows already
	// stamp at item/completed (codex_background.go:884-886), so this
	// catchall only does work for unifiedExec items.
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

	// Settle the turns row + emit provider:turn_completed even on
	// persist error so the frontend's working indicator clears.
	// Persistence failures are logged inside settleTurnRow; we keep
	// persistErr as the return so upstream (observability, error
	// reporting) sees the real failure.
	//
	// Checkpoint capture for THIS turn does NOT happen here. It runs at
	// the next handleTurnStart (capturePriorTurnCheckpoint), matching
	// Claude Code's user-prompt-submit snapshot pattern. The turn-end
	// emission still fires so the frontend can flip its working
	// indicator off; the diff panel surfaces "what changed in this
	// turn" by diffing the prior baseline against the current working
	// tree until the next user-send commits a checkpoint.
	settled := r.settleTurnRow(evt, turnIndex, now, meta, persistErr)
	r.emit("provider:turn_completed", settled)

	r.clearOpenTurn(evt.ThreadID)
	r.closeTurnSpan(evt.ThreadID, persistErr)
	return persistErr
}

// turnCompleteIsTruncated reports whether the turn ended via
// interruption / truncation. See decodeTurnCompleteMeta for the wire
// shapes this accepts.
func turnCompleteIsTruncated(meta turnCompleteMeta) bool {
	return meta.Truncated || meta.Aborted || meta.TurnStatus == "interrupted"
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

// settleTurnRow updates the turns row and returns the frontend-facing
// TurnCompletedEvent payload. Persists on best-effort: a persistence
// failure logs but still yields a payload so the wire emission fires.
// Missing turn rows (e.g. a turn_complete with no matching turn_start
// because the first event arrived mid-crash-recovery) are tolerated —
// the UPDATE's sql.ErrNoRows is logged and the payload still emits.
func (r *Router) settleTurnRow(evt provider.ProviderEvent, turnIndex int, now int64, meta turnCompleteMeta, persistErr error) TurnCompletedEvent {
	stopReason := canonicalStopReason(meta)
	if persistErr != nil && stopReason == "" {
		stopReason = "error"
	}
	assistantMessageID := resolveAssistantMessageID(meta)
	errorMessage := meta.Error
	if errorMessage == "" && persistErr != nil {
		errorMessage = persistErr.Error()
	}
	turnID := resolveTurnID(evt, turnIndex)

	// Lookup started_at. A persisted row is the authoritative clock;
	// fall back to `now` so the payload always carries a sensible
	// StartedAt if the turns row was never written.
	startedAt := now
	if existing, found, err := r.store.GetTurn(turnID); err == nil && found {
		startedAt = existing.StartedAt
	}

	usageJSON := ""
	if len(meta.Usage) > 0 {
		usageJSON = string(meta.Usage)
	}

	if err := r.store.UpdateTurnCompleted(turnID, now, stopReason, assistantMessageID, usageJSON, errorMessage); err != nil {
		log.Printf("triage: update turn %s: %v", turnID, err)
	}

	return TurnCompletedEvent{
		ThreadID:           evt.ThreadID,
		TurnID:             turnID,
		TurnIndex:          turnIndex,
		StartedAt:          startedAt,
		CompletedAt:        now,
		StopReason:         stopReason,
		AssistantMessageID: assistantMessageID,
		TokenUsage:         meta.Usage,
		ErrorMessage:       errorMessage,
		Aborted:            meta.Aborted || meta.Truncated || meta.TurnStatus == "interrupted",
	}
}

func (r *Router) persistLateTurnUsage(evt provider.ProviderEvent, turnIndex int, meta turnCompleteMeta) {
	if len(meta.Usage) == 0 {
		return
	}
	turnID := resolveTurnID(evt, turnIndex)
	if err := r.store.UpdateTurnTokenUsageIfEmpty(turnID, string(meta.Usage)); err != nil {
		log.Printf("triage: update turn %s late token usage: %v", turnID, err)
	}
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

func (r *Router) setOpenTurn(threadID string, turnIndex int) {
	r.mu.Lock()
	r.openTurns[threadID] = turnIndex
	key := scopeCounterKey(threadID, turnIndex, "")
	r.segmentIndexByScope[key] = -1
	r.blockIndexByScope[key] = -1
	delete(r.activeTextBlocks, key)
	delete(r.activeThinkingBlocks, key)
	delete(r.errorSeqByScope, key)
	// Clear the settled marker so a re-init (Claude resend system.init
	// after an interrupt; Codex resend turn/started) can settle the
	// same turn again. The multi-result-per-turn case does NOT re-fire
	// EventTurnStart between the two completes, so the marker survives
	// there and the second complete returns early.
	delete(r.settledTurns, settledTurnKey(threadID, turnIndex))
	r.mu.Unlock()
}

// markTurnSettled records that handleTurnComplete has already run to
// completion for (threadID, turnIndex). Returns true if the turn was
// ALREADY settled (caller should bail early), false if this is the
// first settle (caller proceeds with the full handler).
func (r *Router) markTurnSettled(threadID string, turnIndex int) bool {
	key := settledTurnKey(threadID, turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settledTurns[key] {
		return true
	}
	r.settledTurns[key] = true
	return false
}

func settledTurnKey(threadID string, turnIndex int) string {
	return fmt.Sprintf("%s|%d", threadID, turnIndex)
}

// clearOpenTurn drops per-turn flow-control state at EventTurnComplete.
//
// Three distinct lifecycles intersect here and MUST stay separate (see
// internal/triage/AGENTS.md "Correlation state" for the full taxonomy):
//
//   - **Per-turn flow-control state** swept HERE: openTurns,
//     interruptQueue, streamingItemCounts, activeTextBlocks/Thinking,
//     pendingCommandDiffs, pendingToolPaths, pendingApprovals (and
//     siblings). These maps answer "what's mid-turn right now."
//   - **Id-allocating counters** (segmentIndexByScope, blockIndexByScope,
//     errorSeqByScope, terminalInteractionSeq): cleared at CleanupThread,
//     with a selective re-init reset in setOpenTurn. Wiping HERE would
//     cause id collisions when the wire emits two `result` envelopes
//     for one logical turn (Claude task_notification → CLI-synthesized
//     `user` envelope → second result, fatal-error synthetic-truncate
//     then real wire complete) — see the regression coverage in
//     multi_result_test.go.
//   - **User-send-time carry-over state** (committedToolPaths,
//     settledTurns, capturedTurns): survives turn-end by design.
//     committedToolPaths drains at the next user-send when
//     capturePriorTurnCheckpoint runs (matching Claude Code's
//     fileHistoryMakeSnapshot at user-prompt-submit). settledTurns
//     clears in setOpenTurn (so a re-init can re-settle) and
//     CleanupThread.
//
// activeTextBlocks/Thinking ARE per-turn flow-control (they guard
// against re-creating a row mid-stream), so they stay swept here as a
// safety net for any block that didn't settle through the normal close
// path.
func (r *Router) clearOpenTurn(threadID string) {
	r.mu.Lock()
	turnIndex, ok := r.openTurns[threadID]
	if ok {
		prefix := fmt.Sprintf("%s|%d|", threadID, turnIndex)
		deleteByPrefix(r.activeTextBlocks, prefix)
		deleteByPrefix(r.activeThinkingBlocks, prefix)
		// committedToolPaths is NOT swept here. It survives turn-end
		// because the prior-turn capture happens at the NEXT turn-start
		// (capturePriorTurnCheckpoint, called from handleTurnStart) —
		// matching Claude Code's user-prompt-submit snapshot pattern.
		// The drain happens inside captureCompletedTurnCheckpoint when
		// the next user-send commits the prior turn's checkpoint;
		// CleanupThread sweeps the whole thread's entries when the
		// session ends without another user-send.
		//
		// pendingToolPaths is keyed by `<threadID>|<itemID>` — no turn
		// component — so a per-thread prefix-sweep is the correct
		// primitive. Any tool that started in this turn but didn't fire
		// EventToolComplete (interrupted turn, crashed provider) leaks
		// here without this sweep. Provider events are serialized per
		// thread, so a tool from a future turn cannot have entered the
		// map yet.
		deleteByPrefix(r.pendingToolPaths, threadID+"|")
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
	}
	delete(r.openTurns, threadID)
	delete(r.interruptQueue, threadID)
	delete(r.streamingItemCounts, threadID)
	// openAPIRetryRows tracks "thread has a running api_retry row that
	// still needs flipping". By turn-end the row was either flipped
	// already or the turn closed without forward progress; either way
	// the next turn starts with a clean flag.
	delete(r.openAPIRetryRows, threadID)
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

// Wait blocks until every in-flight Handle call has returned, or until
// ctx is cancelled. Shutdown uses this to drain the router before
// flushing observability writers; a completed Wait means the store and
// event emit have seen the last event. Callers that pass a deadlined
// ctx get a context.DeadlineExceeded when the drain runs long.
func (r *Router) Wait(ctx context.Context) error {
	if r == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return r.flushAllStreamPersistence()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CleanupThread removes all accumulator state for a thread. Call this when a
// session ends or disconnects to prevent memory leaks. Also flags the
// thread as "stopped" so any event that arrives afterward — typically a
// readLoop line that was already in-flight when StopSession returned — is
// dropped instead of persisting under the torn-down session (Bug B5).
//
// As a safety net, any open turn at cleanup time gets a synthesized
// truncated EventTurnComplete so the frontend working indicator clears
// even when the session ends without a wire turn-complete (clean stdout
// EOF, host-side StopSession during a turn, etc). This pairs with
// session_status.go's handleSessionDied — which path runs first
// depends on whether `EventSessionStatus{"error"}` arrives before
// or after StopSession unwinds — and is idempotent in either order
// thanks to markTurnSettled.
func (r *Router) CleanupThread(threadID string) {
	if _, ok := r.openTurnIndex(threadID); ok {
		if err := r.synthesizeTruncatedTurnComplete(threadID, time.Now().UnixMilli()); err != nil {
			log.Printf("triage: synthesize turn-complete on cleanup for thread %s: %v", threadID, err)
		}
	}
	if err := r.flushStreamingThread(threadID); err != nil {
		log.Printf("triage: cleanup flush stream buffers for thread %s: %v", threadID, err)
	}

	r.mu.Lock()
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
	for key, pending := range r.pendingCommandDiffs {
		if pending.ThreadID == threadID {
			delete(r.pendingCommandDiffs, key)
		}
	}
	approvalPrefix := threadID + ":"
	deleteByPrefix(r.pendingApprovals, approvalPrefix)
	deleteByPrefix(r.pendingApprovalItems, approvalPrefix)
	deleteByPrefix(r.pendingUserInputs, approvalPrefix)
	prefix := threadID + "|"
	deleteByPrefix(r.capturedTurns, prefix)
	deleteByPrefix(r.segmentIndexByScope, prefix)
	deleteByPrefix(r.blockIndexByScope, prefix)
	deleteByPrefix(r.activeTextBlocks, prefix)
	deleteByPrefix(r.activeThinkingBlocks, prefix)
	deleteByPrefix(r.errorSeqByScope, prefix)
	deleteByPrefix(r.notificationSeqByScope, prefix)
	for key := range r.streamPersistBuffers {
		if strings.HasPrefix(key, prefix) {
			if buffer := r.streamPersistBuffers[key]; buffer != nil && buffer.timer != nil {
				buffer.timer.Stop()
			}
			delete(r.streamPersistBuffers, key)
		}
	}
	deleteByPrefix(r.terminalInteractionSeq, prefix)
	deleteByPrefix(r.settledTurns, prefix)
	// Drop tool-path tracking for the thread. pendingToolPaths is
	// keyed by `<threadID>|<itemID>` and committedToolPaths by
	// `<threadID>|<turnIndex>` — both share the same `<threadID>|`
	// prefix.
	deleteByPrefix(r.pendingToolPaths, prefix)
	deleteByPrefix(r.committedToolPaths, prefix)
	delete(r.openAPIRetryRows, threadID)
	// Drop the Codex background projector's per-thread trackers. A
	// restarted session never inherits trackers from a prior session —
	// the wire replays item/started for still-inProgress items and the
	// projector re-observes them fresh. Keeping stale trackers would
	// cause the first yield in the new session to stamp a row that
	// belongs to an entirely different process id.
	delete(r.codexBackground, threadID)
	r.mu.Unlock()

	if orphanSpan != nil {
		// Closing outside the lock avoids self-deadlock when the tracer's
		// OnEnd hook reaches for any shared resource.
		orphanSpan.End()
	}
}

// isThreadStopped returns true when CleanupThread has been called for
// threadID and no subsequent EventInit has re-activated it.
func (r *Router) isThreadStopped(threadID string) bool {
	r.mu.Lock()
	_, stopped := r.stoppedThreads[threadID]
	r.mu.Unlock()
	return stopped
}

// markThreadActive clears the stopped flag, called on EventInit so a
// restarted session can persist again. No-op when the flag was already
// clear.
func (r *Router) markThreadActive(threadID string) {
	r.mu.Lock()
	delete(r.stoppedThreads, threadID)
	r.mu.Unlock()
}
