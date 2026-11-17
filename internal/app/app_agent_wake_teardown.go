package app

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// agentPartsNotTakenIn finds what the app-written messages an unconfirmed
// item carries are owed when the provider did not take it in, its session
// having ended or refused it (reason, a queue_restored reason): loop and
// watch wakes, outside `thread send` messages, workers' prompts, and
// requests' answers and reminders and remote jobs' completions. It returns
// what of the item goes back to the composer, false when nothing does, and
// those outcomes, which the caller applies (settleNotTakenIn) once the
// restore of the rest has committed: a restore that fails requeues the items
// whole, these included, and they are still queued.
//
// Like a wake queued at quit, none of them goes to the composer; unlike one,
// a wake does not replay: the session that ended may end again, and a replay
// would start it on every fire. A wake's row records `not delivered` and
// keeps its schedule, and a watch keeps the wake's own text. An outside send
// is thrown away with a notice, as at boot, and a worker's prompt goes back
// to the user in the worker. An answer, a reminder or a completion goes back
// to its request or watch, as a Stop's does. Another thread's request goes
// back to its queue, owed again, when its session ended (giveBackRequest);
// when the provider refused it with its session alive, it settles, its text
// going nowhere. When a person's message was joined with them, the person's
// text alone goes back to the composer.
func (a *App) agentPartsNotTakenIn(threadID string, item triage.UnconfirmedFlushItem, part composerdraft.Part, reason string) (composerdraft.Part, bool, notTakenIn) {
	wakeOutcome, sendWhy := agentWakeOutcomeSessionEnded, "the session ended before the agent took it in"
	if reason != queueRestoredReasonSessionDied && reason != queueRestoredReasonNotDelivered {
		wakeOutcome, sendWhy = agentWakeOutcomeNotDelivered, "the provider did not take it in"
	}
	sessionEnded := reason == queueRestoredReasonSessionDied || item.Discarded
	members := unconfirmedMembersOf(item)
	ids := members.ids
	if !slices.ContainsFunc(ids, appWrittenSendID) {
		return part, true, notTakenIn{}
	}
	var owed notTakenIn
	for _, id := range ids {
		if !ownedSendID(id) {
			continue
		}
		// A message sent alone is the item's queue row, still there if it
		// never left the queue. The members of a joined one lost theirs at
		// the provider write, which handed them over unconfirmed.
		rowID := ""
		if !members.joined {
			rowID = item.QueueItemID
		}
		owed.owned = append(owed.owned, ownedMessage{sendID: id, rowID: rowID, unconfirmed: true, ended: true})
	}
	messages, person, split := splitSessionEndParts(members, part.Content)
	if !split {
		// The text does not hold a part for every app message the item
		// names, so none of it can be told apart from the person's: all of
		// it goes back to the composer, an outside send's with it. Its wakes
		// are still recorded, its prompts handed back and its answers and
		// completions returned. An outside send leaves no notice: its text
		// is in front of the person, and a notice would invite a duplicate.
		log.Printf("agent wakes: a message of %s does not split into the app messages it names; restoring all of it", threadID)
		for _, id := range ids {
			if agentAppPartRoot(id) != "" && !strings.HasPrefix(id, agentSendIDRoot) {
				owed.messages = append(owed.messages, appMessageOutcome{sendID: id, wakeOutcome: wakeOutcome})
			}
			// A request's text is in the composer now; queued again, it
			// would be sent twice.
			if token := threadRequestSendIDToken(id); token != "" {
				owed.unread = append(owed.unread, unreadRequest{token: token, why: threadtools.InterruptNotTakenInToComposer})
			}
		}
		return part, true, owed
	}
	for _, id := range ids {
		if agentAppPartRoot(id) != "" {
			owed.messages = append(owed.messages, appMessageOutcome{sendID: id, wakeOutcome: wakeOutcome, message: messages[id], sendWhy: sendWhy})
		}
		if token := threadRequestSendIDToken(id); token != "" {
			if sessionEnded {
				owed.requests = append(owed.requests, returnedRequest{token: token, message: messages[id]})
			} else {
				owed.unread = append(owed.unread, unreadRequest{token: token, why: threadtools.InterruptNotTakenIn})
			}
		}
	}
	if person != nil {
		part.Content = *person
		return part, true, owed
	}
	// The composer restore that would have retired the durable row takes
	// none of it.
	owed.rowID = item.QueueItemID
	return composerdraft.Part{}, false, owed
}

// teardownRequests is what a teardown owes other threads' requests whose
// messages it drops before the agent took them in: their receipts are back
// in `accepted` before the drop, so the session's end settles none of them,
// and once the session is closed their messages go back on the queue. On a
// thread New Thread, Same Assistant superseded, the loop and watch wakes it
// drops are owed too, and go on to the assistant's current thread.
type teardownRequests struct {
	// queued are requests whose messages never left the queue.
	queued []returnedRequest
	// wakes are loop and watch wakes whose messages never left the queue.
	wakes []returnedWake
	// sent are messages the provider was handed and never took in, wholly
	// these requests' and wakes', whose timeline rows go before they are
	// queued again.
	sent []teardownSend
}

func (owed teardownRequests) empty() bool {
	return len(owed.queued) == 0 && len(owed.wakes) == 0 && len(owed.sent) == 0
}

// tookSend reports whether item is one of the sent messages the teardown
// took back (sent), by the queue row it was dispatched from or, for one with
// none, its timeline row.
func (owed teardownRequests) tookSend(item triage.UnconfirmedFlushItem) bool {
	userItemID := item.UserItemID
	if item.QuietItem != nil {
		userItemID = item.QuietItem.ID
	}
	return slices.ContainsFunc(owed.sent, func(send teardownSend) bool {
		if item.QueueItemID != "" {
			return send.queueItemID == item.QueueItemID
		}
		return userItemID != "" && send.userItemID == userItemID
	})
}

type teardownSend struct {
	userItemID  string
	queueItemID string
	requests    []returnedRequest
	wakes       []returnedWake
}

// returnedWake is a loop or watch wake a handoff's teardown took back.
type returnedWake struct {
	sendID  string
	message usermessage.AgentMessage
}

// returnRequestsAtTeardown takes back the requests a teardown is about to
// drop, and on a thread a handoff superseded the loop and watch wakes:
// those still queued, and those in a message the provider was handed and
// has not taken in that holds nothing else. A message joined with a
// person's text is the thread's history and stays as it is; the session's
// end settles its requests. Caller holds the thread's mutation lock.
func (a *App) returnRequestsAtTeardown(threadID string) teardownRequests {
	var owed teardownRequests
	tools := a.threadTools()
	_, superseded := a.assistantCurrentThread(threadID)
	movedWake := func(sendID string) bool {
		_, _, ok := agentWakeFromSendID(sendID)
		return superseded && ok
	}
	rows, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		logThreadRequestSweep("list the queued requests of "+threadID, err)
	}
	for _, row := range rows {
		if token := threadRequestSendIDToken(row.SendID); token != "" && tools.returnRequestNotTakenIn(threadID, token) {
			owed.queued = append(owed.queued, returnedRequest{token: token, message: queuedAgentMessage(row)})
		} else if movedWake(row.SendID) {
			// Its row goes now, so the drop does not record the wake as
			// cleared (noteAgentWakesDropped).
			if err := a.store.DeleteFlushQueueItem(row.ID); err != nil {
				logThreadRequestSweep("take back the queued wake "+row.SendID, err)
				continue
			}
			owed.wakes = append(owed.wakes, returnedWake{sendID: row.SendID, message: queuedAgentMessage(row)})
		}
	}
	for _, item := range a.triage.UnconfirmedFlushSends(threadID) {
		members := unconfirmedMembersOf(item)
		ids := members.ids
		if len(ids) == 0 || slices.ContainsFunc(ids, func(id string) bool { return threadRequestSendIDToken(id) == "" && !movedWake(id) }) {
			continue
		}
		messages, person, split := splitSessionEndParts(members, a.restoredFlushDraftPart(threadID, item).Content)
		if !split || person != nil {
			continue
		}
		send := teardownSend{userItemID: item.UserItemID, queueItemID: item.QueueItemID}
		if item.QuietItem != nil {
			send.userItemID = item.QuietItem.ID
		}
		for _, id := range ids {
			if movedWake(id) {
				send.wakes = append(send.wakes, returnedWake{sendID: id, message: messages[id]})
				continue
			}
			token := threadRequestSendIDToken(id)
			if tools.returnRequestNotTakenIn(threadID, token) {
				send.requests = append(send.requests, returnedRequest{token: token, message: messages[id]})
			}
		}
		if len(send.requests) > 0 || len(send.wakes) > 0 {
			owed.sent = append(owed.sent, send)
		}
	}
	return owed
}

// giveBackTeardownRequests queues again what returnRequestsAtTeardown took
// back, once the teardown has emptied the queue. A message the provider was
// handed loses its timeline row first, or the queue would read the request
// as sent already. A request that cannot be queued again (its row stays,
// the thread is archived, the write fails) settles as never read.
func (a *App) giveBackTeardownRequests(threadID string, owed teardownRequests) {
	requests, wakes := owed.queued, owed.wakes
	var userItemIDs, queueItemIDs []string
	// The requests that settle run their work together, so a caller with
	// several of them is told once.
	var settled []threadSettlementWork
	for _, send := range owed.sent {
		if send.userItemID != "" {
			if _, err := a.cleanupStaleFlushRow(threadID, send.userItemID); err != nil {
				log.Printf("thread tools: remove the row of %s's dropped request message %s: %v", threadID, send.userItemID, err)
				for _, r := range send.requests {
					settled = append(settled, a.settleReceiptNotRead(threadID, r.token, store.ThreadReceiptAccepted, threadtools.InterruptSessionEndedUnread))
				}
				for _, w := range send.wakes {
					a.noteMovedAgentWakeUndelivered(threadID, w)
				}
				continue
			}
			userItemIDs = append(userItemIDs, send.userItemID)
		}
		if send.queueItemID != "" {
			queueItemIDs = append(queueItemIDs, send.queueItemID)
		}
		requests = append(requests, send.requests...)
		wakes = append(wakes, send.wakes...)
	}
	for _, w := range wakes {
		a.redeliverMovedAgentWake(threadID, w)
	}
	if len(userItemIDs)+len(queueItemIDs) > 0 {
		a.emit(eventchan.ProviderQueueRestored, QueueRestoredEvent{
			ThreadID:     threadID,
			Reason:       queueRestoredReasonRequestRequeued,
			QueueItemIDs: queueItemIDs,
			UserItemIDs:  userItemIDs,
		})
	}
	requeued := false
	for _, r := range requests {
		if err := a.threadTools().requeueRequest(r.token, threadID, r.message); err != nil {
			log.Printf("thread tools: give request %s back to %s after its session's end: %v", r.token, threadID, err)
			settled = append(settled, a.settleReceiptNotRead(threadID, r.token, store.ThreadReceiptAccepted, threadtools.InterruptSessionEndedUnread))
			continue
		}
		requeued = true
	}
	a.runThreadSettlementWorksDetached(settled)
	// The torn-down session left the registry before the teardown, so a
	// live one started after it: while the idle reaper held the lock this
	// waited on, its start flushed the queue before these were back on it.
	// Idle, it has no turn boundary coming to send them. A turn in flight
	// sends them at its end.
	if requeued && a.triage != nil {
		if _, live := a.sessionManager().get(threadID); live && !a.threadTurnInFlight(threadID) {
			a.triage.FlushQueuedItems(threadID)
		}
	}
}

// notTakenIn is what the app-written messages of unconfirmed items are owed
// when the provider did not take them in (agentPartsNotTakenIn), held until
// the restore of the rest commits.
type notTakenIn struct {
	// messages are the loop and watch wakes, outside sends and workers'
	// prompts, by send id, in order.
	messages []appMessageOutcome
	// owned are the answers, reminders and completions, for their owners.
	owned []ownedMessage
	// requests are other threads' requests that go back to their queue:
	// their session ended before the agent took them in.
	requests []returnedRequest
	// unread are other threads' requests the provider refused with the
	// session alive, or whose text went back to the composer, which settle.
	unread []unreadRequest
	// rowID is the queue row of an item wholly the app's, which no composer
	// restore retires.
	rowID string
}

// returnedRequest is another thread's request whose message goes back to its
// queue: by its token, with its message as it was written.
type returnedRequest struct {
	token   string
	message usermessage.AgentMessage
}

// unreadRequest is another thread's request a restore settles, with why.
type unreadRequest struct {
	token string
	why   string
}

// appMessageOutcome is one app-written message the agent did not take in: a
// loop or watch wake's row records wakeOutcome and keeps its text, an outside
// send leaves its thread a notice saying sendWhy, and a worker's prompt goes
// back to the user in the worker.
type appMessageOutcome struct {
	sendID      string
	wakeOutcome string
	message     usermessage.AgentMessage
	sendWhy     string
}

// settleNotTakenIn applies what agentPartsNotTakenIn found, for every item of
// one restore, once that restore has committed.
func (a *App) settleNotTakenIn(threadID string, owed []notTakenIn) {
	var owned []ownedMessage
	var settled []threadSettlementWork
	for _, item := range owed {
		for _, m := range item.messages {
			switch {
			case a.handBackWorkerPromptWake(m.sendID):
			case strings.HasPrefix(m.sendID, agentSendIDRoot):
				a.outsideSendNotTakenIn(threadID, m.message, m.sendWhy)
			case strings.HasPrefix(m.sendID, assistantSendIDRoot):
				a.requeueAssistantMessage(threadID, m.sendID, m.message.Text())
			default:
				if wakeID, _, ok := agentWakeFromSendID(m.sendID); ok {
					a.noteAgentWakeUndelivered(threadID, wakeID, m.wakeOutcome, m.message.Text())
				}
			}
		}
		if item.rowID != "" {
			if err := a.store.DeleteFlushQueueItem(item.rowID); err != nil {
				logThreadRequestSweep("delete the queued wake "+item.rowID, err)
			}
		}
		owned = append(owned, item.owned...)
		for _, r := range item.requests {
			settled = append(settled, a.giveBackRequestWork(threadID, r.token, r.message))
		}
		for _, r := range item.unread {
			settled = append(settled, a.settleReceiptNotRead(threadID, r.token, store.ThreadReceiptRunning, r.why))
		}
	}
	// Together, so a caller with several requests in this restore is told
	// once.
	a.runThreadSettlementWorksDetached(settled)
	if len(owned) > 0 {
		a.giveBackToOwners(threadID, owned)
	}
}

// appWrittenSendID reports a message the app wrote for a thread rather than
// a person: the app parts agentAppPartRoot names, the answers, reminders
// and completions that belong to a request or a watch, and other threads'
// requests.
func appWrittenSendID(sendID string) bool {
	return agentAppPartRoot(sendID) != "" || ownedSendID(sendID) || threadRequestSendIDToken(sendID) != ""
}

// threadRequestSendIDToken is the token of a request's send id, or empty.
func threadRequestSendIDToken(sendID string) string {
	token, _ := strings.CutPrefix(sendID, threadRequestSendIDPrefix)
	if token == sendID {
		return ""
	}
	return token
}

// splitSessionEndParts splits an unconfirmed item's text into the
// app-written messages it names, by send id, each with the attribution it
// was written with, and the rest, which is the person's (person nil when
// nothing is left). A message of one member is all that member's. A joined
// one splits by the parts its dispatch recorded, exactly, whatever number
// of its members have send ids; one whose parts are unknown or no longer
// describe its text does not split.
func splitSessionEndParts(members unconfirmedMembers, content string) (app map[string]usermessage.AgentMessage, person *string, ok bool) {
	if !members.joined {
		if len(members.ids) != 1 {
			return nil, nil, false
		}
		own := members.own
		return map[string]usermessage.AgentMessage{
			members.ids[0]: storedAgentMessage(members.ids[0], content, own.Origin, own.OriginThread, own.OriginWake),
		}, nil, true
	}
	texts, fits := agentmeta.SplitJoinedParts(content, members.parts)
	if !fits || !partsNameIDs(members.parts, members.ids) {
		return nil, nil, false
	}
	app = make(map[string]usermessage.AgentMessage)
	var kept []string
	for i, part := range members.parts {
		if appWrittenSendID(part.SendID) {
			app[part.SendID] = storedAgentMessage(part.SendID, texts[i], part.Origin, part.OriginThread, part.OriginWake)
			continue
		}
		kept = append(kept, texts[i])
	}
	if len(kept) == 0 {
		return app, nil, true
	}
	joined := strings.Join(kept, agentmeta.JoinSeparator)
	return app, &joined, true
}

// partsNameIDs reports whether parts name the send ids of a joined message
// in its order; a member without a send id is in parts but not in ids.
func partsNameIDs(parts []agentmeta.JoinedPart, ids []string) bool {
	var named []string
	for _, part := range parts {
		if part.SendID != "" {
			named = append(named, part.SendID)
		}
	}
	return slices.Equal(named, ids)
}

// agentAppPartRoot is the send id root of a message the app wrote for a
// thread (a loop or watch wake, an outside send, a worker's prompt), or
// empty for any other message, which is a person's.
func agentAppPartRoot(sendID string) string {
	for _, root := range []string{agentWakeSendIDRoot, agentSendIDRoot, workerPromptSendIDRoot, assistantSendIDRoot} {
		if strings.HasPrefix(sendID, root) {
			return root
		}
	}
	return ""
}

// unconfirmedMembers is what an unconfirmed item says of the messages it
// carries: their send ids in order, whether it is a joined message, and the
// parts its dispatch recorded when it is.
type unconfirmedMembers struct {
	ids    []string
	joined bool
	parts  []agentmeta.JoinedPart
	// own is the attribution of an item of one message, its facts
	// included: its row's, or its payload's when it has no row.
	own agentmeta.JoinedPart
}

// unconfirmedMembersOf reads an unconfirmed item's members: from the row it
// reserved or deferred, which a joined message lists them all on, or from
// its queue payload when it never got that far, which a joined message
// requeued after a session death lists them on too. Parts come from its
// pending send, its payload or its row, in that order.
func unconfirmedMembersOf(item triage.UnconfirmedFlushItem) unconfirmedMembers {
	var members unconfirmedMembers
	members.ids, members.joined = unconfirmedSendIDs(item)
	var payload flushQueuePayload
	decoded := len(item.Payload) > 0 && json.Unmarshal(item.Payload, &payload) == nil
	members.own = agentmeta.JoinedPart{Origin: payload.Origin, OriginThread: payload.OriginThread, OriginWake: payload.OriginWake}
	for _, row := range []*store.Item{item.QuietItem, item.DeferredItem} {
		if row == nil {
			continue
		}
		if meta, err := usermessage.FromItem(*row); err == nil {
			members.own = agentmeta.JoinedPart{Origin: meta.Origin, OriginThread: meta.OriginThread, OriginWake: meta.OriginWake}
			break
		}
	}
	switch {
	case len(item.JoinedParts) > 0:
		members.parts = item.JoinedParts
	case decoded && len(payload.JoinedParts) > 0:
		members.parts = payload.JoinedParts
	default:
		for _, row := range []*store.Item{item.DeferredItem, item.QuietItem} {
			if row == nil {
				continue
			}
			if meta, err := usermessage.FromItem(*row); err == nil && len(meta.JoinedParts) > 0 {
				members.parts = meta.JoinedParts
				break
			}
		}
	}
	members.joined = members.joined || len(members.parts) > 0
	return members
}

// unconfirmedSendIDs names every message an unconfirmed item carries, in
// order, and reports whether the item is a joined message
// (unconfirmedMembersOf).
func unconfirmedSendIDs(item triage.UnconfirmedFlushItem) (ids []string, joined bool) {
	for _, row := range []*store.Item{item.DeferredItem, item.QuietItem} {
		if row == nil {
			continue
		}
		meta, err := usermessage.FromItem(*row)
		if err != nil {
			return nil, false
		}
		if len(meta.JoinedSendIDs) > 0 {
			return meta.JoinedSendIDs, true
		}
		if meta.SendID != "" {
			return []string{meta.SendID}, false
		}
	}
	var payload flushQueuePayload
	if len(item.Payload) > 0 && json.Unmarshal(item.Payload, &payload) == nil {
		if len(payload.JoinedSendIDs) > 0 {
			return payload.JoinedSendIDs, true
		}
		if payload.SendID != "" {
			return []string{payload.SendID}, false
		}
	}
	return nil, false
}

// noteAgentWakesDropped records `not delivered` on the rows whose wakes a
// wholesale queue drop (the person's Stop, an archive, a Codex rollback)
// throws away, so thread_status does not report them delivered, and a watch
// keeps the wake's body. They keep their schedule. A worker's prompt among
// them goes back to the user in the worker, and an outside send leaves its
// thread a notice.
//
// A request's answer or reminder and a remote job's completion are held
// instead: the rows the drop must keep, for giveBackToOwners.
func (a *App) noteAgentWakesDropped(threadID string) (held []ownedMessage) {
	rows, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		logThreadRequestSweep("list the queued wakes of "+threadID, err)
		return nil
	}
	var sends []string
	for _, row := range rows {
		if a.handBackWorkerPromptWake(row.SendID) {
			continue
		}
		if ownedSendID(row.SendID) {
			held = append(held, ownedMessage{sendID: row.SendID, rowID: row.ID})
			continue
		}
		if strings.HasPrefix(row.SendID, agentSendIDRoot) {
			sends = append(sends, row.ID)
			continue
		}
		if wakeID, _, ok := agentWakeFromSendID(row.SendID); ok {
			a.noteAgentWakeUndelivered(threadID, wakeID, agentWakeOutcomeDropped, row.Message)
		}
	}
	if len(sends) > 0 {
		a.noteAgentSendsCleared(threadID, sends)
	}
	return held
}

// ownedMessage is a message that belongs to a request or a watch: an answer,
// a reminder or a remote job's completion, by its send id. rowID is its queue
// row, empty when it has none left; unconfirmed marks one the provider was
// handed and never confirmed, whose row went at the write. ended marks one
// a session end or a failed Codex resend did not take in, rather than a
// person's Stop, archive or revert or a restart.
type ownedMessage struct {
	sendID      string
	rowID       string
	unconfirmed bool
	ended       bool
}

// withdrawal is the store scope that gives m back.
func (m ownedMessage) withdrawal() store.WakeWithdrawal {
	if m.unconfirmed {
		return store.WithdrawUnconfirmed
	}
	return store.WithdrawQueued
}

// ownedSendID reports an answer's, a reminder's or a remote completion's
// send id, or an agent's answer to its worker's async questions, whose
// owner is those questions (returnWorkerAnswer).
func ownedSendID(sendID string) bool {
	_, _, isAnswer := threadWakeFromSendID(sendID)
	_, _, isCompletion := remoteCompletionFromSendID(sendID)
	return isAnswer || isCompletion || isWorkerAnswer(sendID)
}

// giveBackToOwners gives answers, reminders and remote completions that
// never reached the agent back to their owners. A request is owed its answer
// again, disarmed, so thread_status returns it and nothing delivers it
// unasked; a watch is pending again, and the watcher delivers its completion
// as it does one that finishes later, starting the session for it; a
// spawner's answer to its worker's async questions puts them back to
// unanswered, routed again (returnWorkerAnswer). None of them goes to the
// composer: it is not the person's to send.
//
// A completion a session end gave back (ended) is delivered again on the
// thread request wake retry's ladder rather than at once, so a session that
// keeps ending before it takes the completion in is not started every few
// seconds; once the ladder is spent, the watch is dismissed with the reason
// and keeps the completion for remote_jobs.
//
// Every path that takes one out of the thread's delivery runs it: a
// wholesale drop (Stop, revert, teardown), a session that ended with it
// queued or unconfirmed, a Codex resend that failed, and the boot sweep. A
// drop runs it once the queue is gone, memory and dispatch included, because
// the watcher can queue a pending completion again within seconds and one
// queued before then would be dropped with the rest. Each message goes back
// by its row, so a message queued again after the caller read the queue is
// not taken; an unconfirmed one has no row left and goes back while its
// owner still reads queued. One the provider confirmed never goes back:
// its owner's store write refuses it and only removes a row that outlived
// the echo.
func (a *App) giveBackToOwners(threadID string, messages []ownedMessage) {
	returned := false
	a.retryOwnedEchoes(threadID)
	for _, m := range messages {
		if a.echoed(m.sendID) {
			// The provider took it in; only its confirmation is unwritten.
			continue
		}
		if isWorkerAnswer(m.sendID) {
			// Its questions say whether the provider took it in: its echo's
			// confirmation marks them delivered, and nothing returns them
			// then.
			logThreadRequestSweep("return "+m.sendID+" to its questions", a.returnWorkerAnswer(threadID, m.sendID, false))
			continue
		}
		if token, late, ok := threadWakeFromSendID(m.sendID); ok {
			// The message's requests go back together: its carrier and
			// every request that rode with it.
			returned, err := a.store.WithdrawThreadWake(token, late, m.rowID, m.withdrawal())
			logThreadRequestSweep("return "+m.sendID+" to its requests", err)
			if err == nil {
				a.redeliverHandedOffWakes(threadID, returned, late)
			}
			continue
		}
		if computerID, requestID, ok := remoteCompletionFromSendID(m.sendID); ok {
			var paced func(int64) store.CompletionReturn
			gaveUp := false
			if m.ended {
				paced = func(attempts int64) store.CompletionReturn {
					back := completionRedelivery(time.Now(), attempts)
					gaveUp = back.Issue != ""
					return back
				}
			}
			err := a.store.WithdrawRemoteCompletion(computerID, requestID, m.rowID, m.withdrawal(), paced)
			logThreadRequestSweep("return "+m.sendID+" to its watch", err)
			if err == nil && gaveUp {
				log.Printf("remote completion watches: giving up on delivering %s after %d session ends", m.sendID, threadWakeRetryAttempts+1)
			}
			returned = returned || err == nil
		}
	}
	if returned {
		// A completion given back is its watch's to deliver again.
		a.nudgeRemoteWatches()
		a.emitBackgroundChanged(threadID)
	}
	if a.heldReturnedForTest != nil {
		a.heldReturnedForTest(threadID)
	}
}

// confirmOwnedDeliveries records the provider's echo of the answers,
// reminders, completions and spawners' answers to async questions consumed
// sends carry: the second phase of their owners' delivery mark, after which
// nothing gives them back, with the row that carried an answer, which its
// request or its questions name from then on, whether or not the echo's own
// row write lands. It runs on
// the provider read loop under the flush anchor lock, so a person's send
// costs nothing but the id check. A confirmation whose write fails is kept
// and written again (app_thread_tools_bookkeeping.go), and until then the
// message reads consumed to every give-back.
func (a *App) confirmOwnedDeliveries(threadID string, sends []triage.ConsumedSend) {
	now := time.Now().UnixMilli()
	retried := false
	for _, send := range sends {
		for _, id := range send.SendIDs {
			if !ownedSendID(id) {
				continue
			}
			if !retried {
				a.retryOwnedEchoes(threadID)
				retried = true
			}
			if err := a.confirmOwnedEcho(id, ownedEcho{threadID: threadID, itemID: send.ItemID, at: now}); err != nil {
				log.Printf("thread tools: confirm %s in %s, kept to try again: %v", id, threadID, err)
			}
		}
	}
}

// giveBackUnconfirmedAtBoot gives back the answers, reminders and
// completions the previous process handed a provider that never confirmed
// them: their rows went at the provider write, and the session that held
// them is gone. A message whose row is still queued is the row's, which
// restoreDurableFlushQueueAtBoot gives back or keeps for the next boot. One
// whose echo a row proves is confirmed instead (confirmEchoedAtBoot).
func (a *App) giveBackUnconfirmedAtBoot() {
	byThread := make(map[string][]ownedMessage)
	requests, err := a.store.UnconfirmedThreadWakes()
	logThreadRequestSweep("list the unconfirmed thread wakes at boot", err)
	firsts := make(map[string]bool)
	for _, r := range requests {
		// A request that rode with another's first wake was the carrier's
		// message, given back once by the carrier's send id.
		if first := threadWakeSendID(r.WakeCarrier()); r.DeliveredAt != 0 && r.ConfirmedAt == 0 && !firsts[first] {
			firsts[first] = true
			byThread[r.CallerThreadID] = append(byThread[r.CallerThreadID], ownedMessage{sendID: first, unconfirmed: true})
		}
		if r.LateDeliveredAt != 0 && r.LateConfirmedAt == 0 {
			byThread[r.CallerThreadID] = append(byThread[r.CallerThreadID], ownedMessage{sendID: threadWakeLateSendID(r.Token), unconfirmed: true})
		}
	}
	watches, err := a.store.UnconfirmedRemoteCompletions()
	logThreadRequestSweep("list the unconfirmed remote completions at boot", err)
	for _, w := range watches {
		byThread[w.ThreadID] = append(byThread[w.ThreadID], ownedMessage{sendID: remoteCompletionSendID(w), unconfirmed: true})
	}
	for threadID, owned := range byThread {
		rows, err := a.store.ListFlushQueueItems(threadID)
		if err != nil {
			logThreadRequestSweep("list the queued messages of "+threadID+" at boot", err)
			continue
		}
		owned = slices.DeleteFunc(owned, func(m ownedMessage) bool {
			return slices.ContainsFunc(rows, func(row store.FlushQueueItem) bool { return row.SendID == m.sendID })
		})
		if owned = a.confirmEchoedAtBoot(threadID, owned); len(owned) > 0 {
			a.giveBackToOwners(threadID, owned)
		}
	}
}

// completionRedelivery paces a completion a session end gave back, after
// attempts earlier ones, on the thread request wake retry's ladder
// (retryUndeliveredThreadWakes): each return books the next delivery
// further out, and the one after the last attempt gives it up.
func completionRedelivery(now time.Time, attempts int64) store.CompletionReturn {
	if attempts >= threadWakeRetryAttempts {
		return store.CompletionReturn{Issue: fmt.Sprintf(
			"Not delivered: the conversation's session ended %d times before the agent took this completion in.", attempts+1)}
	}
	attempts++
	return store.CompletionReturn{NextCheck: now.Add(threadWakeRetryDelay(attempts)).UnixMilli(), Attempts: attempts}
}
