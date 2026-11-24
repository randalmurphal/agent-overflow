package app

import (
	"context"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// A request's message on its way into its target thread: the dispatch, the
// receipt's move to running, its binding to the turn the provider delivered
// it into, and its return to the queue when the provider never took it in.

// dispatchRequest sends one request's message into its target thread, as the
// user's own message with the request's footer appended.
//
// The send is composer-shaped on purpose: a busy thread queues the message at
// its turn boundary instead of interrupting it, and an idle one starts a turn
// now. The receipt moves to `running` when the message reaches the provider.
// On the idle path the message opens its own turn, which binds the receipt at
// once. On the queued path the provider still decides the turn: Claude can
// drain the message into the running turn or hold it for a turn of its own,
// so the receipt is bound only when triage reports the delivery
// (bindDeliveredRequests), and a message merged into another queued send is
// bound to the turn they ran in together.
func (t threadToolsApp) dispatchRequest(
	ctx context.Context, origin threadRequestOrigin, token, kind, targetThreadID, text string, wantsAnswer bool,
) error {
	// The message drives the target thread in the mode that thread already
	// runs in, which is what SendMessageWithOptions judges for the composer.
	// A forwarded request arrives on a paired computer's session, so the
	// same gate has to stand here or thread_send would be the way around it.
	if err := t.app.requireAutonomyForThread(ctx, targetThreadID, ""); err != nil {
		return err
	}
	message, err := t.requestMessage(origin, token, kind, targetThreadID, text, wantsAnswer)
	if err != nil {
		return err
	}
	attribution := message.Attribution()
	sendID := threadRequestSendID(token)
	// writtenTo is the session an idle thread's send wrote the message to.
	// A repeated frame writes nothing and leaves it empty: the first frame
	// bound the receipt.
	var writtenTo string
	item, err := t.app.sendMessageWithOptions(ctx, targetThreadID, message.Text(), sendMessageOptions{
		SendID:    sendID,
		Admission: admitQueueing,
		// The target's composer holds whatever the person at that thread
		// typed and has not sent. An agent's message must never take it.
		PreserveDraft:     true,
		Origin:            attribution.Origin,
		OriginThread:      attribution.Thread,
		onDurable:         func(session string) { t.requestDispatched(token, targetThreadID, session) },
		onSessionDispatch: func(session string) { writtenTo = session },
	})
	if err != nil {
		return err
	}
	if item.ID != "" && writtenTo != "" {
		// Sent to an idle thread, the message opened its own turn.
		t.app.bindRequestDelivery(token, targetThreadID, writtenTo, item.ID, item.TurnIndex, true)
	}
	return nil
}

// requestDispatched moves a queued request's receipt to `running` once its
// message is durable and with the provider (markRequestDispatched). session
// is the session the message was written to.
func (t threadToolsApp) requestDispatched(token, targetThreadID, session string) {
	record, found, err := t.app.findRecordedSend(targetThreadID, threadRequestSendID(token))
	if err != nil {
		log.Printf("thread tools: resolve dispatched request %s: %v", token, err)
		return
	}
	if !found || !record.dispatched {
		// The message did not reach the provider: it was restored instead of
		// being sent (a session death, a restart). Nothing ran, so the
		// receipt stays `accepted`.
		return
	}
	t.markRequestDispatched(token, targetThreadID, session, record.item.ID)
}

// returnRequestNotTakenIn takes a request whose message goes back before the
// provider took it in (a session end, a teardown, a handoff's move) back to
// `accepted`. Nothing answered it, so its sender is told nothing; the
// request is owed again and requeueRequest puts its message back on the
// queue. It reports true for a request still `accepted` (its message never
// left the queue) or `running` on threadID with no turn and not taken in by
// the provider, now `accepted`, and false for any other, which the session
// end settles. It takes the token's settle lock, so a turn end reading the
// receipt sees it running or accepted, never in between.
func (t threadToolsApp) returnRequestNotTakenIn(threadID, token string) bool {
	unlock := t.app.threadRequestSettleLock(token)
	defer unlock()
	receipt, found, err := t.app.deliveredReceiptLocked(token)
	if err != nil {
		log.Printf("thread tools: return request %s: %v", token, err)
		return false
	}
	if !found || receipt.TargetThreadID != threadID {
		return false
	}
	switch {
	case receipt.State == store.ThreadReceiptAccepted:
		return true
	case receipt.State == store.ThreadReceiptRunning && receipt.TurnID == "" && !t.app.receiptTakenIn(threadID, token):
		return t.returnRequestLocked(token)
	}
	return false
}

// returnRequestLocked moves a receipt and its request back to `accepted`.
// The caller holds the token's settle lock.
func (t threadToolsApp) returnRequestLocked(token string) bool {
	returned, err := t.app.store.ReturnThreadReceipt(token)
	if err != nil {
		log.Printf("thread tools: return request %s: %v", token, err)
		return false
	}
	if !returned {
		return false
	}
	// A request another computer made has no row here; its caller reads
	// the receipt.
	if _, err := t.app.store.AdvanceThreadRequestState(token, store.ThreadRequestRunning, store.ThreadRequestAccepted); err != nil {
		log.Printf("thread tools: return request %s to accepted: %v", token, err)
	}
	return true
}

// requeueRequest puts a returned request's message (body, footer included)
// back on its target's queue, attributed as dispatchRequest attributed it:
// the sender its receipt names, and the facts the message was written with.
// It is held: the session end or teardown that gave it back does not start
// a turn for it, and the thread's next turn boundary or session start sends
// it. An archived thread refuses it, except one New Thread, Same Assistant
// superseded: its request moves to the assistant's current thread, receipt
// and all, and is queued there.
func (t threadToolsApp) requeueRequest(token, targetThreadID string, message usermessage.AgentMessage) error {
	if current, ok := t.app.assistantCurrentThread(targetThreadID); ok {
		moved, err := t.app.store.RetargetThreadReceipt(token, targetThreadID, current)
		if err != nil {
			return fmt.Errorf("requeue request %s on the assistant's current thread: %w", token, err)
		}
		if moved {
			targetThreadID = current
		}
	}
	receipt, found, err := t.app.store.GetThreadRequestReceipt(token)
	if err != nil {
		return fmt.Errorf("requeue request %s: %w", token, err)
	}
	if !found || receipt.State != store.ThreadReceiptAccepted {
		return nil
	}
	origin := &agentmeta.OriginThread{
		ComputerID:   receipt.SourceComputerID,
		ComputerName: receipt.SourceComputerName,
		ThreadID:     receipt.SourceThreadID,
		Title:        receipt.SourceThreadTitle,
		Token:        token,
		Kind:         receipt.Kind,
	}
	if written := message.Attribution().Thread; written != nil {
		origin.Facts = written.Facts
	}
	_, err = t.app.registerQueueItem(targetThreadID, message.Text(), SendMessageOptions{SendID: threadRequestSendID(token)}, injectedQueueOptions{
		preserveDraft:     true,
		held:              true,
		appWritten:        true,
		heldWhileArchived: true,
		origin:            agentmeta.OriginAgentThread,
		originThread:      origin,
		onDurable:         func(session string) { t.requestDispatched(token, targetThreadID, session) },
	})
	if err != nil {
		return fmt.Errorf("requeue request %s: %w", token, err)
	}
	return nil
}

// markRequestDispatched moves the receipt to `running` once its queued
// message is with the provider, with no turn bound: the provider has not
// said yet which turn will read it, and no turn end settles the receipt
// until bindRequestDelivery names one. A reply in the meantime still
// settles it.
//
// The observer's gate is set under the token's settle lock BEFORE the row,
// as bindRequestDelivery does, so the end of session, the one the message
// was written to (settleReceiptsForEndedSession), sees this receipt. A
// dispatch the gate no longer names writes nothing (receiptGate).
func (t threadToolsApp) markRequestDispatched(token, targetThreadID, session, messageItemID string) {
	unlock := t.app.threadRequestSettleLock(token)
	defer unlock()
	if !t.app.noteReceiptRunning(targetThreadID, token, session) {
		return
	}
	// The store moves a source row here to `running` in the same write.
	if _, err := t.app.store.MarkThreadReceiptDispatched(token, messageItemID); err != nil {
		log.Printf("thread tools: mark receipt %s dispatched: %v", token, err)
	}
}

// bindDeliveredRequests binds every request a delivered send carries to the
// turn the provider delivered it into. A joined message carries several, and
// they share the turn. It is triage's send-delivered hook, so it runs on the
// provider read loop. A send taken in before its turn opened binds nothing
// yet; its delivery is reported again when that turn opens.
func (a *App) bindDeliveredRequests(threadID string, delivery triage.SendDelivery) {
	for _, sendID := range delivery.SendIDs {
		token, ok := strings.CutPrefix(sendID, threadRequestSendIDPrefix)
		if !ok || token == "" {
			continue
		}
		if delivery.AwaitingTurn {
			a.noteReceiptTakenIn(threadID, token, delivery.Session)
			continue
		}
		if delivery.Rebind {
			a.rebindRequestDelivery(token, threadID, delivery.Session, delivery.UserItemID, delivery.ReboundFrom, delivery.TurnIndex, delivery.OwnTurn)
			continue
		}
		a.bindRequestDelivery(token, threadID, delivery.Session, delivery.UserItemID, delivery.TurnIndex, delivery.OwnTurn)
	}
}

// rebindRequestDelivery moves the receipt from a turn a merge fold emptied to
// the turn that ran its message (triage.SendDelivery.Rebind). The store moves
// it only from a turn that never ran, so a receipt bound anywhere else is left
// alone, and so is one the gate names another session for (receiptGate). It
// takes the settle lock as bindRequestDelivery does.
func (a *App) rebindRequestDelivery(token, targetThreadID, session, messageItemID string, fromIndex, turnIndex int, ownTurn bool) {
	t := a.threadTools()
	var work threadSettlementWork
	func() {
		unlock := a.threadRequestSettleLock(token)
		defer unlock()
		if !a.noteReceiptRunning(targetThreadID, token, session) {
			return
		}
		moved, err := a.writeReceiptBindingLocked(token, receiptBinding{
			threadID: targetThreadID, itemID: messageItemID, turnIndex: turnIndex, ownTurn: ownTurn, rebindFrom: fromIndex,
		})
		if err != nil {
			log.Printf("thread tools: rebind receipt %s to its turn, kept to try again: %v", token, err)
			return
		}
		if moved {
			work = t.settleIfTurnAlreadyEnded(token, targetThreadID, turnIndex)
		}
	}()
	a.runThreadSettlementWorkDetached(work)
}

// bindRequestDelivery binds the receipt to the turn its message was delivered
// into, which is the only turn whose end settles it, and records whether that
// turn opened for the message (ownTurn), which is the only turn a cancel may
// interrupt. It is the one place the
// binding is written, and the first delivery wins (BindThreadReceiptTurn).
//
// It runs under the token's settle lock, so the observer's gate and the row
// transition are one step as far as a turn end is concerned: without that the
// observer could read the receipt as unbound, decide there is nothing to
// watch, and drop the gate the write is about to need. A turn that finished
// before the binding landed is settled here, because by then no turn end is
// coming. session is the session that delivered it; a delivery from one the
// gate no longer names binds nothing (receiptGate).
func (a *App) bindRequestDelivery(token, targetThreadID, session, messageItemID string, turnIndex int, ownTurn bool) {
	t := a.threadTools()
	var work threadSettlementWork
	func() {
		unlock := a.threadRequestSettleLock(token)
		defer unlock()
		// The observer's gate is set BEFORE the row, so a turn that ends
		// between the two still reaches this token. The gate only says
		// "look"; the store row says whether there is anything to settle.
		if !a.noteReceiptRunning(targetThreadID, token, session) {
			return
		}
		// The store moves a source row here to `running` in the same write,
		// bound or not.
		bound, err := a.writeReceiptBindingLocked(token, receiptBinding{
			threadID: targetThreadID, itemID: messageItemID, turnIndex: turnIndex, ownTurn: ownTurn, rebindFrom: -1,
		})
		if err != nil {
			// The delivery is kept (writeReceiptBindingLocked), and the turn's
			// end or the session's writes it again.
			log.Printf("thread tools: bind receipt %s to its turn, kept to try again: %v", token, err)
			return
		}
		if !bound {
			// Bound by an earlier delivery, or cancelled or settled while the
			// message was on its way. Whoever did that owns it; a cancel
			// waiting on this delivery stops the turn if it is the message's.
			a.cancelOnDelivery(token, targetThreadID, turnIndex, ownTurn)
			return
		}
		work = t.settleIfTurnAlreadyEnded(token, targetThreadID, turnIndex)
	}()
	// This runs inside the target thread's own send or on its provider read
	// loop, which hold that thread's locks. The deferred work takes thread
	// locks of its own, so it never runs on this goroutine.
	a.runThreadSettlementWorkDetached(work)
}

// settleIfTurnAlreadyEnded settles a receipt whose turn completed before the
// receipt was observable as running. The observer's gate was unset when that
// turn ended, so nothing else will ever look at it again.
//
// The caller holds the token's settle lock.
func (t threadToolsApp) settleIfTurnAlreadyEnded(token, targetThreadID string, turnIndex int) threadSettlementWork {
	if t.app.triage != nil && t.app.triage.OpenTurnIndex(targetThreadID) == turnIndex {
		// The turn is running, whatever completion its row still carries.
		return threadSettlementWork{}
	}
	turn, found, err := t.app.store.GetTurnByThreadIndex(targetThreadID, turnIndex)
	if err != nil {
		log.Printf("thread tools: read turn %d of %s for request %s: %v", turnIndex, targetThreadID, token, err)
		return threadSettlementWork{}
	}
	if !found || turn.CompletedAt == nil {
		return threadSettlementWork{}
	}
	settlement, err := t.app.turnSettlement(targetThreadID, turn)
	if err != nil {
		log.Printf("thread tools: read the answer of request %s: %v", token, err)
		return threadSettlementWork{}
	}
	work, _, err := t.app.settleThreadReceiptLocked(token, []string{store.ThreadReceiptRunning}, settlement, dropScratchThread)
	if err != nil {
		log.Printf("thread tools: settle request %s on a finished turn: %v", token, err)
		return threadSettlementWork{}
	}
	return work
}
