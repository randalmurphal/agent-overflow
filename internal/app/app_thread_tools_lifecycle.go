package app

import (
	"context"
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Stopping a request, and what happens to one when either of its threads
// goes away: the cancel that takes back queued or running work, and the
// lifecycle hooks the delete, archive and transfer ports call.

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
	interrupted, err := a.interruptTurnAtIndex(ctx, threadID, turnIndex, nil)
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
	name, _ := a.threadTools().threadToolsBackendName(backendID)
	return name
}
