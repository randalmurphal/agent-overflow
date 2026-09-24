package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

type InterruptRevertOptions struct {
	ExpectedSendID string         `json:"expectedSendId,omitempty"`
	Draft          *DraftSnapshot `json:"draft,omitempty"`
}

// InterruptAndRevertResult is returned by InterruptAndRevertIfClean.
// The frontend uses Reverted to decide whether to commit or roll back
// its optimistic UI (timeline row removal + composer rehydrate).
type InterruptAndRevertResult struct {
	TurnStartedSequence   uint64 `json:"turnStartedSequence"`
	TurnCompletedSequence uint64 `json:"turnCompletedSequence"`
	ItemEventSequence     uint64 `json:"itemEventSequence"`
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
	TurnStartedSequence   uint64 `json:"turnStartedSequence"`
	TurnCompletedSequence uint64 `json:"turnCompletedSequence"`
	// Replacement is published with the cut once send preparation and persistence finish.
	Replacement *store.Item `json:"replacement,omitempty"`
	// ItemEventSequence fences item frames published before the destructive cut.
	ItemEventSequence uint64 `json:"itemEventSequence"`
	ThreadID          string `json:"threadId"`
	UserItemID        string `json:"userItemId"`
	TurnIndex         int    `json:"turnIndex"`
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
	// DraftPendingResend identifies a replacement operation. It leaves the
	// ordinary composer draft alone; only the early un-send rehydrates it.
	DraftPendingResend bool `json:"draftPendingResend,omitempty"`
	// ConnectionID attributes replacement recovery to the requesting page load.
	// Every client applies the cut; only that connection records its local marker.
	ConnectionID string `json:"connectionId,omitempty"`
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
//
// Both branches interrupt the provider, and a Claude interrupt kills the
// thread's live background agents (app_background_kill.go). Unless
// confirmBackgroundKill is set, the call refuses with
// background_agents_running and the agents once it is known to interrupt,
// before anything is interrupted, reverted or written. That refusal comes
// ahead of the predicate's own "running background tasks" decline, which a
// confirmed call still takes to the plain interrupt.
//
//ao:scope threads:operate
func (a *App) InterruptAndRevertIfClean(threadID string, opts InterruptRevertOptions, confirmBackgroundKill bool) (InterruptAndRevertResult, error) {
	if a.shuttingDown.Load() {
		return InterruptAndRevertResult{}, ErrShuttingDown
	}
	if strings.TrimSpace(threadID) == "" {
		return InterruptAndRevertResult{}, errors.New("interrupt-and-revert: thread id is required")
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.threadApplication().CheckMutable(threadID); err != nil {
		return InterruptAndRevertResult{}, err
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: load thread: %w", err)
	}

	if opts.ExpectedSendID != "" {
		if _, found, err := a.findRecordedSend(threadID, opts.ExpectedSendID); err != nil {
			return InterruptAndRevertResult{}, err
		} else if !found {
			return InterruptAndRevertResult{Reason: "sent message is no longer present"}, nil
		}
	}
	eligible, userItem, reason, err := a.evaluateInterruptRevertPredicate(threadID)
	if err != nil {
		return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: predicate: %w", err)
	}
	if eligible && opts.ExpectedSendID != "" {
		meta, err := usermessage.FromItem(userItem)
		if err != nil {
			return InterruptAndRevertResult{}, err
		}
		if meta.SendID != opts.ExpectedSendID {
			return InterruptAndRevertResult{Reason: "latest message changed"}, nil
		}
	}
	if !confirmBackgroundKill {
		if sess, ok := a.sessionManager().get(threadID); ok {
			if err := a.refuseBackgroundKill(threadID, sess); err != nil {
				return InterruptAndRevertResult{}, err
			}
		}
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

	if opts.Draft != nil && opts.ExpectedSendID != "" {
		promptDraft, err = encodeThreadDraft(threadID, *opts.Draft)
		if err != nil {
			return InterruptAndRevertResult{}, err
		}
	}
	markedReverted := false
	if a.triage != nil {
		a.triage.MarkTurnReverted(threadID)
		markedReverted = true
	}

	// Codex thread/revert owns active-turn shutdown and the history cut as one
	// server-side operation. Sending turn/interrupt first splits that operation
	// across two readiness boundaries. Claude has no equivalent primitive, so
	// it still receives its interrupt before AO rewrites provider history.
	if thread.Provider != string(provider.Codex) {
		if sess, ok := a.sessionManager().get(threadID); ok {
			if providerSess := sess.ProviderSession(); providerSess != nil {
				if err := providerSess.Interrupt(context.Background()); err != nil {
					log.Printf("app: interrupt-and-revert: provider interrupt: %v", err)
				}
			}
		}
	}

	// The predicate read a turn the provider was still writing: agent output,
	// a peer message or deferred rows can land between that read and the cut.
	// Headless Claude is the one provider whose rollback AO performs after
	// stopping the session, so stop it now and decide on rows nothing can add
	// to. A decline here leaves the message in place, as a plain interrupt
	// would; the next send resumes the unmodified session.
	if thread.Provider == string(provider.Claude) {
		if err := a.stopSession(threadID); err != nil {
			if markedReverted {
				a.triage.ClearTurnReverted(threadID)
			}
			return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: stop session: %w", err)
		}
		settledUserItem, reason, err := a.unsendTurnMessage(threadID, userItem.TurnIndex)
		if err == nil && reason == "" && settledUserItem.ID != userItem.ID {
			reason = "latest message changed"
		}
		if err != nil || reason != "" {
			if markedReverted {
				a.triage.ClearTurnReverted(threadID)
			}
			if err != nil {
				return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: recheck stopped turn: %w", err)
			}
			return InterruptAndRevertResult{Reason: reason}, nil
		}
	}

	// Resolve the message anchor for the provider-rollback helpers.
	// When the at-send record didn't land, we synthesize one from the
	// item row — Claude session-fork and Codex fork-at-turn read
	// TurnIndex plus the provider ids the item meta already carries.
	anchor := a.resolveMessageAnchor("interrupt-and-revert", threadID, userItem)

	cut, err := a.rollbackConversationLocked(rollbackConversationLockedArgs{
		thread:      thread,
		userItem:    userItem,
		anchor:      anchor,
		promptDraft: &promptDraft,
		errorPrefix: "interrupt-and-revert",
	})
	if err != nil {
		if markedReverted {
			a.triage.ClearTurnReverted(threadID)
		}
		return InterruptAndRevertResult{}, err
	}

	cutEvent := UserMessageRevertedEvent{
		TurnStartedSequence:   a.eventSequence(eventchan.ProviderTurnStarted),
		TurnCompletedSequence: a.eventSequence(eventchan.ProviderTurnCompleted),
		ItemEventSequence:     a.itemEventSequence(),
		ThreadID:              threadID,
		UserItemID:            userItem.ID,
		TurnIndex:             userItem.TurnIndex,
		KeptAnchorTurnItemIDs: cut.KeptAnchorTurnItemIDs,
		HistoryRev:            cut.Stamp.Rev,
		HistoryEpoch:          cut.Stamp.Epoch,
	}
	a.emit(eventchan.UserMessageReverted, cutEvent)

	return InterruptAndRevertResult{
		TurnStartedSequence:   cutEvent.TurnStartedSequence,
		TurnCompletedSequence: cutEvent.TurnCompletedSequence,
		ItemEventSequence:     cutEvent.ItemEventSequence,
		Reverted:              true,
		UserItemID:            userItem.ID,
		TurnIndex:             userItem.TurnIndex,
		KeptAnchorTurnItemIDs: cutEvent.KeptAnchorTurnItemIDs,
		HistoryRev:            cutEvent.HistoryRev,
		HistoryEpoch:          cutEvent.HistoryEpoch,
	}, nil
}

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
	providerSess := sess.ProviderSession()
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

func (a *App) itemEventSequence() uint64 { return a.eventSequence(eventchan.ProviderItemEvent) }

func (a *App) eventSequence(channel eventchan.Channel) uint64 {
	if bus := a.eventBus.Load(); bus != nil {
		return bus.ChannelSequence(channel)
	}
	return 0
}
