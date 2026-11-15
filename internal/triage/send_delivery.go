package triage

import "log"

// SendDelivery names the turn a provider delivered one of this app's queued
// sends into: the turn whose end is the end of the work the message asked
// for.
//
// Handing a message to the provider does not decide that turn. Claude can
// drain a message written mid-turn into the running turn, or hold it and run
// it as a turn of its own once the running one ends (claude-wire.md
// §Queued-message consumption), and the two read the same at the stdin
// write. Delivery is what tells them apart.
type SendDelivery struct {
	// UserItemID is the AO row the send wrote.
	UserItemID string
	// SendIDs are the send ids the row carries, every member's for a
	// joined message: one delivery answers all of them.
	SendIDs []string
	// Members is the send id of every message the row carries, in order,
	// "" for one sent without (a person's): what the turn took in.
	Members []string
	// TurnIndex is the logical turn the message was delivered into.
	TurnIndex int
	// OwnTurn reports that the turn opened for this message, or for the
	// joined message carrying it. False when the message was drained into
	// a turn already running, whoever started it, as a Codex steer always
	// is: that turn is not the message's to stop.
	OwnTurn bool
	// AwaitingTurn reports that the provider has taken the message in (its
	// `started` or its confirmation arrived) before the turn that reads it
	// opened. TurnIndex and OwnTurn mean nothing yet: the delivery is
	// reported again, with its turn, when that turn opens.
	AwaitingTurn bool
	// Rebind reports a send whose delivery was already reported into turn
	// ReboundFrom, which a merge fold has since emptied: the provider ran
	// the message as part of a later one, in TurnIndex. The binding moves.
	Rebind      bool
	ReboundFrom int
	// Session is the token of the session whose frame reported the
	// delivery, empty for a host-originated one. A delivery waiting for its
	// turn is held with the process state of that session and dropped with
	// it (processBound), so a later release still names it.
	Session string

	// writtenDuring is the logical turn open when the send was written,
	// or -1 (pendingSend.WrittenDuringTurn).
	writtenDuring int
	// opensTurn is the turn a deferred send opens for itself, or -1. Its
	// delivery is never into an earlier turn (deliveryFloor).
	opensTurn int
	// tookTurn is the turn a turn start took the send as, known when the
	// delivery is read from the send's confirmation (tookTurnSet).
	tookTurn    int
	tookTurnSet bool
}

// deliveryFloor is the earliest turn a send can be delivered into. A
// deferred send was dispatched with no turn running and reserved its own
// turn index: its echo opens that turn (openQueuedEchoTurn) when no init
// does. A `started` for it can arrive while another message's turn is still
// open, as when the CLI drains a second deferred send mid-loop into the
// round of the first, and that turn is not this send's. A direct send heads
// its own turn and a steer is written into the turn it joins, so each has
// its row's turn as the floor. A message flushed into a running turn has no
// floor: it can be drained into it.
func deliveryFloor(send *pendingSend) int {
	if send.DeferredItem != nil || send.WasDeferred || send.Shape != sendShapeFlush {
		return send.TurnIndex
	}
	return -1
}

// SetSendDeliveredHook wires the app-layer callback told once per queued send
// which turn it was delivered into. It runs on the provider read loop, at
// times under the thread's flush anchor lock, so it must not take that lock
// or wait on the thread's event worker. Nil disables it.
func (r *Router) SetSendDeliveredHook(fn func(threadID string, delivery SendDelivery)) {
	r.mu.Lock()
	r.sendDelivered = fn
	r.mu.Unlock()
}

// noteSendStarted records Claude's command_lifecycle `started` for the send
// registered under providerItemID, which is that send's delivery: the
// message reached the model.
//
// The delivery turn is read off the logical turn, not the wire round the
// classification compares (classifyCommandDelivery). Drained mid-turn
// (`mid_turn`), the turn is the one still open. Run as a turn of its own
// (`new_turn`), the CLI writes `started` BEFORE that turn's system/init
// (claude-wire.md §command_lifecycle), so no turn is open yet and the
// delivery waits for the next turn start; when init came first, the turn
// it opened is the open one. A re-round inside one logical turn reads as
// `new_turn` to the round comparison but delivers into the same turn,
// which is why the turn and not the round is the binding.
func (r *Router) noteSendStarted(threadID, sessionToken, providerItemID string) {
	if providerItemID == "" {
		return
	}
	r.mu.Lock()
	var started []*pendingSend
	queue := r.pendingSendsLocked(threadID)
	for i := range queue {
		if queue[i].ExpectedProviderItemID == providerItemID {
			queue[i].Started = true
			started = append(started, &queue[i])
			break
		}
	}
	ready, waiting := r.placeDeliveriesLocked(threadID, sessionToken, started, false)
	r.mu.Unlock()
	r.reportDeliveries(threadID, ready)
	r.reportDeliveriesAwaitingTurn(threadID, waiting)
}

// reportSendsDelivered reports the delivery of sends a confirmation consumed
// (an echo, a transcript record, the lifecycle backstop, or a merge fold
// absorbing a member). A send whose `started` already reported is skipped,
// so the first delivery signal decides. Each send is marked so a send put
// back after a failed write does not report again.
//
// Caller holds the thread's flush anchor lock and not r.mu, and runs this
// after the confirmation's own writes: an echo that opens the send's turn
// (openQueuedEchoTurn) has opened it by then.
func (r *Router) reportSendsDelivered(threadID, sessionToken string, sends []*pendingSend) {
	r.mu.Lock()
	ready, waiting := r.placeDeliveriesLocked(threadID, sessionToken, sends, true)
	r.mu.Unlock()
	r.reportDeliveries(threadID, ready)
	r.reportDeliveriesAwaitingTurn(threadID, waiting)
}

// placeDeliveriesLocked marks each send's delivery reported and resolves its
// turn: a delivery is returned ready with the open turn when one is open and
// is not below its floor (deliveryFloor); otherwise it waits on the thread for
// the turn start that reaches its floor (releaseDeliveriesAtTurnStart) and is
// returned as waiting too.
//
// confirmed is a delivery read from the send's confirmation, which can
// arrive after the turn that ran the message has closed. With no turn open,
// a send the turn start that took it ran (ranInTakingTurn) is delivered
// there: the next turn to open belongs to someone else. A `started` says
// nothing of the kind: it precedes the turn it announces.
//
// A retired session's frame delivers nothing: its sends belong to a process
// that is gone. Caller holds r.mu.
func (r *Router) placeDeliveriesLocked(threadID, sessionToken string, sends []*pendingSend, confirmed bool) (ready, waiting []SendDelivery) {
	if r.frameRetiredLocked(threadID, sessionToken) {
		return nil, nil
	}
	var deliveries []SendDelivery
	for _, send := range sends {
		if send.DeliveryReported {
			continue
		}
		send.DeliveryReported = true
		ids, err := send.sendIDs()
		if err != nil {
			log.Printf("triage: delivered send %s/%s names no request: %v", threadID, send.AOItemID, err)
		}
		members := send.Members
		if len(members) == 0 {
			members = ids
		}
		if len(members) == 0 {
			// A send registered without its members is a person's.
			members = []string{""}
		}
		delivery := SendDelivery{
			UserItemID: send.AOItemID, SendIDs: ids, Members: members, Session: sessionToken,
			writtenDuring: send.WrittenDuringTurn, opensTurn: deliveryFloor(send),
		}
		deliveries = append(deliveries, delivery)
	}
	if len(deliveries) == 0 {
		return nil, nil
	}
	st := r.state(threadID)
	if confirmed {
		for i := range deliveries {
			if send := sendByItemID(sends, deliveries[i].UserItemID); send != nil && st.ranInTakingTurn(send) {
				deliveries[i].tookTurn, deliveries[i].tookTurnSet = send.TurnIndex, true
			}
		}
	}
	for _, delivery := range deliveries {
		switch {
		case st.openTurnSet && delivery.opensTurn <= st.openTurn:
			delivery.setTurn(st.openTurn)
			ready = append(ready, delivery)
		case !st.openTurnSet && delivery.tookTurnSet:
			delivery.setTurn(delivery.tookTurn)
			ready = append(ready, delivery)
		default:
			st.deliveriesAwaitingTurn = append(st.deliveriesAwaitingTurn, delivery)
			waiting = append(waiting, delivery)
		}
	}
	return ready, waiting
}

func sendByItemID(sends []*pendingSend, aoItemID string) *pendingSend {
	for _, send := range sends {
		if send.AOItemID == aoItemID {
			return send
		}
	}
	return nil
}

// ranInTakingTurn reports whether the turn start that took send (OpenedTurn)
// opened the turn that ran it. Claude announces a queued message it starts
// (`started`) before that turn's init, so a turn that took a send whose
// `queued` ack is on record and whose `started` had not arrived is one the
// provider opened on its own, racing the send (turn-lifecycle.md §Stdin
// race): the message runs later. A CLI that sends no acks leaves nothing to
// tell the two apart, and the turn that took the send is the evidence there
// is.
func (st *threadState) ranInTakingTurn(send *pendingSend) bool {
	if !send.OpenedTurn {
		return false
	}
	if send.StartedAtClaim {
		return true
	}
	_, acked := st.commandLifecycle[send.ExpectedProviderItemID]
	return !acked
}

// setTurn names the turn the message was delivered into. The turn is the
// message's own unless the message was written while it ran.
func (d *SendDelivery) setTurn(turnIndex int) {
	d.TurnIndex = turnIndex
	d.OwnTurn = turnIndex != d.writtenDuring
}

// releaseDeliveriesAtTurnStart reports the deliveries that were waiting for a
// turn, against the turn that just opened, except those whose floor is a
// later turn. Every path that opens a logical
// turn calls it: a turn start (handleTurnStart) and a queued echo that opens
// its message's turn with no init of its own (openQueuedEchoTurn).
func (r *Router) releaseDeliveriesAtTurnStart(threadID string, turnIndex int, sessionToken string) {
	r.mu.Lock()
	var ready []SendDelivery
	if !r.frameRetiredLocked(threadID, sessionToken) {
		if st := r.threadStateIfPresent(threadID); st != nil {
			waiting := st.deliveriesAwaitingTurn[:0]
			for _, delivery := range st.deliveriesAwaitingTurn {
				if delivery.opensTurn > turnIndex {
					waiting = append(waiting, delivery)
					continue
				}
				delivery.setTurn(turnIndex)
				ready = append(ready, delivery)
			}
			clear(st.deliveriesAwaitingTurn[len(waiting):])
			st.deliveriesAwaitingTurn = waiting
			if len(waiting) == 0 {
				st.deliveriesAwaitingTurn = nil
			}
		}
	}
	r.mu.Unlock()
	r.reportDeliveries(threadID, ready)
}

// reportReboundDeliveries tells the hook of deliveries a merge fold moved
// (SendDelivery.Rebind). A retired session's frame moves nothing.
func (r *Router) reportReboundDeliveries(threadID, sessionToken string, deliveries []SendDelivery) {
	if len(deliveries) == 0 {
		return
	}
	r.mu.Lock()
	hook := r.sendDelivered
	retired := r.frameRetiredLocked(threadID, sessionToken)
	r.mu.Unlock()
	if hook == nil || retired {
		return
	}
	for _, delivery := range deliveries {
		hook(threadID, delivery)
	}
}

// reportDeliveriesAwaitingTurn tells the hook which sends the provider took in
// before their turn opened. A session that ends in that interval read the
// message and never answered it, which is not the same as never reading it.
func (r *Router) reportDeliveriesAwaitingTurn(threadID string, deliveries []SendDelivery) {
	if len(deliveries) == 0 {
		return
	}
	r.mu.Lock()
	hook := r.sendDelivered
	r.mu.Unlock()
	if hook == nil {
		return
	}
	for _, delivery := range deliveries {
		delivery.AwaitingTurn = true
		hook(threadID, delivery)
	}
}

// reportDeliveries tells the hook of deliveries whose turn is named (setTurn).
func (r *Router) reportDeliveries(threadID string, deliveries []SendDelivery) {
	if len(deliveries) == 0 {
		return
	}
	r.mu.Lock()
	hook := r.sendDelivered
	r.mu.Unlock()
	if hook == nil {
		return
	}
	for _, delivery := range deliveries {
		hook(threadID, delivery)
	}
}
