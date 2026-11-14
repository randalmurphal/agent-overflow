package app

import (
	"context"
	"errors"
	"log"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// Stopping a request, and what happens to one when either of its threads
// goes away: the cancel that takes back queued or running work, and the
// lifecycle hooks the delete, archive and transfer ports call.

// stopRequestWork settles the receipt cancelled and then takes back whatever
// the request had already set in motion.
//
// The settlement comes FIRST so the turn observer, which settles only a
// `running` receipt, cannot race the interrupt and report the interrupted
// turn as this request's answer. Once it is settled the stop always runs to
// the end, and a repeated cancel of a cancelled receipt stops whatever the
// first one did not reach: the turn bound to it, or its message while still
// queued.
func (a *App) stopThreadRequestWork(ctx context.Context, token string) (string, error) {
	return a.stopThreadRequestWorkFor(ctx, token, threadRequestSenderCancel)
}

// Settlement answers of a cancelled request: the sender's own cancel, and a
// person's Stop in the sender's Agents tray, which the sender's agent is
// woken with (its notify stays armed) so it knows the stop was not its own.
const (
	threadRequestSenderCancel = "The sender cancelled this request."
	threadRequestUserStop     = "The user stopped it from the Agents tray; you did not cancel it."
	threadRequestUserWithdrew = "The user withdrew it from the recipient's queue before its agent read it."
)

// stopFromAgentsTray is the user's Stop of a request in its caller's Agents
// tray. It is the user's stop, so the stopped thread's timeline says so, and
// it carries no kill consent: the tray stops the request's turn, not the
// background agents that turn started.
func stopFromAgentsTray() turnStop {
	return turnStop{
		label: triage.StoppedByUser,
		cause: "The user stopped its turn from the Agents tray before it answered.",
	}
}

// requestCancelStop is who a request's cancel names as stopping its turn,
// for any other request that turn was answering: the user for a Stop from
// the Agents tray, otherwise the thread that sent the request.
func (a *App) requestCancelStop(receipt store.ThreadRequestReceipt, answer string) turnStop {
	if answer == threadRequestUserStop {
		return stopFromAgentsTray()
	}
	return a.receiptCanceller(receipt)
}

// stopThreadRequestWorkFor is stopThreadRequestWork with the answer the
// cancelled receipt settles with.
func (a *App) stopThreadRequestWorkFor(ctx context.Context, token, answer string) (string, error) {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return "", err
	}
	if !found {
		return threadtools.EffectNothing, nil
	}
	state := receipt.State
	if hook := a.cancelReadForTest; hook != nil {
		hook(token)
	}
	repeated := false
	switch {
	case slices.Contains(store.ThreadReceiptOpenStates(), state):
		// The door takes the settle lock for the settlement and releases it
		// before the queue and provider work below: both take thread locks,
		// and neither settles anything.
		work, settled, err := a.settleReceiptUnderLock(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
			State:      store.ThreadReceiptCancelled,
			Answer:     []byte(answer),
			AnswerKind: store.ThreadAnswerError,
		}, dropScratchThread)
		if err != nil {
			return "", err
		}
		a.runThreadSettlementWork(work)
		// A receipt something else settled first (a reply, a turn end, an
		// earlier cancel), before the read above or after it, has nothing
		// left to stop: its turn, still running or not, is no longer this
		// request's work. Only the settlement's own answer says which
		// happened last.
		if !settled {
			return threadtools.EffectNothing, nil
		}
	case state == store.ThreadReceiptCancelled:
		// A turn bound to the receipt means it was running when cancelled,
		// and that turn is the one to stop. Otherwise its message may still
		// be queued; a repeat takes it back if so and touches no turn, since
		// it cannot name one.
		repeated = true
		state = store.ThreadReceiptAccepted
		if receipt.TurnID != "" {
			state = store.ThreadReceiptRunning
		}
	default:
		return threadtools.EffectNothing, nil
	}
	ctx = context.WithoutCancel(ctx)
	stop := a.requestCancelStop(receipt, answer)

	if state == store.ThreadReceiptAccepted {
		// The message was still on the queue when the receipt was read, so
		// the first thing to try is taking it back.
		removal, err := a.removeQueuedItem(ctx, receipt.TargetThreadID, threadRequestSendID(token))
		if err != nil {
			return "", err
		}
		if removal == queueRemovalRemoved {
			return threadtools.EffectQueuedRemoved, nil
		}
	}
	// The turn is read after the settlement: a delivery that bound the
	// receipt first names the turn that read the message, and none can bind
	// it after.
	if settled, found, err := a.store.GetThreadRequestReceipt(token); err != nil {
		return "", err
	} else if found {
		receipt = settled
	}
	if index, ok := threadRequestTurnIndex(receipt.TargetThreadID, receipt.TurnID); ok {
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
		// A message drained into a turn already running joined a turn the
		// person or another request started; a token never interrupts it.
		if !receipt.OwnTurn {
			return threadtools.EffectLeftRunning, nil
		}
		return a.interruptRequestTurn(ctx, receipt.TargetThreadID, index, stop)
	}
	if repeated {
		return threadtools.EffectNothing, nil
	}
	if state == store.ThreadReceiptAccepted {
		// It is not on the queue any more and no delivery bound it: the
		// dispatch claimed it while the cancel was running, or it reached
		// the provider before the receipt moved to `running`.
		return a.interruptDispatchedRequest(ctx, receipt.TargetThreadID, threadRequestSendID(token), stop)
	}
	// Running with no turn bound: the message is with the provider and no
	// turn has read it yet. The turn running meanwhile is someone else's,
	// and a token never interrupts that.
	return threadtools.EffectLeftUnread, nil
}

// receiptCanceller is the stop of a request cancelled by its own sender.
// The request itself is already settled cancelled; the cause reaches any
// other request the same turn was answering.
func (a *App) receiptCanceller(receipt store.ThreadRequestReceipt) turnStop {
	computer := ""
	if receipt.OwnerDeviceID != threadReceiptLocalOwner {
		computer = a.stoppingComputerName(receipt.SourceComputerID)
	}
	return stopByThread(receipt.SourceThreadTitle, receipt.SourceThreadID, computer)
}

// interruptDispatchedRequest stops the turn a request's message started, for
// a cancel that found nothing left to take off the queue. The send id names
// the message, and the row it produced names the turn: without that the
// cancel would report that there was nothing to stop while the turn it was
// asked to stop ran on.
func (a *App) interruptDispatchedRequest(ctx context.Context, threadID, sendID string, stop turnStop) (string, error) {
	record, found, err := a.findRecordedSend(threadID, sendID)
	if err != nil {
		return "", err
	}
	if !found || !record.dispatched {
		// The message was never sent, or it was restored into the composer.
		return threadtools.EffectNothing, nil
	}
	// A message written into a turn that was already running did not start
	// it, and the provider may still hold the message for a turn of its own:
	// that turn is someone else's, and a token never interrupts it.
	turn, turnFound, err := a.store.GetTurnByThreadIndex(threadID, record.item.TurnIndex)
	if err != nil {
		return "", err
	}
	if turnFound && turn.StartedAt < record.item.CreatedAt {
		return threadtools.EffectLeftUnread, nil
	}
	return a.interruptRequestTurn(ctx, threadID, record.item.TurnIndex, stop)
}

// interruptRequestTurn interrupts one thread only while the named turn is the
// one it is running, and reports what happened in the cancel's vocabulary.
func (a *App) interruptRequestTurn(ctx context.Context, threadID string, turnIndex int, stop turnStop) (string, error) {
	interrupted, err := a.interruptTurnAtIndex(ctx, threadID, turnIndex, stop)
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
	asks, err := a.disarmThreadRequests(callerThreadID)
	if err != nil {
		return err
	}
	a.cancelOwnedAsks(ctx, callerThreadID, asks)
	return nil
}

// disarmThreadRequests ends the caller's parked calls, disarms its wakes and
// returns the asks it owns that are still open.
func (a *App) disarmThreadRequests(callerThreadID string) ([]store.ThreadRequest, error) {
	a.cancelThreadRequestWaits(callerThreadID)
	if _, err := a.store.SetThreadRequestsNotifyForCaller(callerThreadID, false); err != nil {
		return nil, err
	}
	rows, err := a.store.ListThreadRequestsByCaller(callerThreadID, threadCancelLineagePage, 0)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rows, func(row store.ThreadRequest) bool {
		return threadRequestSettled(row) || row.Kind != store.ThreadRequestAsk
	}), nil
}

// cancelOwnedAsks stops exactly the asks named, best effort by design: the
// caller's own operation is what is being served, and a scratch thread left
// running is swept at the next boot, here or on the computer that holds it.
func (a *App) cancelOwnedAsks(ctx context.Context, callerThreadID string, asks []store.ThreadRequest) {
	for _, row := range asks {
		if err := a.cancelOwnedAsk(ctx, row); err != nil {
			log.Printf("thread tools: cancel ask %s for %s: %v", row.Token, callerThreadID, err)
		}
	}
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
	return a.collectThreadPeerReply(row.Token, row.TargetComputerID, *reply.Request)
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
	a.cancelAgentWakesForThread(threadID, "cancelled: the thread was deleted")
	a.settleReceiptsForThread(threadID, store.ThreadReceiptErrored, "The thread was deleted before it answered.")
	return a.cancelThreadRequests(ctx, threadID)
}

// stopThreadRequestsForMovedThread is the same for a thread this computer is
// handing to another. The receipts stay here and the caller collects them as
// usual; nothing is re-addressed, because the request was made of a thread on
// this computer and that thread is gone from it.
func (a *App) stopThreadRequestsForMovedThread(ctx context.Context, threadID, destination string) error {
	reason := threadtools.InterruptMoved(destination)
	// Loops and watches are this computer's clock and processes; they do
	// not travel with the conversation.
	a.cancelAgentWakesForThread(threadID, "cancelled: the thread was moved to another computer")
	withdrawErr := a.withdrawCancelledAgentWakes(threadID)
	a.settleReceiptsForThread(threadID, store.ThreadReceiptInterrupted, reason)
	return errors.Join(withdrawErr, a.cancelThreadRequests(ctx, threadID))
}

// threadRequestDestinationName names the computer a moved thread went to, for
// the settlement text its open receipts carry. An unknown backend leaves the
// reason unqualified rather than naming an id nobody reads.
func (a *App) threadRequestDestinationName(backendID string) string {
	name, _ := a.threadTools().threadToolsBackendName(backendID)
	return name
}
