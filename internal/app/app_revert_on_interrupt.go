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
// thread's live background agents (app_background_kill.go).
// confirmedAgents names, by transcriptRootId, the agents the person
// confirmed the Stop may kill. While a live agent is not among them the
// call refuses with background_agents_running and every live agent once it
// is known to interrupt, before anything is interrupted, reverted or
// written. That refusal comes ahead of the predicate's own "running
// background tasks" decline, which a confirmed call still takes to the
// plain interrupt.
//
//ao:scope threads:operate
func (a *App) InterruptAndRevertIfClean(threadID string, opts InterruptRevertOptions, confirmedAgents []string) (InterruptAndRevertResult, error) {
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
	if sess, ok := a.sessionManager().get(threadID); ok {
		if err := a.refuseBackgroundKill(threadID, sess, personalStop(confirmedAgents)); err != nil {
			return InterruptAndRevertResult{}, err
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
					if thread.Provider != string(provider.Claude) {
						// claude-tui: the Esc is the native revert
						// (rollbackConversationLocked). Without it the TUI
						// keeps the turn, so AO must not cut its own copy.
						if markedReverted {
							a.triage.ClearTurnReverted(threadID)
						}
						return InterruptAndRevertResult{}, fmt.Errorf("interrupt-and-revert: provider interrupt: %w", err)
					}
					// Headless Claude: the session stop below ends the turn
					// whatever the interrupt did, and the session-end settle
					// writes what its kill frames would have.
					log.Printf("app: interrupt-and-revert: provider interrupt, superseded by the session stop: %v", err)
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
