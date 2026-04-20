package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// handleTurnStart forwards the event inline (so the UI gets the turn marker)
// and, if a checkpoint store is wired up, captures a baseline snapshot of the
// workspace. Checkpoint failure is NOT fatal to the turn — it surfaces as a
// `provider:event_error` event the frontend can display, but the turn proceeds.
func (r *Router) handleTurnStart(evt provider.ProviderEvent) error {
	r.emit("provider:event", evt)
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
	r.captureBaselineForTurn(context.Background(), evt.ThreadID)
	return nil
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
// current turn. Errors are logged and surfaced as activity events so the UI
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
func (r *Router) captureBaselineForTurn(ctx context.Context, threadID string) {
	cap := r.checkpointStore()
	if cap == nil {
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
	turnIndex, err := r.store.LastTurnIndex(threadID)
	if err != nil {
		log.Printf("triage: checkpoint turn index %s: %v", threadID, err)
		return
	}
	if r.markTurnCaptured(threadID, turnIndex) {
		return // already captured for this (thread, turn)
	}

	// Idempotency guard: any pre-existing row for (thread, turn) means a
	// previous capture partially succeeded or we're re-capturing. Drop both
	// sides — DB row first (SQLite write is the cheap one to roll back),
	// then the backing git ref — so the upcoming capture starts clean.
	if staleRef, hadRow, err := r.store.DeleteCheckpointByThreadTurn(threadID, turnIndex); err != nil {
		log.Printf("triage: checkpoint stale row thread=%s turn=%d: %v", threadID, turnIndex, err)
		r.unmarkTurnCaptured(threadID, turnIndex)
		return
	} else if hadRow && staleRef != "" {
		if err := cap.DeleteRef(ctx, workspace, staleRef); err != nil {
			log.Printf("triage: checkpoint delete stale ref %s: %v", staleRef, err)
		}
	}

	ref, err := cap.CaptureBaseline(ctx, workspace, threadID, turnIndex)
	if err != nil {
		r.unmarkTurnCaptured(threadID, turnIndex)
		r.emit("checkpoint:error", map[string]any{
			"threadId":  threadID,
			"turnIndex": turnIndex,
			"error":     err.Error(),
		})
		log.Printf("triage: checkpoint capture thread=%s turn=%d: %v", threadID, turnIndex, err)
		return
	}
	now := time.Now().UnixMilli()
	record := store.Checkpoint{
		ID:            uuid.NewString(),
		ThreadID:      threadID,
		TurnIndex:     turnIndex,
		RefName:       ref,
		CapturedAt:    now,
		WorkspacePath: workspace,
	}
	if err := r.store.SaveCheckpoint(record); err != nil {
		log.Printf("triage: checkpoint persist thread=%s turn=%d: %v", threadID, turnIndex, err)
		// DB row never landed — don't let the git ref linger.
		if derr := cap.DeleteRef(ctx, workspace, ref); derr != nil {
			log.Printf("triage: checkpoint rollback ref %s: %v", ref, derr)
		}
		r.unmarkTurnCaptured(threadID, turnIndex)
		return
	}
	r.emit("checkpoint:captured", map[string]any{
		"threadId":   threadID,
		"turnIndex":  turnIndex,
		"refName":    ref,
		"capturedAt": now,
	})
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
// Prefers worktree_path (where the agent actually edits) over workspace_path.
func checkpointWorkspacePath(t store.Thread) string {
	if t.WorktreePath != "" {
		return t.WorktreePath
	}
	return t.WorkspacePath
}

func (r *Router) handleTurnComplete(evt provider.ProviderEvent) error {
	var persistErr error
	now := eventTimestampMillis(evt)
	truncated := turnCompleteIsTruncated(evt.Meta)
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
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
	// The captured-turn guard only needs to hold for the duration of
	// this turn — once the turn is complete the baseline checkpoint is
	// already persisted and no further EventTurnStart can re-fire for
	// the same (thread, turn). Dropping the entry here keeps
	// capturedTurns bounded by concurrency rather than by session
	// lifetime, which mattered for long-running sessions that would
	// otherwise accumulate one entry per turn until CleanupThread.
	r.unmarkTurnCaptured(evt.ThreadID, turnIndex)
	r.clearOpenTurn(evt.ThreadID)
	r.closeTurnSpan(evt.ThreadID, persistErr)
	r.emit("provider:event", evt)
	return persistErr
}

func turnCompleteIsTruncated(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	if truncated, _ := meta["truncated"].(bool); truncated {
		return true
	}
	if aborted, _ := meta["aborted"].(bool); aborted {
		return true
	}
	if turnStatus, _ := meta["turn_status"].(string); turnStatus == "interrupted" {
		return true
	}
	return false
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

func (r *Router) setOpenTurn(threadID string, turnIndex int) {
	r.mu.Lock()
	r.openTurns[threadID] = turnIndex
	key := scopeCounterKey(threadID, turnIndex, "")
	r.segmentIndexByScope[key] = -1
	r.blockIndexByScope[key] = -1
	delete(r.activeTextBlocks, key)
	delete(r.activeThinkingBlocks, key)
	delete(r.errorSeqByScope, key)
	r.mu.Unlock()
}

func (r *Router) clearOpenTurn(threadID string) {
	r.mu.Lock()
	turnIndex, ok := r.openTurns[threadID]
	if ok {
		prefix := fmt.Sprintf("%s|%d|", threadID, turnIndex)
		deleteByPrefix(r.segmentIndexByScope, prefix)
		deleteByPrefix(r.blockIndexByScope, prefix)
		deleteByPrefix(r.activeTextBlocks, prefix)
		deleteByPrefix(r.activeThinkingBlocks, prefix)
		deleteByPrefix(r.errorSeqByScope, prefix)
	}
	delete(r.openTurns, threadID)
	delete(r.interruptQueue, threadID)
	delete(r.streamingItemCounts, threadID)
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
	items, err := r.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return fmt.Errorf("error flip list turn items: %w", err)
	}
	for _, item := range items {
		if item.Status != statusRunning && item.Status != "streaming" {
			continue
		}
		item.Status = statusErrored
		item.Summary = interruptedSummary(item.Summary)
		item.UpdatedAt = now
		if err := r.persistItem(item, nil); err != nil {
			return fmt.Errorf("error flip item %s: %w", item.ID, err)
		}
	}
	return nil
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
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CleanupThread removes all accumulator state for a thread. Call this when a
// session ends or disconnects to prevent memory leaks. Also flags the
// thread as "stopped" so any event that arrives afterward — typically a
// readLoop line that was already in-flight when StopSession returned — is
// dropped instead of persisting under the torn-down session (Bug B5).
func (r *Router) CleanupThread(threadID string) {
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
	prefix := threadID + "|"
	deleteByPrefix(r.capturedTurns, prefix)
	deleteByPrefix(r.segmentIndexByScope, prefix)
	deleteByPrefix(r.blockIndexByScope, prefix)
	deleteByPrefix(r.activeTextBlocks, prefix)
	deleteByPrefix(r.activeThinkingBlocks, prefix)
	deleteByPrefix(r.errorSeqByScope, prefix)
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
