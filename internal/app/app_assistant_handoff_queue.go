package app

import (
	"context"
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// What a handoff carries on from the old thread once its session is gone:
// the requests, wakes and outside sends still queued for it, and the runs of
// the loops and watches that moved.

// continueAfterHandOff carries over what the old thread held once its
// session is gone: its queued messages go to their owners, which send them
// on to the successor, and the loops and watches that moved run there.
func (a *App) continueAfterHandOff(oldThreadID, newThreadID string, handOff store.AssistantHandOff) {
	// Loop and watch wakes and other threads' requests still queued for the
	// old thread go on to the successor's queue. Everything else
	// app-written goes on through its owner (withdrawQueuedMessages), and an
	// opening the old thread never read is dropped: it is not the
	// successor's.
	rows, err := a.store.ListFlushQueueItems(oldThreadID)
	if err != nil {
		log.Printf("assistants: hand off %s: read its queue: %v", oldThreadID, err)
	}
	for _, row := range rows {
		switch {
		case threadRequestSendIDToken(row.SendID) != "":
			a.moveQueuedRequest(oldThreadID, row)
		default:
			if _, _, ok := agentWakeFromSendID(row.SendID); ok {
				a.moveQueuedAgentWake(oldThreadID, row)
			}
		}
	}
	if err := a.withdrawQueuedMessages(oldThreadID, everyQueuedMessage, queuedWithdrawal{why: "the assistant moved to a new thread before the agent took it in"}); err != nil {
		log.Printf("assistants: hand off %s: %v", oldThreadID, err)
	}
	// A worker's prompt the old thread took in and left unanswered is the
	// successor's too, since the spawn rows moved.
	a.reroutePromptsFrom(oldThreadID)
	a.restartMovedAgentWakes(oldThreadID, newThreadID, handOff.MovedWakeIDs)
}

// moveQueuedRequest takes another thread's request still queued on the old
// thread back and queues it on the successor (requeueRequest follows the
// handoff).
func (a *App) moveQueuedRequest(oldThreadID string, row store.FlushQueueItem) {
	token := threadRequestSendIDToken(row.SendID)
	removal, err := a.removeQueuedItem(a.lifeCtx(), oldThreadID, row.SendID)
	if err != nil {
		log.Printf("assistants: hand off %s: withdraw request %s: %v", oldThreadID, token, err)
		return
	}
	if removal != queueRemovalRemoved {
		return
	}
	a.giveBackRequest(oldThreadID, token, queuedAgentMessage(row))
}

// moveQueuedAgentWake takes a loop or watch wake still queued on the old
// thread with no session to drop it back and queues it on the successor,
// whose row it now is. A live session's teardown takes the wakes it drops
// back the same way (returnRequestsAtTeardown).
func (a *App) moveQueuedAgentWake(oldThreadID string, row store.FlushQueueItem) {
	removal, err := a.removeQueuedItem(a.lifeCtx(), oldThreadID, row.SendID)
	if err != nil {
		log.Printf("assistants: hand off %s: withdraw %s: %v", oldThreadID, row.SendID, err)
		return
	}
	if removal == queueRemovalRemoved {
		a.redeliverMovedAgentWake(oldThreadID, returnedWake{sendID: row.SendID, message: queuedAgentMessage(row)})
	}
}

// redeliverMovedAgentWake queues a wake taken back from a thread a handoff
// superseded on the thread that holds its row now. One that cannot be
// queued there is recorded on the row as not delivered, a watch keeping its
// output, and the next fire says so. A teardown calls it holding the old
// thread's action lock, so a start it tries again for the wake runs as
// notice work, holding no lock while it waits for a delete's decision.
func (a *App) redeliverMovedAgentWake(oldThreadID string, w returnedWake) {
	wakeID, fireAt, _ := agentWakeFromSendID(w.sendID)
	row, found, err := a.store.GetAgentWake(wakeID)
	if err == nil && !found {
		return
	}
	settle := func(err error) {
		if err != nil {
			log.Printf("assistants: hand off %s: move %s on: %v", oldThreadID, w.sendID, err)
			a.noteMovedAgentWakeUndelivered(oldThreadID, w)
		}
	}
	var retry func(context.Context) error
	if err == nil {
		if err = a.lifeCtx().Err(); err == nil {
			retry, err = a.queueAgentWake(row, fireAt, w.message)
		}
	}
	if retry != nil && !a.goAgentNoticeWork(row.ThreadID, func() { settle(retry(a.lifeCtx())) }) {
		err = ErrShuttingDown
	}
	settle(err)
}

// noteMovedAgentWakeUndelivered records on its row a wake a handoff could
// not move.
func (a *App) noteMovedAgentWakeUndelivered(oldThreadID string, w returnedWake) {
	wakeID, _, _ := agentWakeFromSendID(w.sendID)
	threadID := oldThreadID
	if current, ok := a.assistantCurrentThread(oldThreadID); ok {
		threadID = current
	}
	a.noteAgentWakeUndelivered(threadID, wakeID, agentWakeOutcomeNotDelivered+": the assistant moved to a new thread", w.message.Text())
}

// restartMovedAgentWakes moves the runs of the loops and watches the
// handoff moved. A long-running watch's process is restarted under the
// successor, as at boot; an interval run in flight finishes, and its wake
// follows the row.
func (a *App) restartMovedAgentWakes(oldThreadID, newThreadID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	a.emitAgentWakesRemoved(oldThreadID, ids)
	for _, id := range ids {
		row, found, err := a.store.GetAgentWake(id)
		if err != nil || !found {
			if err != nil {
				log.Printf("assistants: read moved wake %s: %v", id, err)
			}
			continue
		}
		if row.Kind == store.AgentWakeWatch && row.LongRunning {
			a.stopLongRunningAgentWatch(id)
			a.startAgentWatchProcess(row)
		}
		a.emitAgentWakeChanged(newThreadID, id)
	}
	a.nudgeThreadRequestSweep()
}

// redeliverOutsideSend queues an outside `thread send` the old thread never
// took in on the assistant's current thread.
func (a *App) redeliverOutsideSend(threadID string, message usermessage.AgentMessage) {
	if _, err := a.queueOutsideSend(threadID, message); err != nil {
		log.Printf("assistants: send an outside message on to %s: %v", threadID, err)
		a.noteAgentSendNotDelivered(threadID, "the assistant moved to a new thread and the message could not follow it")
	}
}
