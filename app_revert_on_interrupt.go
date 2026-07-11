package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/checkpoint"
	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/store"
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
	TurnIndex int `json:"turnIndex,omitempty"`
	// Reason is a short tag describing why a revert was declined.
	// Populated only when Reverted is false; useful for telemetry and
	// frontend debugging without exposing internals to the user.
	Reason string `json:"reason,omitempty"`
}

// UserMessageRevertedEvent is the wire payload for the
// `user_message:reverted` event emitted at the end of a successful
// revert-on-interrupt. The frontend consumes this to confirm the
// optimistic timeline removal it already performed. Idempotent on the
// frontend: a removal of an already-absent id is a no-op.
type UserMessageRevertedEvent struct {
	ThreadID   string `json:"threadId"`
	UserItemID string `json:"userItemId"`
	TurnIndex  int    `json:"turnIndex"`
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

	// Resolve a checkpoint for the provider-revert helpers. When the
	// at-send capture failed (e.g. workspace is not a git repo), we
	// synthesize a record carrying just the turn index — Claude
	// session-fork and Codex fork-at-turn only read TurnIndex.
	record := a.resolveRevertCheckpoint(threadID, userItem)

	err = a.revertConversationLocked(revertConversationLockedArgs{
		thread:       thread,
		userItem:     userItem,
		record:       record,
		mode:         RevertModeConversationOnly,
		promptDraft:  promptDraft,
		errorPrefix:  "interrupt-and-revert",
		markReverted: false,
	})
	if err != nil {
		if markedReverted {
			a.triage.ClearTurnReverted(threadID)
		}
		return InterruptAndRevertResult{}, err
	}

	a.emit("user_message:reverted", UserMessageRevertedEvent{
		ThreadID:   threadID,
		UserItemID: userItem.ID,
		TurnIndex:  userItem.TurnIndex,
	})

	return InterruptAndRevertResult{
		Reverted:   true,
		UserItemID: userItem.ID,
		TurnIndex:  userItem.TurnIndex,
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
			if checkpoint.IsWireOnlyUserItem(item) {
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
// pending count, App-layer inflight count), so it holds flushHandoffMu across
// all three — the same mutex RegisterQueueItem holds across its enqueue→flush
// handoff. That makes a message mid-handoff observable here as either
// still-queued or already-in-flight, never invisible in the gap between.
//
// Reached only from evaluateInterruptRevertPredicate (InterruptAndRevertIfClean
// holds the per-thread action lock, not flushHandoffMu), so there is no
// re-entrancy on this mutex.
func (a *App) pendingFlushWorkCount(threadID string) int {
	a.flushHandoffMu.Lock()
	defer a.flushHandoffMu.Unlock()
	total := a.flushDispatchItemCount(threadID)
	if a.triage != nil {
		total += a.triage.QueuedFlushItemCount(threadID)
		total += a.triage.DeferredPendingFlushItemCount(threadID)
	}
	return total
}

// resolveRevertCheckpoint returns the persisted checkpoint for the
// user item, or a synthesized record with just UserItemID, TurnIndex,
// and ProviderUserMessageID populated when the at-send capture didn't
// write a row (non-git workspace, capture error). The Claude revert
// path keys on `ProviderUserMessageID` when available so the slice
// point is immune to synthetic-entry ordinal drift; populating it on
// the synthesized record means a non-git workspace also benefits from
// the structural fix.
func (a *App) resolveRevertCheckpoint(threadID string, userItem store.Item) store.Checkpoint {
	if record, ok, err := a.store.GetCheckpointByUserItemID(threadID, userItem.ID); err == nil && ok {
		if record.TurnIndex == userItem.TurnIndex {
			return record
		}
		log.Printf("app: interrupt-and-revert: checkpoint turn index %d does not match user item turn index %d; synthesizing", record.TurnIndex, userItem.TurnIndex)
	} else if err != nil {
		log.Printf("app: interrupt-and-revert: load checkpoint: %v", err)
	}
	return store.Checkpoint{
		UserItemID:            userItem.ID,
		TurnIndex:             userItem.TurnIndex,
		ProviderUserMessageID: usermessage.ReadProviderItemID(userItem.Meta),
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
	if err := providerSess.Interrupt(context.Background()); err != nil {
		return err
	}
	if a.triage != nil {
		if _, err := a.triage.MarkUserInterrupt(threadID); err != nil {
			log.Printf("app: interrupt-and-revert: plain fallback: mark user interrupt: %v", err)
		}
		a.eagerPersistFlushSendsOnInterrupt(threadID, sess)
	}
	return nil
}
