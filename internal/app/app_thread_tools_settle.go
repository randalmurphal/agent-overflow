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
		a.subscribeGlobalTurnObserver(func(threadID string, evt provider.ProviderEvent) {
			// Top-level turn ends only: a subagent finishing inside the turn
			// is not the turn ending, and settling there would answer with a
			// Task tool's last words.
			if evt.Kind != provider.EventTurnComplete || strings.TrimSpace(evt.ParentToolUseID) != "" {
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
			a.goThreadRequestSettling(func() { a.settleReceiptsForEndedTurn(threadID) })
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
	// One turn can answer several requests: it consumed several messages.
	// They are all settled before anything is delivered, so their callers
	// are told once per turn rather than once per message.
	works := make([]threadSettlementWork, 0, len(tokens))
	for _, token := range tokens {
		work, err := a.settleReceiptOnTurnEnd(threadID, token)
		if err != nil {
			log.Printf("thread tools: settle request %s on %s: %v", token, threadID, err)
			continue
		}
		works = append(works, work)
	}
	a.runThreadSettlementWorks(works)
}

// settleReceiptOnTurnEnd reads what the receipt's own turn did and settles it.
//
// The binding is the turn, not the event: an end of some other turn on the
// same thread leaves this receipt's turn row unfinished and settles nothing,
// which is what keeps a user's own turn from answering an agent's request.
//
// It returns the work the settlement owes, for the caller to run with the
// work of the turn's other receipts (runThreadSettlementWorks).
func (a *App) settleReceiptOnTurnEnd(threadID, token string) (threadSettlementWork, error) {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return threadSettlementWork{}, err
	}
	if !found || receipt.TargetThreadID != threadID {
		// Gone, or bound to another thread: nothing here owns it any more.
		a.forgetReceiptRunning(threadID, token)
		return threadSettlementWork{}, nil
	}
	if receipt.State == store.ThreadReceiptAccepted {
		// The dispatch has not bound its turn yet. The gate is set before
		// the row, so dropping it here would take away the only thing
		// watching for this receipt's turn end.
		return threadSettlementWork{}, nil
	}
	if receipt.State != store.ThreadReceiptRunning {
		// Replied, cancelled or settled: nothing here owns it any more.
		a.forgetReceiptRunning(threadID, token)
		return threadSettlementWork{}, nil
	}
	turn, ended, err := a.receiptEndedTurn(threadID, token, receipt.MessageItemID)
	if err != nil {
		return threadSettlementWork{}, err
	}
	if !ended {
		// The request's own turn is still running. Some other turn ended.
		return threadSettlementWork{}, nil
	}
	settlement, err := a.turnSettlement(threadID, turn)
	if err != nil {
		return threadSettlementWork{}, err
	}
	work, _, err := a.settleReceiptUnderLock(token, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
	return work, err
}

// receiptEndedTurn returns the ended turn that answers a running receipt,
// or false while that answer is still to come.
//
// The receipt names the turn its message's row sits in until the provider
// consumes the message, and from then the turn that consumption named
// (recordRequestAnswerTurn). A message queued behind a running turn is not
// answered by that turn ending: while the provider holds it unconsumed the
// receipt waits, and once a turn start takes it, that turn answers.
//
// The pending state is read before the receipt. A send reported consumed has
// had its answer turn written, so the receipt read after it is current.
func (a *App) receiptEndedTurn(threadID, token, messageItemID string) (store.Turn, bool, error) {
	claimed, state := a.triage.PendingSendTurn(threadID, messageItemID)
	if state == triage.PendingSendWaiting {
		return store.Turn{}, false, nil
	}
	turnKey := ""
	if state == triage.PendingSendClaimed {
		turnKey = threadRequestTurnKey(threadID, claimed)
		if _, err := a.store.SetThreadReceiptAnswerTurn(token, threadID, turnKey); err != nil {
			return store.Turn{}, false, err
		}
	} else {
		receipt, found, err := a.store.GetThreadRequestReceipt(token)
		if err != nil || !found {
			return store.Turn{}, false, err
		}
		turnKey = receipt.TurnID
	}
	index, ok := threadRequestTurnIndex(threadID, turnKey)
	if !ok {
		return store.Turn{}, false, fmt.Errorf("receipt names turn %q, which is not a turn of this thread", turnKey)
	}
	if a.triage.OpenTurnIndex(threadID) == index {
		// The turn is running. A row that still carries an earlier
		// completion (a soft-closed turn the model continued, a re-sent
		// turn start) is not this turn's end.
		return store.Turn{}, false, nil
	}
	turn, found, err := a.store.GetTurnByThreadIndex(threadID, index)
	if err != nil || !found || turn.CompletedAt == nil {
		return store.Turn{}, false, err
	}
	return turn, true, nil
}

// recordRequestAnswerTurn is the consumed hook: the provider consumed a
// message, and answerTurn is the turn that answers it. Each request whose
// message the row carries has that turn written to its receipt. It runs
// inside triage's confirmation transaction, so it writes and returns.
func (a *App) recordRequestAnswerTurn(threadID string, sendIDs []string, answerTurn int) {
	turnKey := threadRequestTurnKey(threadID, answerTurn)
	for _, sendID := range sendIDs {
		token, ok := strings.CutPrefix(sendID, threadRequestSendIDPrefix)
		if !ok {
			continue
		}
		if _, err := a.store.SetThreadReceiptAnswerTurn(token, threadID, turnKey); err != nil {
			log.Printf("thread tools: record answer turn %d of %s for request %s: %v", answerTurn, threadID, token, err)
		}
	}
}

// turnSettlement maps one ended turn onto the answer its requester reads.
//
// A turn that answered without thread_reply still said something, and that
// last message is the most useful thing this computer has; it is reported as
// `final` so the caller is never told a fallback is the answer it asked for.
func (a *App) turnSettlement(threadID string, turn store.Turn) (store.ThreadRequestSettlement, error) {
	settledAt := time.Now().UnixMilli()
	if turn.CompletedAt != nil {
		settledAt = *turn.CompletedAt
	}
	switch {
	case turn.StopReason == "interrupted":
		return store.ThreadRequestSettlement{
			State:      store.ThreadReceiptInterrupted,
			Answer:     []byte(a.turnStopCause(threadID, turn.TurnIndex)),
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
		if work.token != "" && work.dropScratch {
			// The scratch thread has done its work. The answer lives in the
			// receipt, which the drop detaches from the thread first so a
			// paired computer that has not collected it yet still can; a
			// responder settling its own ask deletes the thread after its
			// reply is written.
			a.dropScratchThreadForRequest(work.token, work.targetThread)
		}
	}
}

// runThreadSettlementWorksDetached runs the deferred work on its own
// goroutine, for settlements written inside an operation that holds thread
// locks of its own: the delete port, and the dispatch binding a receipt to
// its turn. None of that work may wait on a lock its own caller is holding.
// Shutdown joins these through threadRequestsWG, and a wake dropped by a
// shutdown is redelivered by the next boot's undelivered-wake pass.
func (a *App) runThreadSettlementWorksDetached(works []threadSettlementWork) {
	works = slices.DeleteFunc(works, func(work threadSettlementWork) bool { return work.token == "" })
	if len(works) == 0 {
		return
	}
	a.goThreadRequestSettling(func() { a.runThreadSettlementWorks(works) })
}

// goThreadRequestSettling runs a settlement or a wake delivery on its own
// goroutine, joined at shutdown through threadRequestsWG, and reports
// whether it started it. Once shutdown has begun it runs nothing: work
// started then could outlive the join and the store. What it would have
// done is left to the next boot's sweeps.
func (a *App) goThreadRequestSettling(fn func()) bool {
	if a.lifeCtx().Err() != nil {
		return false
	}
	return a.threadRequestsWG.Go(fn)
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
// It returns the work the settlement owes.
//
// Those callers hold the answering thread's action lock, so they run the
// deferred work detached (runThreadSettlementWorksDetached): the wake takes
// the asking thread's own locks, and a subtree delete can be holding those
// too.
func (a *App) settleThreadReceiptNoScratch(token string, from []string, settlement store.ThreadRequestSettlement) (threadSettlementWork, error) {
	work, _, err := a.settleReceiptUnderLock(token, from, settlement, keepScratchThread)
	return work, err
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
// check, and the dispatch binding a receipt to its turn. It returns the work
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
	source, local, err := a.localThreadRequest(token)
	if err != nil {
		return threadSettlementWork{}, false, err
	}
	disarm := settlement.DisarmWake
	settlement.DisarmWake = false
	settled := false
	var collected store.ThreadRequestCollected
	if local {
		var result store.LocalThreadRequestSettlement
		result, err = a.store.SettleLocalThreadRequest(token, from, settlement, store.ThreadRequestWake{
			ReplyArms: settlement.AnswerKind == store.ThreadAnswerReply && !disarm && a.replyArmsWake(source),
			Disarm:    disarm,
		})
		settled, collected = result.Receipt, result.Request
	} else {
		settled, err = a.store.SettleThreadRequestReceipt(token, from, settlement)
	}
	if err != nil {
		return threadSettlementWork{}, false, err
	}
	if !settled {
		// Another path settled it first. Its collection ran with its answer.
		return threadSettlementWork{}, false, nil
	}
	a.forgetReceiptRunning(receipt.TargetThreadID, token)
	work := threadSettlementWork{
		token:        token,
		targetThread: receipt.TargetThreadID,
		dropScratch:  scratch,
	}
	work.wake, work.late = a.collectionWake(token, collected)
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
// request in the same transaction. It reports whether the receipt took it and
// what the request collected.
func (a *App) storeThreadReceiptLateReply(token string, reply []byte, at int64) (bool, store.ThreadRequestCollected, error) {
	source, local, err := a.localThreadRequest(token)
	if err != nil {
		return false, store.ThreadRequestCollected{}, err
	}
	if !local {
		stored, err := a.store.StoreThreadReceiptLateReply(token, reply, at)
		return stored, store.ThreadRequestCollected{}, err
	}
	result, err := a.store.StoreLocalThreadRequestLateReply(token, reply, at, a.replyArmsWake(source))
	return result.Receipt, result.Request, err
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
		stored, collected, err := t.app.storeThreadReceiptLateReply(call.Token, text, now)
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

// replyArmsWake reports whether an explicit reply to one of row's caller's
// requests is owed to that caller as a message.
//
// It is, whether or not the caller asked to be notified: `notify` decides
// whether a caller hears that a turn ended without an answer, and a
// thread_reply is the answer. The one caller it is not owed to is a thread
// that cannot take it: one that is gone or that this computer may no longer
// run. An archived caller is refused by the statement that stores the reply
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
	return a.store.CheckThreadExecutionAccess(thread) == nil
}

// finishThreadRequestCollection decides how a stored settlement reaches the
// caller. It is the delivery half of collection, shared by the local door
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
	a.wakeRequestWaits(token)
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		log.Printf("thread tools: read request %s for delivery: %v", token, err)
		return false
	}
	if !found || !row.Notify {
		return false
	}
	return !a.requestWaitActive(token)
}
