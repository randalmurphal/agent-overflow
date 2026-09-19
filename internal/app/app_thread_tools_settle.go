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
	unlock := a.threadRequestSettleLock(token)
	defer unlock()

	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return err
	}
	if !found || receipt.State != store.ThreadReceiptRunning || receipt.TargetThreadID != threadID {
		// Replied, cancelled or gone: nothing here owns it any more.
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
		return adapter.itemBodyText(row, threadtools.WakePreviewBytes)
	}
	return "", nil
}

// settleThreadReceipt writes one settlement on the destination row and hands
// it to the source. It is the single door: the turn observer, thread_cancel,
// the lifecycle hooks and the boot sweep all arrive here, so the scratch
// cleanup and the collection happen exactly once per settled request.
//
// The caller holds the token's settle lock.
func (a *App) settleThreadReceipt(token string, from []string, settlement store.ThreadRequestSettlement) error {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	settled, err := a.store.SettleThreadRequestReceipt(token, from, settlement)
	if err != nil {
		return err
	}
	if !settled {
		// Another path settled it first. Its collection ran with its answer.
		return nil
	}
	a.forgetReceiptRunning(receipt.TargetThreadID, token)
	a.collectLocalThreadRequest(token, false)
	// The scratch thread has done its work. The answer lives in the receipt
	// and the source row, so deleting the thread loses nothing; a responder
	// settling its own ask deletes it after its reply is written instead.
	a.dropScratchThreadForRequest(token, receipt.TargetThreadID)
	return nil
}

// Reply settles one request the caller's thread was asked.
//
// The settlement runs under the token's settle lock and the cleanup after it,
// never inside it: an in-process caller runs the after-response work on this
// same goroutine, and the lock is not reentrant.
func (t threadToolsApp) Reply(ctx context.Context, caller threadtools.Caller, call threadtools.ReplyCall) (threadtools.ReplyAck, error) {
	ack, err := t.replyLocked(caller, call)
	if err != nil {
		return threadtools.ReplyAck{}, err
	}
	// The scratch thread this reply was written in is deleted only after the
	// response reaches the responder: a tool call whose own thread vanished
	// mid-call would fail after having done exactly what it was asked.
	t.afterResponse(ctx, func() {
		unlock := t.app.threadRequestSettleLock(call.Token)
		defer unlock()
		t.app.dropScratchThreadForRequest(call.Token, caller.ThreadID)
	})
	return ack, nil
}

func (t threadToolsApp) replyLocked(caller threadtools.Caller, call threadtools.ReplyCall) (threadtools.ReplyAck, error) {
	unlock := t.app.threadRequestSettleLock(call.Token)
	defer unlock()

	receipt, found, err := t.app.store.GetThreadRequestReceipt(call.Token)
	if err != nil {
		return threadtools.ReplyAck{}, err
	}
	if !found {
		return threadtools.ReplyAck{}, errorsx.Public(threadtools.CodeRequestUnknown,
			"No request carries that token on this computer. Take the token from the \"Agent request\" footer of the message you are answering.", nil)
	}
	if receipt.TargetThreadID != caller.ThreadID {
		return threadtools.ReplyAck{}, errorsx.Public(threadtools.CodeRequestNotYours,
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
		if err := t.app.settleThreadReceiptNoScratch(call.Token, []string{store.ThreadReceiptRunning}, store.ThreadRequestSettlement{
			State:      store.ThreadReceiptReplied,
			Answer:     text,
			AnswerKind: store.ThreadAnswerReply,
		}); err != nil {
			return threadtools.ReplyAck{}, err
		}
		ack.Accepted, ack.State = true, threadtools.RequestReplied

	case store.ThreadReceiptReplied:
		// A retry of the same reply is the same reply, not a second one.
		if !bytes.Equal(receipt.Answer, text) {
			return threadtools.ReplyAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request was already answered with different text. Reply once per token; send a correction with thread_send.", nil)
		}
		ack.State = threadtools.RequestReplied

	case store.ThreadReceiptFinished:
		// The turn ended before the reply. The sender has already been told
		// so, and this arrives as a follow-up revision rather than a refusal.
		ack.Late = true
		if len(receipt.LateReply) > 0 {
			if !bytes.Equal(receipt.LateReply, text) {
				return threadtools.ReplyAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
					"That request already carries a late reply with different text. Reply once per token.", nil)
			}
			ack.State = threadtools.RequestFinished
			break
		}
		stored, err := t.app.store.StoreThreadReceiptLateReply(call.Token, text, 0)
		if err != nil {
			return threadtools.ReplyAck{}, err
		}
		if !stored {
			return threadtools.ReplyAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"That request stopped accepting replies while this one was being written.", nil)
		}
		t.app.collectLocalThreadRequest(call.Token, true)
		ack.Accepted, ack.State = true, threadtools.RequestFinished

	default:
		return threadtools.ReplyAck{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("That request is %s and no longer takes a reply.", receipt.State), nil)
	}

	if updated, found, err := t.app.store.GetThreadRequestReceipt(call.Token); err == nil && found {
		ack.Revision = updated.Revision
	}
	return ack, nil
}

// settleThreadReceiptNoScratch is settleThreadReceipt without the scratch
// deletion, for the responder's own thread_reply: the same settlement and the
// same collection, with the thread kept alive until the response is written.
func (a *App) settleThreadReceiptNoScratch(token string, from []string, settlement store.ThreadRequestSettlement) error {
	settled, err := a.store.SettleThreadRequestReceipt(token, from, settlement)
	if err != nil {
		return err
	}
	if !settled {
		return errorsx.Public(threadtools.CodeInvalidRequest,
			"That request settled while this reply was being written.", nil)
	}
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err == nil && found {
		a.forgetReceiptRunning(receipt.TargetThreadID, token)
	}
	a.collectLocalThreadRequest(token, false)
	return nil
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
// decision below exclusive with a timing-out wait.
func (a *App) collectLocalThreadRequest(token string, late bool) {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		log.Printf("thread tools: read receipt %s: %v", token, err)
		return
	}
	if !found {
		return
	}
	row, found, err := a.store.GetThreadRequest(token)
	if err != nil {
		log.Printf("thread tools: read request %s: %v", token, err)
		return
	}
	if !found {
		return
	}
	if late {
		if _, err := a.store.StoreThreadRequestLateReply(token, receipt.LateReply, receipt.LateReplyAt); err != nil {
			log.Printf("thread tools: store late reply %s: %v", token, err)
			return
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
			return
		}
		if !settled {
			return
		}
	}
	// The source has the answer durably; the destination may drop its copy.
	if _, err := a.store.AckThreadRequestReceipt(token, receipt.Revision, 0); err != nil {
		log.Printf("thread tools: acknowledge receipt %s: %v", token, err)
	}
	// A parked wait is the delivery: it returns the answer in the reply to
	// the call that is waiting, so no message is owed.
	a.wakeRequestWaits(token)
	if !row.Notify || a.requestWaitActive(token) {
		return
	}
	a.deliverThreadWake(token, late)
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
			log.Printf("thread tools: read request %s for delivery: %v", token, err)
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
	if err != nil {
		// The thread is gone. The request row is cascading away with it;
		// marking it keeps a concurrent reader from waiting on a wake that
		// will never be written.
		log.Printf("thread tools: request %s has no caller thread; nothing delivered", token)
		if _, markErr := a.store.MarkThreadRequestDelivered(token, store.ThreadWakeInline, 0, late); markErr != nil {
			log.Printf("thread tools: mark request %s undeliverable: %v", token, markErr)
		}
		return
	}
	if thread.Archived {
		// The answer the thread asked for arriving is exactly the reason to
		// bring it back; a wake queued on an archived thread would sit unread
		// behind a row the sidebar does not show.
		if _, err := a.UnarchiveThread(thread.ID); err != nil {
			log.Printf("thread tools: unarchive %s for request %s: %v", thread.ID, token, err)
			return
		}
	}
	if err := a.store.CheckThreadExecutionAccess(thread); err != nil {
		log.Printf("thread tools: request %s cannot be delivered to %s: %v", token, thread.ID, err)
		return
	}
	body, err := a.threadWakeBody(row, late)
	if err != nil {
		log.Printf("thread tools: render wake %s: %v", token, err)
		return
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), 10*time.Second)
	defer cancel()
	unlock, err := a.threadLocks().LockCtx(ctx, thread.ID)
	if err != nil {
		log.Printf("thread tools: wake %s could not take the thread lock: %v", token, err)
		return
	}
	queued := func() error {
		defer unlock()
		_, err := a.registerQueueItem(thread.ID, body, SendMessageOptions{SendID: threadWakeSendIDFor(token, late)}, injectedQueueOptions{
			preserveDraft: true,
			// The wake is a message the app wrote on the answering thread's
			// behalf, and it is attributed to that thread for the same
			// reason the request itself is attributed to the sender: a
			// person reading the timeline can see where it came from.
			origin:       string(usermessage.OriginAgentThread),
			originThread: a.threadWakeOrigin(row),
			// One transaction writes the durable queue row and the delivery
			// mark, so a second observation of the same settlement cannot
			// queue the answer twice and a crash cannot lose it.
			persist: func(item store.FlushQueueItem) error {
				return a.store.QueueThreadWake(token, late, item)
			},
		})
		return err
	}()
	if queued != nil {
		log.Printf("thread tools: queue wake for request %s: %v", token, queued)
		return
	}
	if _, live := a.sessionManager().get(thread.ID); live {
		return
	}
	// The message is already in the ordinary durable queue; startup's flush
	// trigger dispatches it. A failure here leaves it queued for the next
	// start rather than losing the answer.
	if err := a.startSession(a.lifeCtx(), thread.ID); err != nil {
		a.emitWireErrorToThread(thread.ID, "A thread request answered, its message is queued, but the agent could not start: "+err.Error())
	}
}

// threadWakeOrigin attributes a wake to the thread that answered.
func (a *App) threadWakeOrigin(row store.ThreadRequest) *usermessage.OriginThread {
	origin := &usermessage.OriginThread{ThreadID: row.TargetThreadID, Token: row.Token}
	if row.TargetThreadID != "" {
		if thread, err := a.store.GetThread(row.TargetThreadID); err == nil {
			origin.Title = thread.Title
		}
	}
	if row.TargetComputerID != "" {
		origin.ComputerName, origin.ComputerID = threadToolsApp{app: a}.threadToolsBackendName(row.TargetComputerID)
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
	if row.TargetThreadID != "" {
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
	unlock := a.threadRequestSettleLock(token)
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		unlock()
		return "", err
	}
	if !found {
		unlock()
		return threadCancelNothing, nil
	}
	state := receipt.State
	if err := a.settleThreadReceipt(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadReceiptCancelled,
		Answer:     []byte("The sender cancelled this request."),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		unlock()
		return "", err
	}
	// The settle lock is released before the queue and provider work: both
	// take thread locks, and neither settles anything.
	unlock()

	switch state {
	case store.ThreadReceiptAccepted:
		// The message never reached the provider, so there is a queued
		// message to take back and no turn to stop.
		removed, err := a.removeQueuedItem(ctx, receipt.TargetThreadID, threadRequestSendID(token))
		if err != nil {
			return "", err
		}
		if removed {
			return threadCancelQueuedRemoved, nil
		}
		return threadCancelNothing, nil
	case store.ThreadReceiptRunning:
		index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID)
		if !ok {
			return threadCancelNothing, nil
		}
		turn, found, err := a.store.GetTurnByThreadIndex(receipt.TargetThreadID, index)
		if err != nil {
			return "", err
		}
		// Only the request's own turn is interrupted. A turn that is over
		// has nothing to stop, and a LATER turn belongs to whoever started
		// it: stopping that would take work away from the person or the
		// thread that asked for it.
		//
		// A turn row that does not exist yet is neither. The message is at
		// the provider and the turn is about to start, so the interrupt is
		// what stops it; on a thread that turns out to be idle it is a
		// no-op.
		if found && turn.CompletedAt != nil {
			return threadCancelNothing, nil
		}
		if a.threadMovedPastTurn(receipt.TargetThreadID, index) {
			return threadCancelNothing, nil
		}
		if err := a.interruptTurnCtx(ctx, receipt.TargetThreadID); err != nil {
			return "", err
		}
		return threadCancelInterrupted, nil
	}
	return threadCancelNothing, nil
}

// threadMovedPastTurn reports whether the thread is now running a DIFFERENT
// turn than the one at this index. Only that answer is a reason not to
// interrupt: the later turn belongs to whoever started it.
//
// A thread with no live turn has not moved past anything. The request's
// message reaches the provider before the turn start comes back, so a cancel
// in that window must still stop the work rather than report that there was
// none; an interrupt of a thread that turns out to be idle is a no-op.
func (a *App) threadMovedPastTurn(threadID string, turnIndex int) bool {
	if a.triage == nil {
		return false
	}
	snapshot := a.triage.LiveStateSnapshotForThread(threadID)
	return snapshot.ActiveTurn != nil && snapshot.ActiveTurn.TurnIndex != turnIndex
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
		if _, err := a.stopThreadRequestWork(ctx, row.Token); err != nil {
			// Best effort by design: the caller's own operation is what is
			// being served, and a scratch thread left running is swept at the
			// next boot.
			log.Printf("thread tools: cancel ask %s for %s: %v", row.Token, callerThreadID, err)
		}
	}
	return nil
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
		func() {
			unlock := a.threadRequestSettleLock(receipt.Token)
			defer unlock()
			if err := a.settleThreadReceipt(receipt.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
				State:      state,
				Answer:     []byte(reason),
				AnswerKind: store.ThreadAnswerError,
			}); err != nil {
				log.Printf("thread tools: settle receipt %s as %s: %v", receipt.Token, state, err)
			}
		}()
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
