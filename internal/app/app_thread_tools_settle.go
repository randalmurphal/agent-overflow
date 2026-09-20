package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
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
		a.subscribeGlobalTurnObserver(func(threadID string, evt provider.ProviderEvent) {
			// Top-level turn ends only: a subagent finishing inside the turn
			// is not the turn ending, and settling there would answer with a
			// Task tool's last words.
			if evt.Kind != provider.EventTurnComplete || strings.TrimSpace(evt.ParentToolUseID) != "" {
				return
			}
			// The gate is read here, on the read loop, because that is what
			// keeps a turn end on a thread with no requests to one map read
			// and no goroutine.
			if _, ok := a.runningReceiptTokens(threadID); !ok {
				return
			}
			// Settling runs OFF the read loop. It deletes the scratch thread
			// an ask ran in and can start the caller's session to deliver a
			// wake, and both stop or start a provider session: doing that on
			// the session's own read loop deadlocks on the goroutine being
			// waited for. Shutdown joins these through threadRequestsWG.
			if a.lifeCtx().Err() != nil {
				return
			}
			a.threadRequestsWG.Add(1)
			go func() {
				defer a.threadRequestsWG.Done()
				a.settleReceiptsForEndedTurn(threadID)
			}()
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
	for _, token := range tokens {
		if err := a.settleReceiptOnTurnEnd(threadID, token); err != nil {
			log.Printf("thread tools: settle request %s on %s: %v", token, threadID, err)
		}
	}
}

// settleReceiptOnTurnEnd reads what the receipt's own turn did and settles it.
//
// The binding is the turn, not the event: an end of some other turn on the
// same thread leaves this receipt's turn row unfinished and settles nothing,
// which is what keeps a user's own turn from answering an agent's request.
func (a *App) settleReceiptOnTurnEnd(threadID, token string) error {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return err
	}
	if !found || receipt.TargetThreadID != threadID {
		// Gone, or bound to another thread: nothing here owns it any more.
		a.forgetReceiptRunning(threadID, token)
		return nil
	}
	if receipt.State == store.ThreadReceiptAccepted {
		// The dispatch has not bound its turn yet. The gate is set before
		// the row, so dropping it here would take away the only thing
		// watching for this receipt's turn end.
		return nil
	}
	if receipt.State != store.ThreadReceiptRunning {
		// Replied, cancelled or settled: nothing here owns it any more.
		a.forgetReceiptRunning(threadID, token)
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
	if !found || turn.CompletedAt == nil {
		// The request's own turn is still running. Some other turn ended.
		return nil
	}
	settlement, err := a.turnSettlement(threadID, turn)
	if err != nil {
		return err
	}
	return a.settleThreadReceipt(token, []string{store.ThreadReceiptRunning}, settlement)
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
			Answer:     []byte("The turn was interrupted before it answered."),
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
	items, err := a.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return "", err
	}
	adapter := threadToolsApp{app: a}
	for index := len(items) - 1; index >= 0; index-- {
		row := items[index]
		if row.Kind != "assistant_text" || row.ParentID != "" {
			continue
		}
		text, _, err := adapter.itemBody(row, threadItemWholeBody)
		return text, err
	}
	return "", nil
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
// the token's settle lock released and never re-enters it.
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
// own: the delete port, and the dispatch binding a receipt to its turn. None
// of that work may wait on a lock its own caller is holding. Shutdown joins
// these through threadRequestsWG, and a wake dropped by a shutdown is redelivered
// by the next boot's undelivered-wake pass.
func (a *App) runThreadSettlementWorkDetached(work threadSettlementWork) {
	if work.token == "" || a.lifeCtx().Err() != nil {
		return
	}
	a.threadRequestsWG.Add(1)
	go func() {
		defer a.threadRequestsWG.Done()
		a.runThreadSettlementWork(work)
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
// check, and the dispatch binding a receipt to its turn. It returns the work
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
	settled, err := a.store.SettleThreadRequestReceipt(token, from, settlement)
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
	work.wake = a.collectLocalThreadRequest(token, false)
	return work, true, nil
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
		stored, err := t.app.store.StoreThreadReceiptLateReply(call.Token, text, 0)
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
			wake:         t.app.collectLocalThreadRequest(call.Token, true),
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

// collectLocalThreadRequest hands a settlement on this computer to the source
// row on this computer. A token no source row here owns belongs to another
// computer, whose poller collects it; that is the whole difference between
// the local path and the remote one.
//
// The caller holds the token's settle lock, which is what makes the wake
// decision below exclusive with a timing-out wait. It reports whether a wake
// is owed; the caller delivers it after releasing that lock.
func (a *App) collectLocalThreadRequest(token string, late bool) bool {
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
		// The row names a request THIS computer sent elsewhere. A receipt
		// carrying the same token was minted by a paired computer, and
		// settling the outbound row from it would write another computer's
		// answer into this one's request.
		log.Printf("thread tools: receipt %s collides with an outbound request; nothing collected", token)
		return false
	}
	if late {
		if _, err := a.store.StoreThreadRequestLateReply(token, receipt.LateReply, receipt.LateReplyAt); err != nil {
			log.Printf("thread tools: store late reply %s: %v", token, err)
			return false
		}
	} else {
		settled, err := a.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State:      threadRequestStateForReceipt(receipt.State),
			Answer:     receipt.Answer,
			AnswerKind: receipt.AnswerKind,
			SettledAt:  receipt.SettledAt,
			ExpiresAt:  receipt.ExpiresAt,
		})
		if err != nil {
			log.Printf("thread tools: settle request %s: %v", token, err)
			return false
		}
		if !settled {
			return false
		}
	}
	// The source has the answer durably; the destination may drop its copy.
	if _, err := a.store.AckThreadRequestReceipt(token, receipt.Revision, 0); err != nil {
		log.Printf("thread tools: acknowledge receipt %s: %v", token, err)
	}
	return a.finishThreadRequestCollection(token, late)
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

// threadWakeSendID and threadWakeLateSendID name the two messages one request
// can deliver. They are distinct so a late reply is never mistaken for a
// retry of the first wake and dropped by send admission.
func threadWakeSendID(token string) string     { return "thread-wake:" + token }
func threadWakeLateSendID(token string) string { return "thread-wake-late:" + token }

func threadWakeSendIDFor(token string, late bool) string {
	if late {
		return threadWakeLateSendID(token)
	}
	return threadWakeSendID(token)
}

// deliverThreadWake puts the answer in the caller's message queue, as the
// message it would have received had it been waiting.
//
// Queued rather than sent: the caller is usually mid-turn, and a message that
// interrupted it would take the agent off what it is doing to read an answer
// it asked to be told about later. The queue delivers it at the next turn
// boundary, which is exactly what the ack promised.
func (a *App) deliverThreadWake(token string, late bool) {
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil || !found {
		if err != nil {
			a.failThreadWake(token, "read the request", err)
		}
		return
	}
	if late && row.LateDeliveredAt != 0 {
		return
	}
	if !late && row.DeliveredAt != 0 {
		return
	}
	thread, err := a.store.GetThread(row.CallerThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		// The thread is gone. The request row is cascading away with it;
		// marking it keeps a concurrent reader from waiting on a wake that
		// will never be written.
		log.Printf("thread tools: request %s has no caller thread; nothing delivered", token)
		if _, markErr := a.store.MarkThreadRequestDelivered(token, store.ThreadWakeInline, 0, late); markErr != nil {
			log.Printf("thread tools: mark request %s undeliverable: %v", token, markErr)
		}
		return
	}
	if err != nil {
		// A read that failed says nothing about whether the thread exists.
		// The wake stays owed and the retry pass comes back to it.
		a.failThreadWake(token, "read the caller thread", err)
		return
	}
	if thread.Archived {
		// The answer the thread asked for arriving is exactly the reason to
		// bring it back; a wake queued on an archived thread would sit unread
		// behind a row the sidebar does not show.
		if _, err := a.UnarchiveThread(thread.ID); err != nil {
			a.failThreadWake(token, "unarchive the caller thread", err)
			return
		}
	}
	if err := a.store.CheckThreadExecutionAccess(thread); err != nil {
		a.failThreadWake(token, "deliver to "+thread.ID, err)
		return
	}
	body, err := a.threadWakeBody(row, late)
	if err != nil {
		a.failThreadWake(token, "render the wake", err)
		return
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), threadWakeLockTimeout)
	defer cancel()
	if err := a.queueAgentNotice(ctx, agentNotice{
		threadID: thread.ID,
		sendID:   threadWakeSendIDFor(token, late),
		// The wake is a message the app wrote on the answering thread's
		// behalf, and it is attributed to that thread for the same reason
		// the request itself is attributed to the sender: a person reading
		// the timeline can see where it came from.
		origin:       string(usermessage.OriginAgentThread),
		originThread: a.threadWakeOrigin(row),
		prepare:      func() (string, bool, error) { return body, true, nil },
		// One transaction writes the durable queue row and the delivery
		// mark, so a second observation of the same settlement cannot queue
		// the answer twice and a crash cannot lose it.
		persist: func(item store.FlushQueueItem) error {
			return a.store.QueueThreadWake(token, late, item)
		},
		startFailed: func(err error) string {
			return "A thread request answered, its message is queued, but the agent could not start: " + err.Error()
		},
	}); err != nil {
		a.failThreadWake(token, "queue the wake", err)
	}
}

// threadWakeLockTimeout bounds the wait for the caller thread's lock. A wake
// that cannot have it now is owed just the same, and the retry pass carries
// it.
const threadWakeLockTimeout = 10 * time.Second

// failThreadWake records why one wake could not be handed over. The reason
// is kept on the row, so the retry that gives up can say what stopped it and
// a person reading the ledger can see the same thing.
func (a *App) failThreadWake(token, what string, cause error) {
	log.Printf("thread tools: wake %s could not %s: %v", token, what, cause)
	if err := a.store.NoteThreadRequestWakeIssue(token, "Could not "+what+": "+cause.Error()); err != nil {
		logThreadRequestSweep("note wake issue for "+token, err)
	}
	// The retry pass owns this row now, and the sweep may be asleep.
	a.nudgeThreadRequestSweep()
}

// retryUndeliveredThreadWakes hands over the wakes a settlement could not.
//
// A settlement and its wake are two transactions, and the second one has ends
// that deliver nothing: a crash between them, a caller thread that could not
// be unarchived or locked in time, a render that failed. Nothing else revisits
// those rows, because the pollers read open requests and this one is settled,
// so the answer would sit in the ledger unread forever. The delivery mark is
// conditional, so a retry of a wake that did land is a no-op.
//
// Each row carries its own clock and attempt count. The delay doubles from
// half a minute towards the ten-minute ceiling, and a wake that has used its
// attempts is abandoned with the reason recorded rather than retried for the
// life of the row: what stops a wake is the caller thread's own state, which
// no number of retries changes.
func (a *App) retryUndeliveredThreadWakes(now time.Time) {
	rows, err := a.store.UndeliveredThreadRequestWakes(now.UnixMilli(), threadRequestWakeRetryBatch)
	if err != nil {
		logThreadRequestSweep("read undelivered wakes", err)
		return
	}
	for _, row := range rows {
		owed := undeliveredThreadWakes(row)
		if len(owed) == 0 {
			continue
		}
		if row.WakeAttempts >= threadWakeRetryAttempts {
			a.abandonThreadWake(row)
			continue
		}
		attempts := row.WakeAttempts + 1
		// Booked before the attempt: a delivery that fails, or that is
		// still holding a lock when the next pass comes round, must not
		// bring this row back at once.
		if err := a.store.ScheduleThreadRequestWake(row.Token,
			now.Add(threadWakeRetryDelay(attempts)).UnixMilli(), attempts); err != nil {
			logThreadRequestSweep("schedule the wake retry for "+row.Token, err)
			continue
		}
		for _, late := range owed {
			stillOwed := func() bool {
				unlock := a.threadRequestSettleLock(row.Token)
				defer unlock()
				return a.finishThreadRequestCollection(row.Token, late)
			}()
			if stillOwed {
				a.deliverThreadWakeDetached(row.Token, late)
			}
		}
	}
}

// abandonThreadWake ends the retry of a wake that cannot be handed over.
//
// The answer is not lost: it stays on the row and `thread_status` returns it
// with the reason the message never arrived. What is given up is the message,
// and with it the row's place in the undelivered set.
func (a *App) abandonThreadWake(row store.ThreadRequest) {
	issue := row.WakeIssue
	if issue == "" {
		issue = "The caller thread could not take this answer as a message."
	}
	abandoned, err := a.store.AbandonThreadRequestWake(row.Token, issue)
	if err != nil {
		logThreadRequestSweep("abandon the wake for "+row.Token, err)
		return
	}
	if abandoned {
		log.Printf("thread tools: giving up on the wake for request %s after %d attempts: %s",
			row.Token, row.WakeAttempts, issue)
	}
}

// undeliveredThreadWakes names which of one row's two wakes are still owed.
func undeliveredThreadWakes(row store.ThreadRequest) []bool {
	owed := make([]bool, 0, 2)
	if row.SettledAt != 0 && row.DeliveredAt == 0 {
		owed = append(owed, false)
	}
	if len(row.LateReply) > 0 && row.LateDeliveredAt == 0 {
		owed = append(owed, true)
	}
	return owed
}

// threadWakeRetryDelay doubles the base delay for each attempt already made,
// up to the ceiling.
func threadWakeRetryDelay(attempts int64) time.Duration {
	delay := threadWakeRetryBase
	for range attempts - 1 {
		delay *= 2
		if delay >= threadWakeRetryCap {
			return threadWakeRetryCap
		}
	}
	return delay
}

// threadWakeRetryBase, threadWakeRetryCap and threadWakeRetryAttempts pace
// and bound the retry; threadRequestWakeRetryBatch bounds one pass. This is
// a recovery net behind a delivery that normally happens with the
// settlement, so it is slow and it ends.
const (
	threadWakeRetryBase         = 30 * time.Second
	threadWakeRetryCap          = 10 * time.Minute
	threadWakeRetryAttempts     = 8
	threadRequestWakeRetryBatch = 32
)

// threadWakeOrigin attributes a wake to the thread that answered, from the
// same two sources threadWakeBody reads: a thread on this computer has a
// row here, and a thread on another computer is named by what its own
// computer reported when the request's target was recorded.
func (a *App) threadWakeOrigin(row store.ThreadRequest) *usermessage.OriginThread {
	origin := &usermessage.OriginThread{ThreadID: row.TargetThreadID, Token: row.Token}
	if row.TargetComputerID != "" {
		origin.Title = row.TargetThreadTitle
		origin.ComputerName, origin.ComputerID = threadToolsApp{app: a}.threadToolsBackendName(row.TargetComputerID)
		return origin
	}
	if row.TargetThreadID != "" {
		if thread, err := a.store.GetThread(row.TargetThreadID); err == nil {
			origin.Title = thread.Title
		}
	}
	return origin
}

// threadWakeBody renders one wake from the fixed template. The kind is what
// the settled state means to the caller, which is not always what it means to
// the destination: a reminder is `finished` on both sides and reads as
// neither a reply nor a thread's last message.
func (a *App) threadWakeBody(row store.ThreadRequest, late bool) (string, error) {
	wake := threadtools.Wake{
		Token:    row.Token,
		ThreadID: row.TargetThreadID,
	}
	if row.SettledAt > 0 {
		wake.Age = time.Duration(time.Now().UnixMilli()-row.SettledAt) * time.Millisecond
	}
	if row.TargetComputerID != "" {
		// A thread on another computer has no row here: its title is what
		// that computer reported, kept on the request so a wake rendered
		// after a restart still names it, and the computer is named the way
		// every other row names it.
		wake.Title = row.TargetThreadTitle
		wake.Computer, _ = threadToolsApp{app: a}.threadToolsBackendName(row.TargetComputerID)
	} else if row.TargetThreadID != "" {
		if thread, err := a.store.GetThread(row.TargetThreadID); err == nil {
			wake.Title = thread.Title
		}
	}
	switch {
	case late:
		wake.Kind, wake.Text = threadtools.WakeReply, string(row.LateReply)
		wake.Age = time.Duration(time.Now().UnixMilli()-row.LateReplyAt) * time.Millisecond
	case row.Kind == store.ThreadRequestRemind:
		wake.Kind, wake.Text = threadtools.WakeReminder, string(row.Answer)
	case row.State == store.ThreadRequestReplied:
		wake.Kind, wake.Text = threadtools.WakeReply, string(row.Answer)
	case row.State == store.ThreadRequestFinished:
		wake.Kind, wake.Text = threadtools.WakeFinished, string(row.Answer)
	case row.State == store.ThreadRequestErrored, row.State == store.ThreadRequestRefused:
		wake.Kind, wake.Text = threadtools.WakeErrored, string(row.Answer)
	case row.State == store.ThreadRequestCancelled:
		wake.Kind = threadtools.WakeCancelled
	case row.State == store.ThreadRequestInterrupted:
		wake.Kind = threadtools.WakeInterrupted
	case row.State == store.ThreadRequestExpired:
		wake.Kind = threadtools.WakeExpired
	default:
		return "", fmt.Errorf("request %s is %s, which owes no wake", row.Token, row.State)
	}
	return wake.String(), nil
}

// stopRequestWork settles the receipt cancelled and then takes back whatever
// the request had already set in motion.
//
// The settlement comes FIRST so the turn observer, which settles only a
// `running` receipt, cannot race the interrupt and report the interrupted
// turn as this request's answer.
func (a *App) stopThreadRequestWork(ctx context.Context, token string) (string, error) {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return "", err
	}
	if !found {
		return threadtools.EffectNothing, nil
	}
	state := receipt.State
	// The door takes the settle lock for the settlement and releases it
	// before the queue and provider work below: both take thread locks, and
	// neither settles anything.
	if err := a.settleThreadReceipt(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadReceiptCancelled,
		Answer:     []byte("The sender cancelled this request."),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		return "", err
	}

	switch state {
	case store.ThreadReceiptAccepted:
		// The message was still on the queue when the receipt was read, so
		// the first thing to try is taking it back.
		removal, err := a.removeQueuedItem(ctx, receipt.TargetThreadID, threadRequestSendID(token))
		if err != nil {
			return "", err
		}
		if removal == queueRemovalRemoved {
			return threadtools.EffectQueuedRemoved, nil
		}
		// It is not on the queue any more: the dispatch claimed it while the
		// cancel was running, or it reached the provider before the receipt
		// moved to `running`. Either way a turn is starting from it, and
		// stopping that turn is what the cancel promised.
		return a.interruptDispatchedRequest(ctx, receipt.TargetThreadID, threadRequestSendID(token))
	case store.ThreadReceiptRunning:
		index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID)
		if !ok {
			return threadtools.EffectNothing, nil
		}
		turn, found, err := a.store.GetTurnByThreadIndex(receipt.TargetThreadID, index)
		if err != nil {
			return "", err
		}
		// Only the request's own turn is interrupted. A turn that is over
		// has nothing to stop, and a LATER turn belongs to whoever started
		// it: the interrupt is fenced on this turn index, so it stops that
		// turn or nothing.
		//
		// A turn row that does not exist yet is neither. The message is at
		// the provider and the turn is about to start, so the interrupt is
		// what stops it; on a thread that turns out to be idle it is a
		// no-op.
		if found && turn.CompletedAt != nil {
			return threadtools.EffectNothing, nil
		}
		return a.interruptRequestTurn(ctx, receipt.TargetThreadID, index)
	}
	return threadtools.EffectNothing, nil
}

// interruptDispatchedRequest stops the turn a request's message started, for
// a cancel that found nothing left to take off the queue. The send id names
// the message, and the row it produced names the turn: without that the
// cancel would report that there was nothing to stop while the turn it was
// asked to stop ran on.
func (a *App) interruptDispatchedRequest(ctx context.Context, threadID, sendID string) (string, error) {
	record, found, err := a.findRecordedSend(threadID, sendID)
	if err != nil {
		return "", err
	}
	if !found || !record.dispatched {
		// The message was never sent, or it was restored into the composer.
		return threadtools.EffectNothing, nil
	}
	return a.interruptRequestTurn(ctx, threadID, record.item.TurnIndex)
}

// interruptRequestTurn interrupts one thread only while the named turn is the
// one it is running, and reports what happened in the cancel's vocabulary.
func (a *App) interruptRequestTurn(ctx context.Context, threadID string, turnIndex int) (string, error) {
	interrupted, err := a.interruptTurnAtIndex(ctx, threadID, turnIndex)
	if err != nil {
		return "", err
	}
	if !interrupted {
		return threadtools.EffectNothing, nil
	}
	return threadtools.EffectInterrupted, nil
}

// Lifecycle: what happens to a request when one of its two threads goes
// away. Both halves are idempotent, because the delete port runs twice and a
// transfer can be retried.

// cancelThreadRequests is the caller side: a thread that is being deleted,
// archived or moved can no longer receive an answer, so its parked calls end,
// its wakes are disarmed, and the asks it owns are stopped.
//
// Spawns and sends are left running. The person asked for that work through
// this thread, not for this thread, and killing a spawned thread because its
// caller was archived would destroy work nobody said to stop. An ask is the
// exception: its scratch thread exists only to answer this caller.
func (a *App) cancelThreadRequests(ctx context.Context, callerThreadID string) error {
	a.cancelThreadRequestWaits(callerThreadID)
	if _, err := a.store.SetThreadRequestsNotifyForCaller(callerThreadID, false); err != nil {
		return err
	}
	rows, err := a.store.ListThreadRequestsByCaller(callerThreadID, threadCancelLineagePage, 0)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if threadRequestSettled(row) || row.Kind != store.ThreadRequestAsk {
			continue
		}
		if err := a.cancelOwnedAsk(ctx, row); err != nil {
			// Best effort by design: the caller's own operation is what is
			// being served, and a scratch thread left running is swept at the
			// next boot, here or on the computer that holds it.
			log.Printf("thread tools: cancel ask %s for %s: %v", row.Token, callerThreadID, err)
		}
	}
	return nil
}

// cancelOwnedAsk stops one ask this caller owns, wherever it is running.
func (a *App) cancelOwnedAsk(ctx context.Context, row store.ThreadRequest) error {
	if row.TargetComputerID == "" {
		_, err := a.stopThreadRequestWork(ctx, row.Token)
		return err
	}
	if a.backends == nil {
		return nil
	}
	call, cancel := context.WithTimeout(ctx, threadCancelTimeout)
	defer cancel()
	var reply ThreadPeerReply
	// A forwarded call is refused unless it names both the thread and the
	// computer it came from, and this one is forwarded like any other. The
	// identity comes from the backend rather than the thread row because
	// this runs while that thread is being deleted, archived or moved.
	backendID, _ := a.backendIdentity()
	if err := a.backends.CallThreadPeer(call, row.TargetComputerID, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool:  "thread_cancel",
		Token: row.Token,
		Source: threadtools.Caller{
			ThreadID:     row.CallerThreadID,
			ComputerID:   backendID,
			ComputerName: a.backendDisplayName(),
		},
	}); err != nil {
		return a.threadOperationError("cancel", row.TargetComputerID, row.TargetThreadID, err)
	}
	if reply.Request == nil {
		return nil
	}
	_, err := a.applyThreadPeerRequest(row.Token, row.TargetComputerID, *reply.Request)
	return err
}

// settleReceiptsForThread is the target side: a thread that is being deleted
// or moved cannot finish what it was asked, and every open receipt against it
// settles with the reason before its rows go.
//
// It runs BEFORE the rows are deleted, because the settlement reads the
// receipt and the collection writes the caller's row; afterwards there would
// be nothing to say and nobody to say it to.
func (a *App) settleReceiptsForThread(threadID, state, reason string) {
	receipts, err := a.store.ListThreadRequestReceiptsForThread(threadID)
	if err != nil {
		log.Printf("thread tools: list receipts for %s: %v", threadID, err)
		return
	}
	for _, receipt := range receipts {
		if receipt.State != store.ThreadReceiptAccepted && receipt.State != store.ThreadReceiptRunning {
			continue
		}
		// No scratch cleanup here. The delete and move ports own this
		// thread's destruction and are running inside it: deleting it again
		// from the settlement would wait for the thread action lock the
		// caller already holds, which is nobody's to release.
		if _, err := a.settleThreadReceiptNoScratch(receipt.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
			State:      state,
			Answer:     []byte(reason),
			AnswerKind: store.ThreadAnswerError,
		}); err != nil {
			log.Printf("thread tools: settle receipt %s as %s: %v", receipt.Token, state, err)
		}
	}
	// This thread is going: deleted, or handed to another computer. The
	// settlements above are what their callers collect, and a receipt still
	// bound to the thread would cascade away with it before a paired
	// computer's poller could read one.
	if _, err := a.store.DetachThreadReceiptsFromThread(threadID); err != nil {
		log.Printf("thread tools: detach receipts from %s: %v", threadID, err)
	}
}

// stopThreadRequestsForDeletedThread is the delete port's whole thread-tools
// half: the thread is about to stop existing, so it is both a caller whose
// requests must be cancelled and a target whose receipts must be settled.
func (a *App) stopThreadRequestsForDeletedThread(ctx context.Context, threadID string) error {
	a.settleReceiptsForThread(threadID, store.ThreadReceiptErrored, "The thread was deleted before it answered.")
	return a.cancelThreadRequests(ctx, threadID)
}

// stopThreadRequestsForMovedThread is the same for a thread this computer is
// handing to another. The receipts stay here and the caller collects them as
// usual; nothing is re-addressed, because the request was made of a thread on
// this computer and that thread is gone from it.
func (a *App) stopThreadRequestsForMovedThread(ctx context.Context, threadID, destination string) error {
	reason := "The thread was moved to another computer before it answered."
	if destination != "" {
		reason = "The thread was moved to " + destination + " before it answered."
	}
	a.settleReceiptsForThread(threadID, store.ThreadReceiptInterrupted, reason)
	return a.cancelThreadRequests(ctx, threadID)
}

// threadRequestDestinationName names the computer a moved thread went to, for
// the settlement text its open receipts carry. An unknown backend leaves the
// reason unqualified rather than naming an id nobody reads.
func (a *App) threadRequestDestinationName(backendID string) string {
	name, _ := threadToolsApp{app: a}.threadToolsBackendName(backendID)
	return name
}
