package app

import (
	"context"
	"errors"
	"fmt"
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
		cause: threadtools.Cause{Code: threadtools.CauseTrayStop},
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
	return a.stopThreadRequestWorkBy(ctx, token, answer, func(receipt store.ThreadRequestReceipt) turnStop {
		return a.requestCancelStop(receipt, answer)
	})
}

// stopThreadRequestWorkBy is stopThreadRequestWorkFor with who stops the
// request's turn named by stopOf, for a stop of a whole thread that settles
// requests other threads made of it.
func (a *App) stopThreadRequestWorkBy(ctx context.Context, token, answer string, stopOf func(store.ThreadRequestReceipt) turnStop) (string, error) {
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
	stop := stopOf(receipt)
	switch {
	case slices.Contains(store.ThreadReceiptOpenStates(), state):
		settled, err := a.cancelOpenReceipt(token, answer, stop, &receipt)
		if err != nil {
			return "", err
		}
		if !settled {
			// A receipt something else settled first (a reply, a turn end,
			// an earlier cancel), before the read above or after it, has
			// nothing left to stop: its turn, still running or not, is no
			// longer this request's work, and the cancel that settled it
			// owns what it armed.
			return threadtools.EffectNothing, nil
		}
		state = receipt.State
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

	if state == store.ThreadReceiptAccepted {
		// The message was still on the queue when the receipt was read, so
		// the first thing to try is taking it back.
		removal, err := a.removeQueuedItem(ctx, receipt.TargetThreadID, threadRequestSendID(token))
		if err != nil || removal == queueRemovalRemoved {
			a.disarmRequestCancel(token)
		}
		if err != nil {
			return "", err
		}
		if removal == queueRemovalRemoved {
			return threadtools.EffectQueuedRemoved, nil
		}
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
	// No delivery bound it: the dispatch claimed it while the cancel was
	// running, it reached the provider before the receipt moved to
	// `running`, or the provider has it and no turn has read it yet.
	return a.cancelUndeliveredRequest(ctx, token, receipt.TargetThreadID, state, stop)
}

// cancelOpenReceipt settles an open receipt cancelled and, when no delivery
// has bound it, arms the cancel for the delivery still to come
// (armRequestCancel). The receipt it settled from replaces *receipt: read
// under the token's settle lock, under which every delivery binds, it names
// the turn a delivery bound, written or kept, and none binds after it. A
// cancel that does not settle the receipt arms and disarms nothing.
func (a *App) cancelOpenReceipt(token, answer string, stop turnStop, receipt *store.ThreadRequestReceipt) (bool, error) {
	var work threadSettlementWork
	settled, err := func() (bool, error) {
		unlock := a.threadRequestSettleLock(token)
		defer unlock()
		current, found, err := a.deliveredReceiptLocked(token)
		if err != nil || !found {
			return false, err
		}
		var settled bool
		// The sender's report says what happened, so a request of this
		// computer's own that its sender cancelled settles with its wake
		// disarmed, in the settlement's own write: a cancel that fails
		// leaves the wake armed for the work still running. A paired
		// computer's request disarms on its side once this cancel answers
		// it. A person's Stop from the tray leaves it armed, so the sender's
		// agent learns the stop was not its own.
		work, settled, err = a.settleReadReceiptLocked(current, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
			State:      store.ThreadReceiptCancelled,
			Answer:     []byte(answer),
			AnswerKind: store.ThreadAnswerError,
			DisarmWake: answer == threadRequestSenderCancel,
		}, dropScratchThread)
		if err != nil || !settled {
			return false, err
		}
		if current.TurnID == "" {
			a.armRequestCancel(token, current.TargetThreadID, stop)
		}
		*receipt = current
		return true, nil
	}()
	// The settle lock is released before this and the queue and provider
	// work the caller does: both take thread locks, and neither settles
	// anything.
	a.runThreadSettlementWork(work)
	return settled, err
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

// requestCancel is a cancel waiting on the delivery of its request's
// message: the message is with the provider and no delivery has said which
// turn reads it. The delivery decides (cancelOnDelivery): a turn that opened
// for the message is interrupted, and one it was drained into is left
// running.
type requestCancel struct {
	threadID string
	stop     turnStop
}

// armRequestCancel holds a cancel for token's delivery. The cancel's
// settlement arms it, under the settle lock every delivery binds under, so
// a delivery either bound the receipt first, and the cancel reads its turn,
// or finds the cancel armed. Caller holds the token's settle lock.
func (a *App) armRequestCancel(token, threadID string, stop turnStop) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if a.threadRequests.cancels == nil {
		a.threadRequests.cancels = make(map[string]requestCancel)
	}
	a.threadRequests.cancels[token] = requestCancel{threadID: threadID, stop: stop}
}

// disarmRequestCancel takes back token's held cancel, reporting it.
func (a *App) disarmRequestCancel(token string) (requestCancel, bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	c, ok := a.threadRequests.cancels[token]
	delete(a.threadRequests.cancels, token)
	return c, ok
}

// disarmRequestCancels drops the held cancels of a thread whose session
// ended: no delivery is coming, and a message given back is a new delivery
// with nothing to stop.
func (a *App) disarmRequestCancels(threadID string) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	for token, c := range a.threadRequests.cancels {
		if c.threadID == threadID {
			delete(a.threadRequests.cancels, token)
		}
	}
}

// cancelOnDelivery applies the cancel held for token to the delivery its
// receipt, settled cancelled, could not bind: a turn that opened for the
// message is interrupted, off the provider read loop this runs on. Caller
// holds the token's settle lock.
func (a *App) cancelOnDelivery(token, threadID string, turnIndex int, ownTurn bool) {
	c, ok := a.disarmRequestCancel(token)
	if !ok || !ownTurn || c.threadID != threadID {
		return
	}
	a.goThreadRequestSettling(func() {
		if _, err := a.interruptRequestTurn(a.lifeCtx(), threadID, turnIndex, c.stop); err != nil {
			log.Printf("thread tools: stop the turn cancelled request %s opened on %s: %v", token, threadID, err)
		}
	})
}

// cancelUndeliveredRequest is a cancel's effect on a request whose message
// no delivery has bound a turn for. A message never sent, or restored into
// the composer, has nothing to stop. A message written with no turn running
// reserved its own (PendingSendOpensTurn). Otherwise the provider has the
// message and has not said which turn reads it, and the turn running
// meanwhile is someone else's: the cancel stays armed for the delivery
// (cancelOnDelivery) and the request is left unread.
func (a *App) cancelUndeliveredRequest(ctx context.Context, token, threadID string, state string, stop turnStop) (string, error) {
	record, found, err := a.findRecordedSend(threadID, threadRequestSendID(token))
	if err != nil {
		a.disarmRequestCancel(token)
		return "", err
	}
	if !found || !record.dispatched {
		if state == store.ThreadReceiptAccepted {
			a.disarmRequestCancel(token)
			return threadtools.EffectNothing, nil
		}
		return threadtools.EffectLeftUnread, nil
	}
	// A message written with no turn running reserved its own turn, as the
	// send that binds it records (requestMessage): once open, that turn is
	// the request's. Before it opens, its delivery stops it.
	if a.triage != nil {
		if turnIndex, opens := a.triage.PendingSendOpensTurn(threadID, record.item.ID); opens && a.triage.OpenTurnIndex(threadID) == turnIndex {
			a.disarmRequestCancel(token)
			return a.interruptRequestTurn(ctx, threadID, turnIndex, stop)
		}
	}
	return threadtools.EffectLeftUnread, nil
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
//
// It returns, apart, the asks it could not stop (undelivered: each may keep
// running where it is) and a store failure that left the requests armed
// (err).
func (a *App) cancelThreadRequests(ctx context.Context, callerThreadID string) (undelivered, err error) {
	asks, err := a.disarmThreadRequests(callerThreadID)
	if err != nil {
		return nil, err
	}
	return a.cancelOwnedAsks(ctx, callerThreadID, asks), nil
}

// disarmThreadRequests ends the caller's parked calls, disarms its wakes and
// returns the asks it owns that are still open.
func (a *App) disarmThreadRequests(callerThreadID string) ([]store.ThreadRequest, error) {
	a.cancelThreadRequestWaits(callerThreadID)
	if _, err := a.store.DisarmThreadRequestWakesForCaller(callerThreadID); err != nil {
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

// cancelOwnedAsks stops exactly the asks named and returns the ones it could
// not stop. One failure does not stop the rest. A scratch thread left running
// is swept at the next boot, here or on the computer that holds it.
func (a *App) cancelOwnedAsks(ctx context.Context, callerThreadID string, asks []store.ThreadRequest) error {
	var errs []error
	for _, row := range asks {
		if err := a.cancelOwnedAsk(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("cancel ask %s for %s: %w", row.Token, callerThreadID, err))
		}
	}
	return errors.Join(errs...)
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
	// Under the pairing that admitted the ask alone; one another pairing
	// replaced is ended here instead, which settles the ask. Detached: the
	// caller can hold the thread's locks, which the end's wakes take.
	backendID, _ := a.backendIdentity()
	if err := a.backends.CallThreadPeerUnder(call, row.TargetComputerID, row.Pairing, "ThreadToolCall", &reply, ThreadPeerCall{
		Tool:  "thread_cancel",
		Token: row.Token,
		Source: threadtools.Caller{
			ThreadID:     row.CallerThreadID,
			ComputerID:   backendID,
			ComputerName: a.backendDisplayName(),
		},
	}); err != nil {
		if a.pairingGone(row.TargetComputerID, row.Pairing) {
			a.endPairingDetached(row.TargetComputerID, pairingEndedHere)
			return nil
		}
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
func (a *App) settleReceiptsForThread(threadID string, settlement store.ThreadRequestSettlement) {
	receipts, err := a.store.ListThreadRequestReceiptsForThread(threadID)
	if err != nil {
		log.Printf("thread tools: list receipts for %s: %v", threadID, err)
		return
	}
	works := make([]threadSettlementWork, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.State != store.ThreadReceiptAccepted && receipt.State != store.ThreadReceiptRunning {
			continue
		}
		// No scratch cleanup here. The delete and move ports own this
		// thread's destruction and are running inside it: deleting it again
		// from the settlement would wait for the thread action lock the
		// caller already holds, which is nobody's to release.
		work, _, err := a.settleThreadReceiptNoScratch(receipt.Token, store.ThreadReceiptOpenStates(), settlement)
		if err != nil {
			log.Printf("thread tools: settle receipt %s as %s: %v", receipt.Token, settlement.State, err)
			continue
		}
		works = append(works, work)
	}
	// Together, so a caller with several requests open against this thread
	// is told once that it went.
	a.runThreadSettlementWorksDetached(works)
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
func (a *App) stopThreadRequestsForDeletedThread(ctx context.Context, threadID string) (undelivered, err error) {
	// The delete cascades the rows away whether or not this cancel lands.
	if err := a.cancelAgentWakesForThread(threadID, "cancelled: the thread was deleted"); err != nil {
		log.Printf("agent wakes: %v", err)
	}
	a.settleReceiptsForThread(threadID, store.ThreadRequestSettlement{
		State:      store.ThreadReceiptErrored,
		Answer:     []byte("The thread was deleted before it answered."),
		AnswerKind: store.ThreadAnswerError,
	})
	return a.cancelThreadRequests(ctx, threadID)
}

// stopThreadRequestsForMovedThread is the same for a thread this computer is
// handing to another. The receipts stay here and the caller collects them as
// usual; nothing is re-addressed, because the request was made of a thread on
// this computer and that thread is gone from it.
func (a *App) stopThreadRequestsForMovedThread(ctx context.Context, threadID, destination string) (undelivered, err error) {
	// Loops and watches are this computer's clock and processes; they do
	// not travel with the conversation.
	if err := a.cancelAgentWakesForThread(threadID, "cancelled: the thread was moved to another computer"); err != nil {
		log.Printf("agent wakes: %v", err)
	}
	withdrawErr := a.withdrawCancelledAgentWakes(threadID)
	a.settleReceiptsForThread(threadID, interruptedSettlement(threadtools.Cause{Code: threadtools.CauseMoved, Arg: destination}, ""))
	undelivered, err = a.cancelThreadRequests(ctx, threadID)
	return undelivered, errors.Join(withdrawErr, err)
}

// threadRequestDestinationName names the computer a moved thread went to, for
// the settlement text its open receipts carry. An unknown backend leaves the
// reason unqualified rather than naming an id nobody reads.
func (a *App) threadRequestDestinationName(backendID string) string {
	name, _ := a.threadTools().threadToolsBackendName(backendID)
	return name
}
