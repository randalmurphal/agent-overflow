package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// The settling half: how a request stops being open, and how the answer
// reaches the thread that asked for it.
//
// Two paths reach the same place. The target's agent calls thread_reply,
// which is the answer the sender asked for; or the turn that consumed the
// request's message ends without one, which is an answer of a different kind
// and is labelled as such. Either way exactly one settlement is written, the
// receipt's conditional update decides which, and the collector then hands it
// to the caller: to a parked wait if one is there, otherwise as a message.

// installThreadRequestObserver watches every thread's turn ends for the
// receipts bound to them. It is registered once, at startup, because a
// request's target is any thread on this computer and a per-thread
// subscription would have to follow spawns.
func (a *App) installThreadRequestObserver() {
	a.threadRequests.observerOnce.Do(func() {
		// Every write to a request reaches the Agents rows of its caller.
		a.store.OnThreadRequestChanged(a.noteThreadRequestChanged)
		a.subscribeGlobalTurnObserver(func(threadID string, evt provider.ProviderEvent) {
			// Top-level turn ends only: a subagent finishing inside the turn
			// is not the turn ending, and settling there would answer with a
			// Task tool's last words.
			sessionEnded := evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected"
			turnEnded := evt.Kind == provider.EventTurnComplete && strings.TrimSpace(evt.ParentToolUseID) == ""
			if !sessionEnded && !turnEnded {
				return
			}
			// A read mark held for a turn that answered mid-turn has its
			// own gate, one map read, since that answer already left the
			// running set.
			if a.hasAnsweredReads(threadID) && a.lifeCtx().Err() == nil {
				a.goThreadRequestSettling(func() { a.finishAnsweredReads(threadID, sessionEnded) })
			}
			if sessionEnded {
				a.disarmRequestCancels(threadID)
			}
			// So does an echo whose confirmation failed to write.
			if a.hasUnwrittenEchoes(threadID) && a.lifeCtx().Err() == nil {
				a.goThreadRequestSettling(func() { a.retryOwnedEchoes(threadID) })
			}
			// The gate is read here, on the event worker, because that is what
			// keeps a turn end on a thread with no requests to one map read
			// and no goroutine.
			if _, ok := a.runningReceiptTokens(threadID); !ok {
				return
			}
			// Settling runs OFF the event worker. It deletes the scratch thread
			// an ask ran in and can start the caller's session to deliver a
			// wake, and both stop or start a provider session: a stop drains
			// the thread's event queue, so doing that on the worker waits for
			// itself. Shutdown joins these through threadRequestsWG.
			if a.lifeCtx().Err() != nil {
				return
			}
			a.goThreadRequestSettling(func() {
				if sessionEnded {
					// A stop sent to the session that ended has been read
					// once its end settled what it cut: a send it cut that
					// was still waiting never opens its turn there. One the
					// end failed to settle stays for the next try.
					stops := a.sessionTurnStops(threadID, evt.SessionToken)
					if a.settleReceiptsForEndedSession(threadID, evt.SessionToken) {
						a.retireTurnStops(threadID, stops)
					}
					return
				}
				a.settleReceiptsForEndedTurn(threadID)
			})
		})
	})
}

// settleReceiptsForEndedTurn settles every receipt whose turn has now
// completed. The in-memory gate keeps a turn end on a thread with no requests
// to a single map read.
func (a *App) settleReceiptsForEndedTurn(threadID string) {
	// The stops this pass may retire are those whose turn was over before
	// it read which requests are running: a turn that ends while it runs,
	// and a request that reaches such a turn meanwhile, are a later pass's.
	ended := a.endedTurnStops(threadID)
	tokens, ok := a.runningReceiptTokens(threadID)
	if !ok {
		return
	}
	// One turn can answer several requests: it consumed several messages.
	// They are all settled before anything is delivered, so their callers
	// are told once per turn rather than once per message.
	works := make([]threadSettlementWork, 0, len(tokens))
	holds := make([]receiptHold, 0, len(tokens))
	failed := false
	for _, token := range tokens {
		work, hold, err := a.settleReceiptOnTurnEnd(threadID, token)
		if err != nil {
			log.Printf("thread tools: settle request %s on %s: %v", token, threadID, err)
			failed = true
		}
		works = append(works, work)
		holds = append(holds, hold)
	}
	a.runThreadSettlementWorks(works)
	if hook := a.turnEndSettledForTest; hook != nil {
		hook(threadID)
	}
	// An ended turn's stop has been read by every request its end settled.
	// It stays for the next try of a settlement that failed, and for a
	// request still waiting on that turn or on a delivery to name its turn.
	if !failed {
		a.retireTurnStops(threadID, slices.DeleteFunc(ended, func(recorded recordedTurnStop) bool {
			return slices.ContainsFunc(holds, func(hold receiptHold) bool { return hold.holds(recorded.turn) })
		}))
	}
}

// settleReceiptsForEndedSession settles the receipts on a thread whose
// provider session ended while they were running. The process that held
// their messages is gone: nothing will deliver an undelivered one, and
// nothing will continue the turn a delivered one was read in. A stop, a
// death and a replacement each end that turn as interrupted (CleanupThread,
// handleSessionDied, EndReplacedSessionTurn), and none of those ends is a
// frame this observer sees.
//
// A message the agent never took in belongs to the restore that holds it
// (a death's, a replacement's, a teardown's), which puts it back on its
// queue or settles it when it cannot (giveBackRequest). Every restore holds
// the thread action lock from taking the message until its receipt is back
// in `accepted`, and the messages still held are read under that lock, so
// this sees either the message held or the receipt returned, never the
// moment between.
//
// session is the session that ended. The pass runs off the event worker,
// so a replacement can have started and been handed requests by the time it
// runs, a message this session never took in included: it settles only the
// receipts whose message went to this session (receiptGate). An empty
// session names none and settles every running receipt.
//
// It reports whether every receipt it read settled or was left to its
// restore; false when a read or a settlement failed.
func (a *App) settleReceiptsForEndedSession(threadID, session string) bool {
	tokens, ok := a.runningReceiptTokens(threadID)
	if !ok {
		return true
	}
	held, err := a.heldSendIDs(threadID)
	if err != nil {
		log.Printf("thread tools: read what %s still holds at its session's end: %v", threadID, err)
		return false
	}
	// Every receipt settles before anything is delivered, as at a turn's
	// end, so a caller with several requests the session's end settled is
	// told once.
	works := make([]threadSettlementWork, 0, len(tokens))
	settled := true
	for _, token := range tokens {
		work, err := a.settleReceiptOnSessionEnd(threadID, session, token, held)
		if err != nil {
			log.Printf("thread tools: settle request %s on %s at its session's end: %v", token, threadID, err)
			settled = false
		}
		works = append(works, work)
	}
	a.runThreadSettlementWorks(works)
	return settled
}

// heldSendIDs names the messages a thread's queue, dispatch or provider
// registry still holds, read under the thread action lock.
func (a *App) heldSendIDs(threadID string) (map[string]bool, error) {
	unlock, err := a.threadLocks().LockCtx(a.lifeCtx(), threadID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	held := map[string]bool{}
	if a.triage == nil {
		return held, nil
	}
	items := a.triage.UnconfirmedFlushSends(threadID)
	for _, queued := range append(a.triage.QueuedFlushItems(threadID), a.pendingFlushDispatchItems(threadID)...) {
		items = append(items, triage.UnconfirmedFlushItem{QueueItemID: queued.ID, Payload: queued.Payload})
	}
	for _, item := range items {
		for _, id := range unconfirmedMembersOf(item).ids {
			held[id] = true
		}
	}
	return held, nil
}

// settleReceiptOnSessionEnd settles one running receipt for its session's
// end. One the provider took in with no turn open yet settles as never
// answered. One it never took in settles as never read, unless its message
// is still held (held) and so its restore's to give back. A delivered one
// settles interrupted with what its turn had written by then, unless that
// turn ended on its own first, whose end then answers it as any turn end
// does.
//
// The receipt is read once, under the token's settle lock, and that read is
// the one the settlement writes from. It returns the work the settlement
// owes, for the caller to run with the work of the session's other receipts
// once the lock is released.
//
// A receipt whose message went to another session than the one that ended
// (session) is that session's, and is left to it. The dispatch and the
// delivery record their session under the same settle lock.
func (a *App) settleReceiptOnSessionEnd(threadID, session, token string, held map[string]bool) (threadSettlementWork, error) {
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	if a.receiptSession(threadID, token) != session {
		return threadSettlementWork{}, nil
	}
	receipt, found, err := a.deliveredReceiptLocked(token)
	if err != nil {
		return threadSettlementWork{}, err
	}
	if !found || receipt.TargetThreadID != threadID || receipt.State != store.ThreadReceiptRunning {
		return threadSettlementWork{}, nil
	}
	settlement := notReadSettlement(threadtools.InterruptSessionEndedUnanswered)
	if receipt.TurnID == "" {
		switch {
		case a.receiptTakenIn(threadID, token):
		case !held[threadRequestSendID(token)]:
			settlement = notReadSettlement(threadtools.InterruptSessionEndedUnread)
		default:
			return threadSettlementWork{}, nil
		}
	} else if settlement, err = a.sessionEndSettlement(threadID, receipt.TurnID); err != nil {
		return threadSettlementWork{}, err
	}
	work, _, err := a.settleReadReceiptLocked(receipt, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
	return work, err
}

// sessionEndSettlement is what a delivered receipt answers when its
// session ends: its turn's own end if that turn ended first, or a person's
// Stop cut it, and otherwise an interruption with what the turn wrote.
func (a *App) sessionEndSettlement(threadID, turnID string) (store.ThreadRequestSettlement, error) {
	index, ok := threadRequestTurnIndex(threadID, turnID)
	if !ok {
		return store.ThreadRequestSettlement{}, fmt.Errorf("receipt names turn %q, which is not a turn of this thread", turnID)
	}
	turn, found, err := a.store.GetTurnByThreadIndex(threadID, index)
	if err != nil {
		return store.ThreadRequestSettlement{}, err
	}
	if found && turn.CompletedAt != nil && (turn.StopReason != "interrupted" || a.stoppedByPerson(threadID, turn)) {
		return a.turnSettlement(threadID, turn)
	}
	if (!found || turn.CompletedAt == nil) && a.triage != nil {
		// A replaced session's turn is ended after its disconnect, and
		// what it wrote last may still be in the stream buffers.
		if err := a.triage.FlushThread(threadID); err != nil {
			return store.ThreadRequestSettlement{}, err
		}
	}
	text, err := a.lastAssistantText(threadID, index)
	if err != nil {
		return store.ThreadRequestSettlement{}, err
	}
	answer := threadtools.InterruptSessionEndedMidTurn
	if strings.TrimSpace(text) != "" {
		answer += threadtools.SessionEndedWrittenSoFar + text
	}
	return store.ThreadRequestSettlement{
		State:      store.ThreadReceiptInterrupted,
		Answer:     []byte(answer),
		AnswerKind: store.ThreadAnswerError,
	}, nil
}

// giveBackRequest puts a request whose message a session's end took back
// before the agent took it in on its queue again, owed: its receipt returns
// to `accepted`, its sender is told nothing, and the thread's next session
// start sends it. One the agent did take in is the session end's to settle.
// One its queue refuses (an archived thread, a failed write) settles as
// never read.
func (a *App) giveBackRequest(threadID, token string, message usermessage.AgentMessage) {
	a.runThreadSettlementWorkDetached(a.giveBackRequestWork(threadID, token, message))
}

// giveBackRequestWork is giveBackRequest returning the work a settlement owes
// instead of running it, for a restore that gives back several requests and
// runs their work together.
func (a *App) giveBackRequestWork(threadID, token string, message usermessage.AgentMessage) threadSettlementWork {
	tools := a.threadTools()
	if !tools.returnRequestNotTakenIn(threadID, token) {
		return threadSettlementWork{}
	}
	if err := tools.requeueRequest(token, threadID, message); err != nil {
		log.Printf("thread tools: give request %s back to %s: %v", token, threadID, err)
		return a.settleReceiptNotRead(threadID, token, store.ThreadReceiptAccepted, threadtools.InterruptSessionEndedUnread)
	}
	return threadSettlementWork{}
}

// settleReceiptNotRead settles one receipt `interrupted` with why if it is
// still in state with no turn: `running` for a message the provider was
// handed, `accepted` for one given back to a queue that refused it. The
// read and the write share the token's settle lock, which a delivery's
// binding takes too, so a delivery that lands first wins and this settles
// nothing.
//
// It returns the work the settlement owes. A restore holds the thread's
// action lock and the work takes thread locks of its own, so the caller runs
// it detached, with the work of the restore's other requests
// (runThreadSettlementWorksDetached) so a caller with several of them is
// told once.
func (a *App) settleReceiptNotRead(threadID, token, state, why string) threadSettlementWork {
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	receipt, found, err := a.deliveredReceiptLocked(token)
	if err != nil {
		log.Printf("thread tools: settle undelivered request %s on %s: %v", token, threadID, err)
		return threadSettlementWork{}
	}
	if !found || receipt.TargetThreadID != threadID || receipt.State != state || receipt.TurnID != "" {
		return threadSettlementWork{}
	}
	work, _, err := a.settleReadReceiptLocked(receipt, []string{state}, notReadSettlement(why), dropScratchThread)
	if err != nil {
		log.Printf("thread tools: settle undelivered request %s on %s: %v", token, threadID, err)
	}
	return work
}

// notReadSettlement is the answer of a receipt whose message no turn read.
func notReadSettlement(why string) store.ThreadRequestSettlement {
	return store.ThreadRequestSettlement{
		State:      store.ThreadReceiptInterrupted,
		Answer:     []byte(why),
		AnswerKind: store.ThreadAnswerError,
	}
}

// settleReceiptOnTurnEnd reads what the receipt's own turn did and settles it.
//
// The binding is the turn, not the event: an end of some other turn on the
// same thread leaves this receipt's turn row unfinished and settles nothing,
// which is what keeps a user's own turn from answering an agent's request.
// A receipt no delivery has named a turn for is bound first to the turn
// that took its message, when one did (bindUndeliveredReceiptToTakingTurn).
//
// It returns the work the settlement owes, for the caller to run with the
// work of the turn's other receipts (runThreadSettlementWorks), and what a
// running receipt it left unsettled still waits for (receiptHold).
func (a *App) settleReceiptOnTurnEnd(threadID, token string) (threadSettlementWork, receiptHold, error) {
	work, undelivered, hold, err := a.settleBoundReceiptOnTurnEnd(threadID, token)
	if err != nil || undelivered == "" {
		return work, hold, err
	}
	bound, err := a.bindUndeliveredReceiptToTakingTurn(threadID, token, undelivered)
	if err != nil || !bound {
		return threadSettlementWork{}, receiptHold{unbound: true, turn: anyOpenTurn}, err
	}
	work, _, hold, err = a.settleBoundReceiptOnTurnEnd(threadID, token)
	return work, hold, err
}

// receiptHold is what a running receipt a turn-end pass left unsettled
// still waits for: the end of its bound turn (turn, with the row's start
// when it was read), or, with no turn bound yet (unbound), a delivery that
// can name any turn, an ended one too. The hold of a settled or absent
// receipt is noHold.
type receiptHold struct {
	unbound   bool
	turn      int
	startedAt int64
}

// holds reports whether the hold keeps the stop of turn.
func (h receiptHold) holds(turn stoppedTurn) bool {
	return h.unbound || (h.turn >= 0 && turn.matches(store.Turn{TurnIndex: h.turn, StartedAt: h.startedAt}))
}

var noHold = receiptHold{turn: anyOpenTurn}

// settleBoundReceiptOnTurnEnd is settleReceiptOnTurnEnd for a receipt whose
// delivery named its turn. For a running receipt no delivery has named a
// turn for, it settles nothing and returns the row of the receipt's message
// (undelivered).
//
// The receipt is read once, under the token's settle lock, and that read is
// the one the settlement writes from.
func (a *App) settleBoundReceiptOnTurnEnd(threadID, token string) (threadSettlementWork, string, receiptHold, error) {
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	receipt, found, err := a.deliveredReceiptLocked(token)
	if err != nil {
		return threadSettlementWork{}, "", noHold, err
	}
	if !found || receipt.TargetThreadID != threadID {
		// Gone, or bound to another thread: nothing here owns it any more.
		a.forgetReceiptRunning(threadID, token)
		return threadSettlementWork{}, "", noHold, nil
	}
	if receipt.State == store.ThreadReceiptAccepted {
		// The dispatch has not bound its turn yet. The gate is set before
		// the row, so dropping it here would take away the only thing
		// watching for this receipt's turn end. Its message has not reached
		// the provider, so no ended turn can take it.
		return threadSettlementWork{}, "", noHold, nil
	}
	if receipt.State != store.ThreadReceiptRunning {
		// Replied, cancelled or settled: nothing here owns it any more.
		a.forgetReceiptRunning(threadID, token)
		return threadSettlementWork{}, "", noHold, nil
	}
	if receipt.TurnID == "" {
		// The message is with the provider and no delivery has named its
		// turn: the turn that ended may be one it was written during, not
		// one that read it. The gate stays for the turn that is named.
		return threadSettlementWork{}, receipt.MessageItemID, noHold, nil
	}
	index, ok := threadRequestTurnIndex(threadID, receipt.TurnID)
	if !ok {
		return threadSettlementWork{}, "", noHold, fmt.Errorf("receipt names turn %q, which is not a turn of this thread", receipt.TurnID)
	}
	if a.triage != nil && a.triage.OpenTurnIndex(threadID) == index {
		// The turn is running. A row that still carries an earlier
		// completion (a soft-closed turn the model continued, a re-sent
		// turn start) is not this turn's end.
		return threadSettlementWork{}, "", receiptHold{turn: index}, nil
	}
	turn, found, err := a.store.GetTurnByThreadIndex(threadID, index)
	if err != nil {
		return threadSettlementWork{}, "", noHold, err
	}
	if !found || turn.CompletedAt == nil {
		// The request's own turn is still running. Some other turn ended.
		return threadSettlementWork{}, "", receiptHold{turn: index, startedAt: turn.StartedAt}, nil
	}
	settlement, err := a.turnSettlement(threadID, turn)
	if err != nil {
		return threadSettlementWork{}, "", noHold, err
	}
	work, _, err := a.settleReadReceiptLocked(receipt, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
	return work, "", noHold, err
}

// bindUndeliveredReceiptToTakingTurn binds a running receipt no delivery has
// named a turn for to the turn that took its message (the row
// messageItemID) and ran it (triage.PendingSendTurn): a provider that opens
// the message's turn and never echoes the message still answers there. A
// delivery that arrives first wins as always (BindThreadReceiptTurn). It
// reports whether the receipt is bound now.
//
// The caller holds no settle lock: the read of the send waits on the
// thread's flush anchor, which a delivery holds when it takes that lock.
func (a *App) bindUndeliveredReceiptToTakingTurn(threadID, token, messageItemID string) (bool, error) {
	if a.triage == nil {
		return false, nil
	}
	turnIndex, state := a.triage.PendingSendTurn(threadID, messageItemID)
	if state != triage.PendingSendClaimed {
		return false, nil
	}
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	return a.store.BindThreadReceiptTurn(token, messageItemID, threadRequestTurnKey(threadID, turnIndex), true)
}

// turnSettlement maps one ended turn onto the answer its requester reads.
//
// A turn that answered without thread_reply still said something, and that
// last message is the most useful thing this computer has; it is reported as
// `final` so the caller is never told a fallback is the answer it asked for.
// A turn the user's own Stop cut (recordTurnStop) is `interrupted` with what
// it said by then as its `final` answer, which is how the wake tells it from
// a restart: every other interruption's answer is an `error` stating why. A
// turn whose session ended with it is settled by settleReceiptOnSessionEnd.
func (a *App) turnSettlement(threadID string, turn store.Turn) (store.ThreadRequestSettlement, error) {
	settledAt := time.Now().UnixMilli()
	if turn.CompletedAt != nil {
		settledAt = *turn.CompletedAt
	}
	switch {
	case turn.StopReason == "interrupted":
		stop, stopped := a.turnStopOf(threadID, turn)
		if stopped && stop.person {
			text, err := a.lastAssistantText(threadID, turn.TurnIndex)
			if err != nil {
				return store.ThreadRequestSettlement{}, err
			}
			return store.ThreadRequestSettlement{
				State:      store.ThreadReceiptInterrupted,
				Answer:     []byte(text),
				AnswerKind: store.ThreadAnswerFinal,
				SettledAt:  settledAt,
			}, nil
		}
		cause := turnStoppedUnknown
		if stopped {
			cause = stop.cause
		}
		return store.ThreadRequestSettlement{
			State:      store.ThreadReceiptInterrupted,
			Answer:     []byte(cause),
			AnswerKind: store.ThreadAnswerError,
			SettledAt:  settledAt,
		}, nil
	case turn.ErrorMessage != "" || turn.StopReason == "error":
		message := turn.ErrorMessage
		if message == "" {
			message = "The turn ended with an error."
		}
		return store.ThreadRequestSettlement{
			State:      store.ThreadReceiptErrored,
			Answer:     []byte(message),
			AnswerKind: store.ThreadAnswerError,
			SettledAt:  settledAt,
		}, nil
	}
	text, err := a.lastAssistantText(threadID, turn.TurnIndex)
	if err != nil {
		return store.ThreadRequestSettlement{}, err
	}
	if strings.TrimSpace(text) == "" {
		text = "That thread's turn ended without a message."
	}
	return store.ThreadRequestSettlement{
		State:      store.ThreadReceiptFinished,
		Answer:     []byte(text),
		AnswerKind: store.ThreadAnswerFinal,
		SettledAt:  settledAt,
	}, nil
}

// lastAssistantText is the final top-level assistant message of one turn: what
// the thread said last before it stopped. Subagent rows are skipped because
// they are a tool's output, not the thread's answer.
//
// The whole body is read: the answer stored on the request is the complete
// settled text, and the preview a wake carries is applied where the wake is
// rendered, not here.
func (a *App) lastAssistantText(threadID string, turnIndex int) (string, error) {
	row, found, err := a.store.LastTopLevelTurnItem(threadID, turnIndex, "assistant_text")
	if err != nil || !found {
		return "", err
	}
	text, _, err := a.threadTools().itemBody(row, threadItemWholeBody)
	return text, err
}

// threadSettlementWork is what one settlement owes once the token's settle
// lock is released. None of it may run under that lock: the wake takes the
// caller thread's own locks and can start its session, the scratch deletion
// takes the answering thread's action lock, and the admission refresh talks
// to a live provider.
type threadSettlementWork struct {
	token string
	// targetThread is the thread that answered: the scratch fork to delete
	// when this settlement owns it, and the session whose thread tools go
	// back to this computer's own switch now that the request is over.
	targetThread string
	dropScratch  bool
	// wake is whether the answer is owed to the caller as a message, and
	// late marks it as the second wake of a late reply.
	wake bool
	late bool
	// answered is the settled receipt when its answer reached the thread
	// on this computer that spawned the thread that answered, which may
	// then be marked read (markAnsweredRead). Nil for every other
	// settlement.
	answered *store.ThreadRequestReceipt
}

// dropScratchThread and keepScratchThread name what a settlement does with
// the thread that answered it.
const (
	dropScratchThread = true
	keepScratchThread = false
)

// runThreadSettlementWork performs what one settlement deferred.
func (a *App) runThreadSettlementWork(work threadSettlementWork) {
	a.runThreadSettlementWorks([]threadSettlementWork{work})
}

// runThreadSettlementWorks performs what the settlements of one event
// deferred: a turn end, a restart, a thread going away. It runs with every
// token's settle lock released; a wake takes its own again only to claim its
// delivery.
//
// The wakes are delivered together, after every admission refresh and before
// any scratch thread goes, so the ones that say the same thing to the same
// caller arrive as one message (deliverThreadWakes).
func (a *App) runThreadSettlementWorks(works []threadSettlementWork) {
	wakes := make([]threadWakeRef, 0, len(works))
	for _, work := range works {
		if work.token == "" {
			continue
		}
		// The request is over, so a thread that was serving the tools only to
		// answer it returns to this computer's switch.
		a.refreshThreadToolsAdmission(work.targetThread)
		if work.wake {
			wakes = append(wakes, threadWakeRef{token: work.token, late: work.late})
		}
	}
	if len(wakes) > 0 {
		a.deliverThreadWakes(wakes)
	}
	for _, work := range works {
		if work.token == "" {
			continue
		}
		if work.answered != nil {
			a.markAnsweredRead(*work.answered)
		}
		if work.dropScratch {
			// The scratch thread has done its work. The answer lives in the
			// receipt, which the drop detaches from the thread first so a
			// paired computer that has not collected it yet still can; a
			// responder settling its own ask deletes the thread after its
			// reply is written.
			a.dropScratchThreadForRequest(work.token, work.targetThread)
		}
	}
}

// runThreadSettlementWorkDetached is runThreadSettlementWorksDetached for
// one settlement.
func (a *App) runThreadSettlementWorkDetached(work threadSettlementWork) {
	a.runThreadSettlementWorksDetached([]threadSettlementWork{work})
}

// runThreadSettlementWorksDetached runs the deferred work on its own
// goroutine, for settlements written inside an operation that holds thread
// locks of its own: the delete port, a delivery binding a receipt to its
// turn, and a restore settling a request whose message went back
// undelivered. None of that work may wait on a lock its own caller is
// holding. Shutdown joins these through threadRequestsWG, and a wake dropped
// by a shutdown is redelivered by the next boot's undelivered-wake pass.
func (a *App) runThreadSettlementWorksDetached(works []threadSettlementWork) {
	works = slices.DeleteFunc(works, func(work threadSettlementWork) bool { return work.token == "" })
	if len(works) == 0 || a.lifeCtx().Err() != nil {
		return
	}
	a.goThreadRequestSettling(func() { a.runThreadSettlementWorks(works) })
}

// goThreadRequestSettling runs a settlement or a wake delivery on its own
// goroutine, joined at shutdown through threadRequestsWG and counted in
// threadRequestSettling, and reports whether it started it. Once shutdown
// has begun it runs nothing: work started then could outlive the join and
// the store. What it would have done is left to the next boot's sweeps.
func (a *App) goThreadRequestSettling(fn func()) bool {
	if a.lifeCtx().Err() != nil {
		return false
	}
	return a.threadRequestsWG.GoCounted(&a.threadRequestSettling, fn)
}

// settleThreadReceipt writes one settlement on the destination row and hands
// it to the source. It is the single door: the turn observer, thread_cancel,
// the lifecycle hooks and the boot sweep all arrive here, so the scratch
// cleanup and the collection happen exactly once per settled request.
//
// It takes the token's settle lock itself, for the durable settlement and the
// delivery decision only, and releases it before the queue, provider and
// deletion work. The caller must not hold that lock.
func (a *App) settleThreadReceipt(token string, from []string, settlement store.ThreadRequestSettlement) error {
	work, _, err := a.settleReceiptUnderLock(token, from, settlement, dropScratchThread)
	if err != nil {
		return err
	}
	a.runThreadSettlementWork(work)
	return nil
}

// settleThreadReceiptNoScratch is the door for the delete and move ports: the
// thread that answered is being destroyed or handed away by the caller, which
// owns that destruction itself, so this settlement must not also delete it.
// It returns the work the settlement owes and whether it applied.
//
// Those callers hold the answering thread's action lock, so they run the
// deferred work detached (runThreadSettlementWorksDetached): the wake takes
// the asking thread's own locks, and a subtree delete can be holding those
// too.
func (a *App) settleThreadReceiptNoScratch(token string, from []string, settlement store.ThreadRequestSettlement) (threadSettlementWork, bool, error) {
	return a.settleReceiptUnderLock(token, from, settlement, keepScratchThread)
}

// settleReceiptUnderLock is the locking half of both doors.
func (a *App) settleReceiptUnderLock(
	token string, from []string, settlement store.ThreadRequestSettlement, scratch bool,
) (threadSettlementWork, bool, error) {
	unlock := a.threadRequestSettleLock(token)
	defer unlock()
	return a.settleThreadReceiptLocked(token, from, settlement, scratch)
}

// settleThreadReceiptLocked is the settlement itself, for a caller that
// already holds the token's settle lock because a read it made under that
// lock must not be separated from this write: thread_reply's idempotency
// check and a delivery binding a receipt to its turn. It returns the work
// the caller must run after releasing the lock.
//
// A request this computer made of its own thread is collected in the same
// transaction (store.SettleLocalThreadRequest); one from another computer is
// collected by that computer's poller. settlement.DisarmWake is the request's
// half and is written only to a request here.
func (a *App) settleThreadReceiptLocked(
	token string, from []string, settlement store.ThreadRequestSettlement, scratch bool,
) (threadSettlementWork, bool, error) {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return threadSettlementWork{}, false, err
	}
	if !found {
		return threadSettlementWork{}, false, nil
	}
	return a.settleReadReceiptLocked(receipt, from, settlement, scratch)
}

// settleReadReceiptLocked is settleThreadReceiptLocked for a receipt the
// caller read under the token's settle lock. The receipt, the source row on
// this computer and the acknowledgement are one store transaction
// (SettleLocalThreadRequest); a request another computer made is collected
// by its poller.
func (a *App) settleReadReceiptLocked(
	receipt store.ThreadRequestReceipt, from []string, settlement store.ThreadRequestSettlement, scratch bool,
) (threadSettlementWork, bool, error) {
	token := receipt.Token
	// The answer records whether its thread is a standing agent now, and
	// the source row copies it, on this computer or another.
	settlement.Agent = a.threadTools().answerAgent(token, receipt.TargetThreadID)
	if settlement.AnswerKind == store.ThreadAnswerReply {
		// An explicit reply is owed to a caller on this computer as a
		// message whether or not it asked to be told (replyArmsWake); the
		// settlement arms it in the statement that stores the reply.
		row, found, err := a.store.GetThreadRequest(token)
		if err != nil {
			return threadSettlementWork{}, false, err
		}
		// A cancel's settlement disarms instead (DisarmWake), in the same
		// write; the store refuses both.
		settlement.ArmWake = found && row.TargetComputerID == "" && !settlement.DisarmWake && a.replyArmsWake(row)
	}
	result, err := a.store.SettleLocalThreadRequest(token, from, settlement, store.ThreadRequestStateForReceipt(settlement.State))
	if err != nil {
		return threadSettlementWork{}, false, err
	}
	if !result.Settled {
		// Another path settled it first. Its collection ran with its answer.
		return threadSettlementWork{}, false, nil
	}
	a.forgetReceiptRunning(receipt.TargetThreadID, token)
	if result.Outbound {
		// The row names a request THIS computer sent elsewhere. A receipt
		// carrying the same token was minted by a paired computer, and
		// settling the outbound row from it would write another computer's
		// answer into this one's request.
		log.Printf("thread tools: receipt %s collides with an outbound request; nothing collected", token)
	}
	work := threadSettlementWork{
		token:        token,
		targetThread: receipt.TargetThreadID,
		dropScratch:  scratch,
	}
	if result.Collected {
		work.wake = a.collectionWakeOwed(token, result.Notify)
		if result.AnsweredBySpawned {
			answered := result.Receipt
			work.answered = &answered
		}
	}
	return work, true, nil
}

// collectionWake decides the wake one collection owes the caller. A late
// reply collected with it is the one message: it replaces a first answer not
// yet delivered (store.ThreadRequest.WakeOwed), and a first answer already
// delivered is owed nothing more.
func (a *App) collectionWake(token string, collected store.ThreadRequestCollected) (wake, late bool) {
	switch {
	case collected.LateReply:
		return a.finishThreadRequestCollection(token, true), true
	case collected.Settled:
		return a.finishThreadRequestCollection(token, false), false
	}
	return false, false
}

// storeThreadReceiptLateReply stores a late reply on the receipt and, for a
// request this computer made of its own thread, collects the receipt into the
// request in the same transaction. agent is whether the thread that answered
// is a standing agent now, which both rows record. It reports whether the
// receipt took it and what the request collected.
func (a *App) storeThreadReceiptLateReply(token string, reply []byte, at int64, agent *store.ThreadAnswerAgent) (bool, store.ThreadRequestCollected, error) {
	source, local, err := a.localThreadRequest(token)
	if err != nil {
		return false, store.ThreadRequestCollected{}, err
	}
	if !local {
		stored, err := a.store.StoreThreadReceiptLateReply(token, reply, at, agent)
		return stored, store.ThreadRequestCollected{}, err
	}
	return a.store.StoreLocalThreadRequestLateReply(token, reply, at, agent, a.replyArmsWake(source))
}

// localThreadRequest reads the request a receipt answers, when this computer
// made it of its own thread. A token no request here owns belongs to another
// computer, whose poller collects it; that is the whole difference between
// the local path and the remote one.
func (a *App) localThreadRequest(token string) (store.ThreadRequest, bool, error) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found {
		return store.ThreadRequest{}, false, err
	}
	if row.TargetComputerID != "" {
		// The row names a request THIS computer sent elsewhere. A receipt
		// carrying the same token was minted by a paired computer, and
		// settling the outbound row from it would write another computer's
		// answer into this one's request.
		log.Printf("thread tools: receipt %s collides with an outbound request; nothing collected", token)
		return store.ThreadRequest{}, false, nil
	}
	return row, true, nil
}

// Reply settles one request the caller's thread was asked.
//
// The settlement runs under the token's settle lock and everything it owes
// after it, never inside it: an in-process caller runs the after-response
// work on this same goroutine, and the lock is not reentrant.
func (t threadToolsApp) Reply(ctx context.Context, caller threadtools.Caller, call threadtools.ReplyCall) (threadtools.ReplyAck, error) {
	if ack, handled, err := t.app.answerWorkerPrompt(ctx, caller, call); handled {
		if err != nil {
			return threadtools.ReplyAck{}, err
		}
		return ack, nil
	}
	if call.Decision != "" || len(call.Answers) > 0 {
		return threadtools.ReplyAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"decision and answers answer a worker's question or approval. That token is a request's: put your answer in text.", nil)
	}
	ack, work, err := t.replyLocked(caller, call)
	if err != nil {
		return threadtools.ReplyAck{}, err
	}
	// Both halves wait for the response, because both act on the thread this
	// call is running in: the request is over, so its live session goes back
	// to this computer's switch, and an ask's scratch thread is deleted. A
	// call whose own session lost its tools, or whose own thread vanished,
	// would fail after having done exactly what it was asked.
	t.afterResponse(ctx, func() {
		t.app.runThreadSettlementWork(work)
		t.app.dropScratchThreadForRequest(call.Token, caller.ThreadID)
	})
	return ack, nil
}

func (t threadToolsApp) replyLocked(caller threadtools.Caller, call threadtools.ReplyCall) (threadtools.ReplyAck, threadSettlementWork, error) {
	unlock := t.app.threadRequestSettleLock(call.Token)
	defer unlock()

	var work threadSettlementWork
	receipt, found, err := t.app.store.GetThreadRequestReceipt(call.Token)
	if err != nil {
		return threadtools.ReplyAck{}, work, err
	}
	if !found {
		return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeRequestUnknown,
			"No request carries that token on this computer. Take the token from the \"Agent request\" footer of the message you are answering.", nil)
	}
	if receipt.TargetThreadID != caller.ThreadID {
		return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a request addressed to another thread. Reply only to the requests this thread was asked.", nil)
	}
	ack := threadtools.ReplyAck{
		Token:          call.Token,
		SourceThreadID: receipt.SourceThreadID,
		SourceComputer: receipt.SourceComputerName,
	}
	text := []byte(call.Text)
	switch receipt.State {
	case store.ThreadReceiptRunning:
		// The scratch thread stays: this reply is being written in it, and
		// the caller drops it once the response has been handed back.
		settled := false
		work, settled, err = t.app.settleThreadReceiptLocked(call.Token, []string{store.ThreadReceiptRunning}, store.ThreadRequestSettlement{
			State:      store.ThreadReceiptReplied,
			Answer:     text,
			AnswerKind: store.ThreadAnswerReply,
		}, keepScratchThread)
		if err != nil {
			return threadtools.ReplyAck{}, threadSettlementWork{}, err
		}
		if !settled {
			return threadtools.ReplyAck{}, threadSettlementWork{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request settled while this reply was being written.", nil)
		}
		ack.Accepted, ack.State = true, threadtools.RequestReplied

	case store.ThreadReceiptReplied:
		// A retry of the same reply is the same reply, not a second one.
		if !bytes.Equal(receipt.Answer, text) {
			return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request was already answered with different text. Reply once per token; send a correction with thread_send.", nil)
		}
		ack.State = threadtools.RequestReplied

	case store.ThreadReceiptFinished:
		// The turn ended before the reply. The sender has already been told
		// so, and this arrives as a follow-up revision rather than a refusal.
		ack.Late = true
		if len(receipt.LateReply) > 0 {
			if !bytes.Equal(receipt.LateReply, text) {
				return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeInvalidRequest,
					"That request already carries a late reply with different text. Reply once per token.", nil)
			}
			ack.State = threadtools.RequestFinished
			break
		}
		// The sender polls for a late reply only while the hold on the
		// first answer runs, so past it the reply would never be collected.
		now := time.Now().UnixMilli()
		if receipt.ExpiresAt != 0 && now >= receipt.ExpiresAt {
			return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request finished more than a day ago and its sender has stopped listening for a reply. Use thread_send to reach it.", nil)
		}
		stored, collected, err := t.app.storeThreadReceiptLateReply(call.Token, text, now, t.answerAgent(call.Token, receipt.TargetThreadID))
		if err != nil {
			return threadtools.ReplyAck{}, work, err
		}
		if !stored {
			return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request stopped accepting replies while this one was being written.", nil)
		}
		work = threadSettlementWork{token: call.Token, targetThread: receipt.TargetThreadID}
		work.wake, work.late = t.app.collectionWake(call.Token, collected)
		ack.Accepted, ack.State = true, threadtools.RequestFinished

	default:
		return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("That request is %s and no longer takes a reply.", receipt.State), nil)
	}

	if updated, found, err := t.app.store.GetThreadRequestReceipt(call.Token); err == nil && found {
		ack.Revision = updated.Revision
	}
	return ack, work, nil
}

// dropScratchThreadForRequest deletes the hidden thread an ask was answered
// in, once the answer is stored somewhere that outlives it.
//
// A thread that is not a scratch fork of this request is left alone: a send
// and a spawn address threads the person owns, and settling a request is
// never a reason to delete one of those.
func (a *App) dropScratchThreadForRequest(token, threadID string) {
	if threadID == "" {
		return
	}
	row, found, err := a.store.GetScratchThread(threadID)
	if err != nil {
		log.Printf("thread tools: read scratch thread %s: %v", threadID, err)
		return
	}
	if !found || row.RequestToken != token {
		return
	}
	// The receipt is what a paired computer collects, and its thread
	// binding would cascade it away with the thread.
	if _, err := a.store.DetachThreadReceiptsFromThread(threadID); err != nil {
		log.Printf("thread tools: detach receipts from scratch thread %s: %v", threadID, err)
		return
	}
	if err := a.deleteThread(threadID); err != nil {
		log.Printf("thread tools: delete scratch thread %s for request %s: %v", threadID, token, err)
		return
	}
	// The row cascades with the thread; deleting it again is a no-op that
	// covers a thread already gone for another reason.
	if _, err := a.store.DeleteScratchThread(threadID); err != nil {
		log.Printf("thread tools: delete scratch row %s: %v", threadID, err)
	}
}

// replyArmsWake reports whether an explicit reply to one of row's caller's
// requests is owed to that caller as a message.
//
// It is, whether or not the caller asked to be notified: `notify` decides
// whether a caller hears that a turn ended without an answer, and a
// thread_reply is the answer. The one caller it is not owed to is a thread
// that cannot take it: one that is gone, that this computer may no longer
// run, or whose delete committed (store.CheckThreadDeliverable). A caller a
// delete only claimed is owed it: the fence refuses the delivery, and the
// wake's retry brings it once the delete is refused. An archived caller is refused by the statement that stores the reply
// (store.ThreadRequestSettlement.ArmWake), where an archive cannot slip in
// between the check and the write. The reply stays on the request either
// way, where `thread_status` returns it.
func (a *App) replyArmsWake(row store.ThreadRequest) bool {
	if row.Notify {
		return true
	}
	thread, err := a.store.GetThread(row.CallerThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		// Unknown is not "cannot take it". The delivery reads the thread
		// again and records why it failed if it does.
		log.Printf("thread tools: read caller %s of request %s: %v", row.CallerThreadID, row.Token, err)
		return true
	}
	return a.store.CheckThreadDeliverable(thread) == nil
}

// finishThreadRequestCollection decides how a stored settlement reaches the
// caller. It is the delivery half of collection, shared by the local doors
// above and the poller that collects from another computer, so a remote
// answer arrives exactly the way a local one does.
//
// A parked wait IS the delivery: it returns the answer in the reply to the
// call that is waiting, so no message is owed. The caller holds the token's
// settle lock, which is what makes that decision exclusive with a timing-out
// wait; the row is re-read under it because the settlement the caller just
// wrote, and any wake a wait armed while it was parked, are both part of what
// the decision reads.
func (a *App) finishThreadRequestCollection(token string, late bool) bool {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		a.wakeRequestWaits(token)
		log.Printf("thread tools: read request %s for delivery: %v", token, err)
		return false
	}
	return a.collectionWakeOwed(token, found && row.Notify)
}

// collectionWakeOwed wakes the calls parked on token and reports whether
// its answer is owed as a message: its wake is armed (notify, read under the
// token's settle lock after the settlement was written) and no wait is
// parked on it to return it.
func (a *App) collectionWakeOwed(token string, notify bool) bool {
	a.wakeRequestWaits(token)
	return notify && !a.requestWaitActive(token)
}
