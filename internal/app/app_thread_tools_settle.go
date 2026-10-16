package app

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
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
					a.settleReceiptsForEndedSession(threadID)
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
	tokens, ok := a.runningReceiptTokens(threadID)
	if !ok {
		return
	}
	// A person's Stop mark on an ended turn goes once every receipt of that
	// turn has settled; one that failed keeps it for the next try.
	allSettled := map[int]bool{}
	for _, token := range tokens {
		turnIndex, err := a.settleReceiptOnTurnEnd(threadID, token)
		if err != nil {
			log.Printf("thread tools: settle request %s on %s: %v", token, threadID, err)
		}
		if turnIndex < 0 {
			continue
		}
		if _, seen := allSettled[turnIndex]; !seen {
			allSettled[turnIndex] = true
		}
		allSettled[turnIndex] = allSettled[turnIndex] && err == nil
	}
	for turnIndex, settled := range allSettled {
		if settled {
			a.forgetPersonStop(threadID, turnIndex)
		}
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
func (a *App) settleReceiptsForEndedSession(threadID string) {
	tokens, ok := a.runningReceiptTokens(threadID)
	if !ok {
		return
	}
	held, err := a.heldSendIDs(threadID)
	if err != nil {
		log.Printf("thread tools: read what %s still holds at its session's end: %v", threadID, err)
		return
	}
	for _, token := range tokens {
		if err := a.settleReceiptOnSessionEnd(threadID, token, held); err != nil {
			log.Printf("thread tools: settle request %s on %s at its session's end: %v", token, threadID, err)
		}
	}
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
		for _, id := range unconfirmedSendIDs(item) {
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
func (a *App) settleReceiptOnSessionEnd(threadID, token string, held map[string]bool) error {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return err
	}
	if !found || receipt.TargetThreadID != threadID || receipt.State != store.ThreadReceiptRunning {
		return nil
	}
	if receipt.TurnID == "" {
		switch {
		case a.receiptTakenIn(threadID, token):
			a.settleUndeliveredReceipt(threadID, token, threadtools.InterruptSessionEndedUnanswered)
		case !held[threadRequestSendID(token)]:
			a.settleUndeliveredReceipt(threadID, token, threadtools.InterruptSessionEndedUnread)
		}
		return nil
	}
	index, ok := threadRequestTurnIndex(threadID, receipt.TurnID)
	if !ok {
		return fmt.Errorf("receipt names turn %q, which is not a turn of this thread", receipt.TurnID)
	}
	turn, found, err := a.store.GetTurnByThreadIndex(threadID, index)
	if err != nil {
		return err
	}
	// A turn that ended on its own, or that a person's Stop cut, answers as
	// its end does.
	if found && turn.CompletedAt != nil && (turn.StopReason != "interrupted" || a.personStopped(threadID, turn)) {
		settlement, err := a.turnSettlement(threadID, turn)
		if err != nil {
			return err
		}
		return a.settleThreadReceipt(token, []string{store.ThreadReceiptRunning}, settlement)
	}
	if (!found || turn.CompletedAt == nil) && a.triage != nil {
		// A replaced session's turn is ended after its disconnect, and
		// what it wrote last may still be in the stream buffers.
		if err := a.triage.FlushThread(threadID); err != nil {
			return err
		}
	}
	text, err := a.lastAssistantText(threadID, index)
	if err != nil {
		return err
	}
	answer := threadtools.InterruptSessionEndedMidTurn
	if strings.TrimSpace(text) != "" {
		answer += threadtools.SessionEndedWrittenSoFar + text
	}
	return a.settleThreadReceipt(token, []string{store.ThreadReceiptRunning}, store.ThreadRequestSettlement{
		State:      store.ThreadReceiptInterrupted,
		Answer:     []byte(answer),
		AnswerKind: store.ThreadAnswerError,
	})
}

// giveBackRequest puts a request whose message a session's end took back
// before the agent took it in on its queue again, owed: its receipt returns
// to `accepted`, its sender is told nothing, and the thread's next session
// start sends it. One the agent did take in is the session end's to settle.
// One its queue refuses (an archived thread, a failed write) settles as
// never read.
func (a *App) giveBackRequest(threadID, token, body string) {
	tools := a.threadTools()
	if !tools.returnRequestNotTakenIn(threadID, token) {
		return
	}
	if err := tools.requeueRequest(token, threadID, body); err != nil {
		log.Printf("thread tools: give request %s back to %s: %v", token, threadID, err)
		a.settleReceiptNotRead(threadID, token, store.ThreadReceiptAccepted, threadtools.InterruptSessionEndedUnread)
	}
}

// settleUndeliveredReceipt settles one receipt `interrupted` if it is still
// running with no turn. The read and the write share the token's settle
// lock, which a delivery's binding takes too, so a delivery that lands first
// wins and this settles nothing.
func (a *App) settleUndeliveredReceipt(threadID, token, why string) {
	a.settleReceiptNotRead(threadID, token, store.ThreadReceiptRunning, why)
}

// settleReceiptNotRead settles one receipt `interrupted` with why if it is
// still in state with no turn: `running` for a message the provider was
// handed, `accepted` for one given back to a queue that refused it.
func (a *App) settleReceiptNotRead(threadID, token, state, why string) {
	work, err := func() (threadSettlementWork, error) {
		unlock := a.threadRequestSettleLock(token)
		defer unlock()
		receipt, found, err := a.store.GetThreadRequestReceipt(token)
		if err != nil {
			return threadSettlementWork{}, err
		}
		if !found || receipt.TargetThreadID != threadID || receipt.State != state || receipt.TurnID != "" {
			return threadSettlementWork{}, nil
		}
		work, _, err := a.settleThreadReceiptLocked(token, []string{state}, store.ThreadRequestSettlement{
			State:      store.ThreadReceiptInterrupted,
			Answer:     []byte(why),
			AnswerKind: store.ThreadAnswerError,
		}, dropScratchThread)
		return work, err
	}()
	if err != nil {
		log.Printf("thread tools: settle undelivered request %s on %s: %v", token, threadID, err)
		return
	}
	// Detached: a restore holds the thread's action lock, and the work
	// takes thread locks of its own.
	a.runThreadSettlementWorkDetached(work)
}

// settleReceiptOnTurnEnd reads what the receipt's own turn did and settles it.
// It returns the index of the ended turn it read, or -1 when the receipt's
// turn has not ended.
//
// The binding is the turn, not the event: an end of some other turn on the
// same thread leaves this receipt's turn row unfinished and settles nothing,
// which is what keeps a user's own turn from answering an agent's request.
//
// The receipt is read once, under the token's settle lock, and that read is
// the one the settlement writes from.
func (a *App) settleReceiptOnTurnEnd(threadID, token string) (int, error) {
	work, index, err := func() (threadSettlementWork, int, error) {
		unlock := a.threadRequestSettleLock(token)
		defer unlock()
		receipt, found, err := a.store.GetThreadRequestReceipt(token)
		if err != nil {
			return threadSettlementWork{}, -1, err
		}
		if !found || receipt.TargetThreadID != threadID {
			// Gone, or bound to another thread: nothing here owns it any more.
			a.forgetReceiptRunning(threadID, token)
			return threadSettlementWork{}, -1, nil
		}
		if receipt.State == store.ThreadReceiptAccepted {
			// The dispatch has not bound its turn yet. The gate is set before
			// the row, so dropping it here would take away the only thing
			// watching for this receipt's turn end.
			return threadSettlementWork{}, -1, nil
		}
		if receipt.State != store.ThreadReceiptRunning {
			// Replied, cancelled or settled: nothing here owns it any more.
			a.forgetReceiptRunning(threadID, token)
			return threadSettlementWork{}, -1, nil
		}
		if receipt.TurnID == "" {
			// The message is with the provider but not delivered yet: the turn
			// that ended is one it was written during, not one that read it.
			// The gate stays for the turn the delivery names.
			return threadSettlementWork{}, -1, nil
		}
		index, ok := threadRequestTurnIndex(threadID, receipt.TurnID)
		if !ok {
			return threadSettlementWork{}, -1, fmt.Errorf("receipt names turn %q, which is not a turn of this thread", receipt.TurnID)
		}
		turn, found, err := a.store.GetTurnByThreadIndex(threadID, index)
		if err != nil {
			return threadSettlementWork{}, -1, err
		}
		if !found || turn.CompletedAt == nil {
			// The request's own turn is still running. Some other turn ended.
			return threadSettlementWork{}, -1, nil
		}
		settlement, err := a.turnSettlement(threadID, turn)
		if err != nil {
			return threadSettlementWork{}, index, err
		}
		work, _, err := a.settleReadReceiptLocked(receipt, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
		return work, index, err
	}()
	a.runThreadSettlementWork(work)
	return index, err
}

// turnSettlement maps one ended turn onto the answer its requester reads.
//
// A turn that answered without thread_reply still said something, and that
// last message is the most useful thing this computer has; it is reported as
// `final` so the caller is never told a fallback is the answer it asked for.
// A turn a person's Stop cut (notePersonStop) is `interrupted` with what it
// said by then as its `final` answer, which is how the wake tells it from a
// restart: every other interruption's answer is an `error` stating why. A
// turn whose session ended with it is settled by settleReceiptOnSessionEnd.
func (a *App) turnSettlement(threadID string, turn store.Turn) (store.ThreadRequestSettlement, error) {
	settledAt := time.Now().UnixMilli()
	if turn.CompletedAt != nil {
		settledAt = *turn.CompletedAt
	}
	switch {
	case turn.StopReason == "interrupted" && a.personStopped(threadID, turn):
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
	case turn.StopReason == "interrupted":
		return store.ThreadRequestSettlement{
			State:      store.ThreadReceiptInterrupted,
			Answer:     []byte(threadtools.InterruptTurn),
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
}

// dropScratchThread and keepScratchThread name what a settlement does with
// the thread that answered it.
const (
	dropScratchThread = true
	keepScratchThread = false
)

// runThreadSettlementWork performs what the settlement deferred. It runs with
// the token's settle lock released; the wake takes it again only to claim its
// delivery.
func (a *App) runThreadSettlementWork(work threadSettlementWork) {
	if work.token == "" {
		return
	}
	// The request is over, so a thread that was serving the tools only to
	// answer it returns to this computer's switch.
	a.refreshThreadToolsAdmission(work.targetThread)
	if work.wake {
		a.deliverThreadWake(work.token, work.late)
	}
	if work.dropScratch {
		// The scratch thread has done its work. The answer lives in the
		// receipt, which the drop detaches from the thread first so a paired
		// computer that has not collected it yet still can; a responder
		// settling its own ask deletes the thread after its reply is written.
		a.dropScratchThreadForRequest(work.token, work.targetThread)
	}
}

// runThreadSettlementWorkDetached runs the deferred work on its own goroutine,
// for a settlement written inside an operation that holds thread locks of its
// own: the delete port, a delivery binding a receipt to its turn, and a
// restore settling a request whose message went back undelivered. None
// of that work may wait on a lock its own caller is holding. Shutdown joins
// these through threadRequestsWG, and a wake dropped by a shutdown is redelivered
// by the next boot's undelivered-wake pass.
func (a *App) runThreadSettlementWorkDetached(work threadSettlementWork) {
	if work.token == "" || a.lifeCtx().Err() != nil {
		return
	}
	a.goThreadRequestSettling(func() { a.runThreadSettlementWork(work) })
}

// goThreadRequestSettling runs a settlement or a wake delivery on its own
// goroutine, joined at shutdown through threadRequestsWG and counted in
// threadRequestSettling.
func (a *App) goThreadRequestSettling(fn func()) {
	a.threadRequestsWG.Add(1)
	a.threadRequestSettling.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		defer a.threadRequestSettling.Done()
		fn()
	}()
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
// It reports whether the settlement applied.
//
// Those callers hold the answering thread's action lock, so the deferred work
// runs detached: the wake takes the asking thread's own locks, and a subtree
// delete can be holding those too.
func (a *App) settleThreadReceiptNoScratch(token string, from []string, settlement store.ThreadRequestSettlement) (bool, error) {
	work, settled, err := a.settleReceiptUnderLock(token, from, settlement, keepScratchThread)
	if err != nil {
		return false, err
	}
	a.runThreadSettlementWorkDetached(work)
	return settled, nil
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
// check, a delivery binding a receipt to its turn, and the settlement of an
// undelivered one. It returns the work
// the caller must run after releasing the lock.
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
	result, err := a.store.SettleLocalThreadRequest(token, from, settlement, threadRequestStateForReceipt(settlement.State))
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
	}
	return work, true, nil
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
		stored, err := t.app.store.StoreThreadReceiptLateReply(call.Token, text, now, t.answerAgent(call.Token, receipt.TargetThreadID))
		if err != nil {
			return threadtools.ReplyAck{}, work, err
		}
		if !stored {
			return threadtools.ReplyAck{}, work, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request stopped accepting replies while this one was being written.", nil)
		}
		work = threadSettlementWork{
			token:        call.Token,
			targetThread: receipt.TargetThreadID,
			late:         true,
			wake:         t.app.collectLocalLateReply(call.Token),
		}
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
	if err := a.DeleteThread(threadID); err != nil {
		log.Printf("thread tools: delete scratch thread %s for request %s: %v", threadID, token, err)
		return
	}
	// The row cascades with the thread; deleting it again is a no-op that
	// covers a thread already gone for another reason.
	if _, err := a.store.DeleteScratchThread(threadID); err != nil {
		log.Printf("thread tools: delete scratch row %s: %v", threadID, err)
	}
}

// collectLocalLateReply hands a late reply on this computer to the source
// row on this computer. A token no source row here owns belongs to another
// computer, whose poller collects it.
//
// The caller holds the token's settle lock, which is what makes the wake
// decision below exclusive with a timing-out wait. It reports whether a wake
// is owed; the caller delivers it after releasing that lock.
func (a *App) collectLocalLateReply(token string) bool {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		log.Printf("thread tools: read receipt %s: %v", token, err)
		return false
	}
	if !found {
		return false
	}
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		log.Printf("thread tools: read request %s: %v", token, err)
		return false
	}
	if !found {
		return false
	}
	if row.TargetComputerID != "" {
		// As settleReadReceiptLocked: another computer's receipt.
		log.Printf("thread tools: receipt %s collides with an outbound request; nothing collected", token)
		return false
	}
	if _, err := a.store.StoreThreadRequestLateReply(token, receipt.LateReply, receipt.LateReplyAt, receipt.AnswerAgent); err != nil {
		log.Printf("thread tools: store late reply %s: %v", token, err)
		return false
	}
	// The source has the answer durably; the destination may drop its copy.
	if _, err := a.store.AckThreadRequestReceipt(token, receipt.Revision, 0); err != nil {
		log.Printf("thread tools: acknowledge receipt %s: %v", token, err)
	}
	return a.finishThreadRequestCollection(token, true)
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

// threadRequestStateForReceipt maps a destination state onto the source's
// vocabulary. The two sets differ only where the source knows something the
// destination cannot (`unconfirmed`, `refused`), so everything settled has a
// name on both sides.
func threadRequestStateForReceipt(state string) string {
	switch state {
	case store.ThreadReceiptReplied:
		return store.ThreadRequestReplied
	case store.ThreadReceiptFinished:
		return store.ThreadRequestFinished
	case store.ThreadReceiptErrored:
		return store.ThreadRequestErrored
	case store.ThreadReceiptCancelled:
		return store.ThreadRequestCancelled
	case store.ThreadReceiptInterrupted:
		return store.ThreadRequestInterrupted
	case store.ThreadReceiptExpired:
		return store.ThreadRequestExpired
	default:
		return store.ThreadRequestErrored
	}
}
