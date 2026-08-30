package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// InterruptAndRevertResult is returned by InterruptAndRevertIfClean.
// The frontend uses Reverted to decide whether to commit or roll back
// its optimistic UI (timeline row removal + composer rehydrate).
type InterruptAndRevertResult struct {
	// Reverted is true when the predicate matched and the user message
	// was successfully reverted. false means we fell back to a plain
	// interrupt (predicate failed under the lock or no session exists)
	// and the caller should restore any optimistic UI changes.
	Reverted bool `json:"reverted"`
	// UserItemID identifies the row that was reverted. Empty when
	// Reverted is false.
	UserItemID string `json:"userItemId,omitempty"`
	// TurnIndex is the turn the reverted user message belonged to.
	// Zero-valued when Reverted is false.
	TurnIndex int `json:"turnIndex"`
	// Reason is a short tag describing why a revert was declined.
	// Populated only when Reverted is false; useful for telemetry and
	// frontend debugging without exposing internals to the user.
	Reason string `json:"reason,omitempty"`
	// The remaining fields carry the authoritative post-commit cut also
	// emitted on user_message:reverted. Returning them lets the initiating
	// client apply the cut before it re-enables Send; the event remains the
	// cross-client/replay path. HistoryEpoch+HistoryRev make applying both
	// deliveries idempotent on the frontend.
	KeptAnchorTurnItemIDs []string `json:"keptAnchorTurnItemIds,omitempty"`
	HistoryRev            int64    `json:"historyRev"`
	HistoryEpoch          int64    `json:"historyEpoch"`
}

// UserMessageRevertedEvent is the wire payload for the
// `user_message:reverted` event emitted at the end of a successful
// conversation revert. Two callers: the Stop/Esc un-send
// (InterruptAndRevertIfClean, below) and the edit-and-resend saga
// (RevertConversationAndResendMessage), which sets DraftPendingResend.
// The frontend consumes this to truncate its timeline to match the
// SQLite cut. Idempotent on the frontend: a removal of an
// already-absent id is a no-op.
type UserMessageRevertedEvent struct {
	ThreadID   string `json:"threadId"`
	UserItemID string `json:"userItemId"`
	TurnIndex  int    `json:"turnIndex"`
	// KeptAnchorTurnItemIDs lists the anchor turn's SURVIVING items.
	// Turns after TurnIndex are always fully removed; within the anchor
	// turn the frontend keeps exactly these ids and drops everything
	// else — including pane-only rows that were never persisted. Empty
	// (the common case) means the whole anchor turn is gone: Codex cuts
	// are always turn-granular, and a Claude anchor that opens its turn
	// keeps nothing. Non-empty only for Claude item-granular cuts to a
	// mid-turn anchor (a queued/steered message sharing its turn with an
	// earlier prompt), where the kept prefix is decided by
	// DeleteConversationFromItem's promoted-row predicate — carried here
	// as data so the frontend never re-derives it.
	KeptAnchorTurnItemIDs []string `json:"keptAnchorTurnItemIds,omitempty"`
	// HistoryRev / HistoryEpoch are the thread's history stamps AFTER the
	// cut, read inside the deleting transaction
	// (docs/architecture/thread-replica-sync.md §3, §4). A client that applies
	// this event has mirrored the cut exactly, so it may adopt them and
	// keep its cached window instead of dropping it. Never adopt them on
	// an event whose removal instruction was not fully applied — an
	// overstated stamp would show stale content as fresh (§3.4).
	HistoryRev   int64 `json:"historyRev"`
	HistoryEpoch int64 `json:"historyEpoch"`
	// DraftPendingResend marks this revert as the first half of an
	// edit-and-resend saga (RevertConversationAndResendMessage): the
	// replacement message is being dispatched right behind this event.
	// The thread's persisted draft row at this instant is that saga's
	// transient crash copy — the edited text merged ahead of the user's
	// untouched composer WIP — NOT composer content, so a handler seeing
	// this flag must not rehydrate composers from it. The saga settles
	// the row itself (WIP restored on success, crash copy kept for the
	// still-open editor on failure). False on the un-send path, where
	// the draft row IS the restored composer.
	DraftPendingResend bool `json:"draftPendingResend,omitempty"`
}

// InterruptAndRevertIfClean is the unified Stop-button entry point.
// When the predicate matches under the per-thread lock — exactly one
// user_text in the latest turn, no assistant content yet, no queued
// follow-up messages — the user message is reverted (provider
// conversation rolled back, timeline truncated, composer draft
// restored, `user_message:reverted` event emitted). When the predicate
// does not match the method falls back to a plain interrupt
// (provider Interrupt + triage.MarkUserInterrupt) so the caller can
// roll back any optimistic UI it applied.
//
// The frontend pre-checks its local predicate before calling so it
// can paint instant optimistic state; this method re-checks under the
// thread lock so a Send→Stop race resolves correctly. The two
// predicates are intentionally independent — the frontend checks
// composer draft + queue + pane items; the backend checks SQLite +
// flush queue. Both must agree for revert to succeed.
func (a *App) InterruptAndRevertIfClean(threadID string) (InterruptAndRevertResult, error) {
	if a.shuttingDown.Load() {
		return InterruptAndRevertResult{}, ErrShuttingDown
	}
	if strings.TrimSpace(threadID) == "" {
		return InterruptAndRevertResult{}, errors.New("interrupt-and-revert: thread id is required")
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: load thread: %w", err)
	}

	eligible, userItem, reason, err := a.evaluateInterruptRevertPredicate(threadID)
	if err != nil {
		return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: predicate: %w", err)
	}
	if !eligible {
		// Frontend predicate disagreed (race) or queue carries
		// follow-up intent. Fall back to plain interrupt semantics so
		// the user's Stop click still takes effect. The caller will
		// undo any optimistic timeline / composer state.
		if err := a.runPlainInterruptLocked(threadID); err != nil {
			return InterruptAndRevertResult{Reverted: false, Reason: reason}, err
		}
		return InterruptAndRevertResult{Reverted: false, Reason: reason}, nil
	}

	promptDraft, err := composerdraft.FromUserItem(threadID, userItem, time.Now().UnixMilli())
	if err != nil {
		return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: build prompt draft: %w", err)
	}

	markedReverted := false
	if a.triage != nil {
		a.triage.MarkTurnReverted(threadID)
		markedReverted = true
	}

	// Best-effort provider interrupt before tearing down. For Claude
	// this aborts the in-flight model call; for Codex it cancels the
	// turn at the app-server. MarkTurnReverted happens before this call
	// because Codex can synchronously emit turn/completed while handling
	// the interrupt response; that completion must be tagged as a revert.
	if sess, ok := a.sessionManager().get(threadID); ok {
		if providerSess := sess.providerSession(); providerSess != nil {
			if err := providerSess.Interrupt(context.Background()); err != nil {
				log.Printf("app: interrupt-and-revert: provider interrupt: %v", err)
			}
		}
	}

	// Resolve the message anchor for the provider-rollback helpers.
	// When the at-send record didn't land, we synthesize one from the
	// item row — Claude session-fork and Codex fork-at-turn read
	// TurnIndex plus the provider ids the item meta already carries.
	anchor := a.resolveMessageAnchor("interrupt-and-revert", threadID, userItem)

	cut, err := a.rollbackConversationLocked(rollbackConversationLockedArgs{
		thread:       thread,
		userItem:     userItem,
		anchor:       anchor,
		promptDraft:  &promptDraft,
		errorPrefix:  "interrupt-and-revert",
		markReverted: false,
	})
	if err != nil {
		if markedReverted {
			a.triage.ClearTurnReverted(threadID)
		}
		return InterruptAndRevertResult{}, err
	}

	cutEvent := UserMessageRevertedEvent{
		ThreadID:              threadID,
		UserItemID:            userItem.ID,
		TurnIndex:             userItem.TurnIndex,
		KeptAnchorTurnItemIDs: cut.KeptAnchorTurnItemIDs,
		HistoryRev:            cut.Stamp.Rev,
		HistoryEpoch:          cut.Stamp.Epoch,
	}
	a.emit(eventchan.UserMessageReverted, cutEvent)

	return InterruptAndRevertResult{
		Reverted:              true,
		UserItemID:            userItem.ID,
		TurnIndex:             userItem.TurnIndex,
		KeptAnchorTurnItemIDs: cutEvent.KeptAnchorTurnItemIDs,
		HistoryRev:            cutEvent.HistoryRev,
		HistoryEpoch:          cutEvent.HistoryEpoch,
	}, nil
}

// evaluateInterruptRevertPredicate runs the backend revert eligibility
// check. Returns (true, userItem, "", nil) when the most-recent turn
// holds exactly one revertable user_text, no agent-visible content has
// landed, and the flush queue is empty. Otherwise returns the reason
// the predicate declined so callers can log / emit it.
//
// Predicate (matches the frontend, intentionally; see plan):
//   - There is at least one item in SQLite.
//   - The newest turn (LastTurnIndex) contains a user_text role=user
//     row that is not wire-only.
//   - That turn contains no items of kind assistant_text or tool_call.
//   - The triage flush queue is empty for the thread (a queued
//     follow-up means Stop should let the queue drain through, not
//     discard everything).
//   - No background task is running in the tray. Reverting would close
//     the provider session, which kills background work; early Stop
//     should preserve that work and fall back to a plain interrupt.
//
// Thinking blocks, error rows, and other synthetic kinds DO NOT block
// the revert (matches Claude Code's `messagesAfterAreOnlySynthetic`).
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
	items, err := a.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return false, store.Item{}, "", fmt.Errorf("list turn items: %w", err)
	}
	var userItem store.Item
	userCount := 0
	for _, item := range items {
		if item.Kind == "user_text" && item.Role == "user" {
			if store.IsWireOnlyUserItem(item) {
				continue
			}
			userItem = item
			userCount++
		}
	}
	if userCount == 0 {
		return false, store.Item{}, "no user message in latest turn", nil
	}
	if userCount > 1 {
		// Steered turns persist multiple user_text rows for one turn.
		// Reverting one of them would break the steer ordering; let
		// the plain interrupt path handle this case.
		return false, store.Item{}, "turn has steered user messages", nil
	}
	for _, item := range items {
		if item.Kind == "assistant_text" || item.Kind == "tool_call" {
			return false, store.Item{}, "agent content present", nil
		}
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

// pendingFlushWorkCount sums every queued / in-flight follow-up message the
// revert predicate must treat as turn-extending work. It reads three counters
// that the flush handoff updates non-atomically (triage queue length, deferred
// pending count, App-layer inflight count), so it holds a.flushDispatch.handoffMu across
// all three — the same mutex RegisterQueueItem holds across its enqueue→flush
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
// Reached only from evaluateInterruptRevertPredicate (InterruptAndRevertIfClean
// holds the per-thread action lock, not a.flushDispatch.handoffMu), so there is no
// re-entrancy on this mutex.
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

// resolveMessageAnchor returns the persisted message anchor for the
// user item, or a synthesized record built from the item row when the
// at-send record didn't land (record error, legacy row) or its turn
// index drifted from the item's. The Claude rollback/fork paths key on
// `ProviderUserMessageID` when available so the slice point is immune
// to synthetic-entry ordinal drift; populating it on the synthesized
// record means an anchor-less row also benefits from the structural
// fix. op labels log lines only.
func (a *App) resolveMessageAnchor(op string, threadID string, userItem store.Item) store.MessageAnchor {
	if anchor, ok, err := a.store.GetMessageAnchor(threadID, userItem.ID); err == nil && ok {
		if anchor.TurnIndex == userItem.TurnIndex {
			return anchor
		}
		log.Printf("app: %s: anchor turn index %d does not match user item turn index %d; synthesizing", op, anchor.TurnIndex, userItem.TurnIndex)
	} else if err != nil {
		log.Printf("app: %s: load message anchor: %v", op, err)
	}
	return store.MessageAnchor{
		ThreadID:              threadID,
		UserItemID:            userItem.ID,
		TurnIndex:             userItem.TurnIndex,
		ProviderUserMessageID: usermessage.ReadProviderItemID(userItem.Meta),
		ProviderParentUUID:    usermessage.ReadProviderParentUUID(userItem.Meta),
	}
}

// runPlainInterruptLocked replicates InterruptTurn's behavior for the
// fallback branch of InterruptAndRevertIfClean. Caller holds the
// thread lock. Tolerant of "no session" so a Stop click on a stale
// thread is a no-op rather than an error.
func (a *App) runPlainInterruptLocked(threadID string) error {
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		// No session — nothing to interrupt, nothing to revert.
		return nil
	}
	providerSess := sess.providerSession()
	if providerSess == nil {
		return nil
	}
	// Pre-ack sample + pre-ack publish onto the unconsumed pending flush
	// entries, same as InterruptTurn (round-5 R5-4, round-6 R6-4,
	// round-7 R7-5).
	interruptedTurn := -1
	var stampToken triage.FlushStampToken
	if a.triage != nil {
		interruptedTurn = a.triage.OpenTurnIndex(threadID)
		stampToken = a.triage.MarkFlushSendsInterrupted(threadID, interruptedTurn)
	}
	if err := providerSess.Interrupt(context.Background()); err != nil {
		if a.triage != nil {
			a.triage.RestoreFlushSendsInterrupted(threadID, stampToken)
		}
		return err
	}
	if a.triage != nil {
		// Pre-ack sampled turn + token fence, same as InterruptTurn
		// (round-11, C11-1 / CT11-1).
		if _, err := a.triage.MarkUserInterrupt(threadID, interruptedTurn, stampToken); err != nil {
			log.Printf("app: interrupt-and-revert: plain fallback: mark user interrupt: %v", err)
		}
		a.eagerPersistFlushSendsOnInterrupt(threadID, sess, interruptedTurn, stampToken)
	}
	return nil
}
