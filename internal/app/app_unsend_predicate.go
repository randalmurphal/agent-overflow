package app

import (
	"fmt"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// evaluateInterruptRevertPredicate runs the backend revert eligibility
// check. Returns (true, userItem, "", nil) when the newest turn is a send
// that has not settled yet and holds nothing but that message and
// unsendTurnCompanionKinds rows, and no queued or background work would be
// lost. Otherwise returns the reason the predicate declined so callers can
// log / emit it.
//
// Predicate (the frontend's canRevertEarlyInterrupt mirrors it):
//   - The newest turn (LastTurnIndex) has never settled. Once any round of
//     it completes (an earlier plain Stop, a finished round) the message is
//     committed history; a later round on the same turn index, such as the
//     CLI answering a background task notification, does not make it
//     undoable again.
//   - That turn passes unsendTurnMessage.
//   - The triage flush queue is empty for the thread (a queued
//     follow-up means Stop should let the queue drain through, not
//     discard everything).
//   - No background task is running in the tray. Reverting shuts down the
//     provider thread runtime, which kills background work; early Stop should
//     preserve that work and fall back to a plain interrupt.
func (a *App) evaluateInterruptRevertPredicate(threadID string) (bool, store.Item, string, error) {
	hasItems, err := a.store.HasItems(threadID)
	if err != nil {
		return false, store.Item{}, "", fmt.Errorf("has items: %w", err)
	}
	if !hasItems {
		return false, store.Item{}, "no items", nil
	}
	turnIndex, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		return false, store.Item{}, "", fmt.Errorf("last turn index: %w", err)
	}
	if turn, found, err := a.store.GetTurnByThreadIndex(threadID, turnIndex); err != nil {
		return false, store.Item{}, "", fmt.Errorf("load latest turn: %w", err)
	} else if found && turn.CompletedAt != nil {
		return false, store.Item{}, "turn already settled", nil
	}
	userItem, reason, err := a.unsendTurnMessage(threadID, turnIndex)
	if err != nil || reason != "" {
		return false, store.Item{}, reason, err
	}
	if a.pendingFlushWorkCount(threadID) > 0 {
		return false, store.Item{}, "queued follow-up messages", nil
	}
	if running, err := a.hasRunningBackgroundTasks(threadID); err != nil {
		return false, store.Item{}, "", fmt.Errorf("check background tasks: %w", err)
	} else if running {
		return false, store.Item{}, "running background tasks", nil
	}
	return true, userItem, "", nil
}

// unsendTurnCompanionKinds are the only rows that may share a turn with the
// message an early Stop un-sends. Each is either the model's unfinished
// reasoning about that message or a request-level retry or error, so cutting
// the turn loses nothing the provider conversation holds. Every other kind
// declines the un-send, including kinds added later: agent output, background
// completions and their notifications, compaction, command results and
// wire-only user rows all record content that entered the conversation.
var unsendTurnCompanionKinds = map[provider.ItemKind]bool{
	provider.ItemThinking: true,
	provider.ItemAPIRetry: true,
	provider.ItemAPIError: true,
	provider.ItemError:    true,
}

// unsendTurnMessage returns the turn's single reader-authored user message
// when every other row in the turn is an unsendTurnCompanionKinds row.
// Otherwise it returns the reason the turn cannot be un-sent.
func (a *App) unsendTurnMessage(threadID string, turnIndex int) (store.Item, string, error) {
	items, err := a.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return store.Item{}, "", fmt.Errorf("list turn items: %w", err)
	}
	var userItem store.Item
	userCount := 0
	for _, item := range items {
		if isReaderAuthoredUserItem(item) {
			userItem = item
			userCount++
			continue
		}
		if !unsendTurnCompanionKinds[provider.ItemKind(item.Kind)] {
			return store.Item{}, fmt.Sprintf("turn holds %s", item.Kind), nil
		}
	}
	if userCount == 0 {
		return store.Item{}, "no user message in latest turn", nil
	}
	if userCount > 1 {
		// Steered turns persist multiple user_text rows for one turn.
		// Reverting one of them would break the steer ordering; let
		// the plain interrupt path handle this case.
		return store.Item{}, "turn has steered user messages", nil
	}
	return userItem, "", nil
}

// isReaderAuthoredUserItem reports whether item is a top-level message the
// user sent, as opposed to a subagent prompt (parented) or a wire-only echo
// of content the provider consumed without an AO send.
func isReaderAuthoredUserItem(item store.Item) bool {
	return item.Kind == string(provider.ItemUserText) &&
		item.Role == "user" &&
		item.ParentID == "" &&
		!store.IsWireOnlyUserItem(item)
}

// pendingFlushWorkCount sums every queued / in-flight follow-up message the
// revert predicate must treat as turn-extending work. It reads three counters
// that the flush handoff updates non-atomically (triage queue length, deferred
// pending count, App-layer inflight count), so it holds a.flushDispatch.handoffMu across
// all three: the same mutex RegisterQueueItem holds across its enqueue→flush
// handoff. That makes a message mid-handoff observable here as either
// still-queued or already-in-flight, never invisible in the gap between.
//
// The lock-free boundary drains don't hold a.flushDispatch.handoffMu; for them the
// triage claim count (see tryFlushQueue) keeps a draining batch inside
// QueuedFlushItemCount until the App inflight count has it. That overlap
// only closes the gap if the triage counts are read FIRST: a batch moving
// claimed→in-flight between the reads is then double-counted, never
// zero-counted. Do not reorder these reads.
//
// Callers hold the per-thread action lock, not handoffMu. This predicate
// protects both interrupt/revert and idle eviction from losing queued work.
func (a *App) pendingFlushWorkCount(threadID string) int {
	a.flushDispatch.handoffMu.Lock()
	defer a.flushDispatch.handoffMu.Unlock()
	total := 0
	if a.triage != nil {
		total += a.triage.QueuedFlushItemCount(threadID)
		total += a.triage.DeferredPendingFlushItemCount(threadID)
	}
	total += a.flushDispatchItemCount(threadID)
	return total
}
