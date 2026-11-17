package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// A message the app queued for a thread on someone's behalf goes back to
// that owner when it is taken out of the queue before the agent took it
// in: a person withdrawing it from the thread's Incoming rows, a rest or an
// archive withdrawing what the thread held when it was captured, a handoff
// moving an assistant to its next thread, a cancel taking back a loop's
// wakes. Every one of them withdraws through withdrawQueuedMessage, which
// finds the message's owner by its send id root (queuedKindOf) and takes it
// out through removeQueuedItemWith, the owner's own write as the durable
// delete where the owner has one:
//
//   - a loop or watch wake: dropped; withdrawn by a person, its row records
//     it as not delivered (a cancel recorded its outcome already);
//   - an outside `thread send`: a notice in the thread, or delivered on to
//     an assistant's current thread (outsideSendNotTakenIn);
//   - a worker's prompt: back to the user in the worker;
//   - an assistant's opening: dropped;
//   - a remote job's completion: back to its watch, which delivers it once
//     the thread is reachable again; withdrawn by a person, dismissed with
//     the reason, as a watch would only queue it again;
//   - an answer or reminder to the thread's own request: owed again and
//     disarmed (store.WithdrawThreadWake); one a handoff superseded goes on
//     to the assistant's current thread;
//   - the spawner's answer to the thread's async questions: they are
//     unanswered again in one transaction with the row's removal
//     (returnWorkerAnswer); withdrawn by a person, they are the person's;
//   - another thread's request, a workflow run's wake and a worker's
//     stopped notice: their owners settle them on their own paths (the
//     request's stop, the run, the spawner), so only a person withdraws
//     them here: the request's sender is told the user withdrew it, and
//     the others are dropped.

// queuedWithdrawal is who takes a queued message back: a person from the
// thread's Incoming rows (byPerson), or the app, with the reason an outside
// send's notice ends in (why).
type queuedWithdrawal struct {
	byPerson bool
	why      string
}

// queuedKind is whose a queued message is, by its send id's root.
type queuedKind int

const (
	// queuedUnowned has no app root: a person's own message.
	queuedUnowned queuedKind = iota
	queuedLoopWake
	queuedOutsideSend
	queuedWorkerPrompt
	queuedAssistantOpening
	queuedRemoteCompletion
	queuedThreadWake
	queuedWorkerAnswer
	queuedThreadRequest
	queuedWorkflowWake
	queuedWorkerStopped
)

// queuedKindRoots names the owner of every root in appSendIDRoots.
var queuedKindRoots = []struct {
	root string
	kind queuedKind
}{
	{agentWakeSendIDRoot, queuedLoopWake},
	{agentSendIDRoot, queuedOutsideSend},
	{workerPromptSendIDRoot, queuedWorkerPrompt},
	{assistantSendIDRoot, queuedAssistantOpening},
	{remoteCompletionSendIDRoot, queuedRemoteCompletion},
	{threadWakeSendID(""), queuedThreadWake},
	{threadWakeLateSendID(""), queuedThreadWake},
	{workerAnswerSendIDRoot, queuedWorkerAnswer},
	{threadRequestSendIDPrefix, queuedThreadRequest},
	{workflowWakeSendIDRoot, queuedWorkflowWake},
	{workerStoppedSendIDRoot, queuedWorkerStopped},
}

// queuedKindOf is the owner of the queued message sendID.
func queuedKindOf(sendID string) queuedKind {
	for _, entry := range queuedKindRoots {
		if strings.HasPrefix(sendID, entry.root) {
			return entry.kind
		}
	}
	return queuedUnowned
}

// queuedOwner is how one queued message goes back to its owner: remove is
// the durable delete removeQueuedItemWith runs under its locks, and settle
// what the owner is told once the message is out of the queue.
type queuedOwner struct {
	remove func(row store.FlushQueueItem) error
	settle func(row store.FlushQueueItem) error
}

// queuedMessageOwner is the owner of threadID's queued message sendID for
// a withdrawal by. ok is false for a message only its own owner or a person
// withdraws (an app withdrawal leaves it queued), and for a person's own
// message, which no app withdrawal takes.
func (a *App) queuedMessageOwner(threadID, sendID string, by queuedWithdrawal) (owner queuedOwner, ok bool) {
	drop := func(row store.FlushQueueItem) error { return a.store.DeleteFlushQueueItem(row.ID) }
	settled := func(store.FlushQueueItem) error { return nil }
	owner = queuedOwner{remove: drop, settle: settled}
	switch queuedKindOf(sendID) {
	case queuedLoopWake:
		if by.byPerson {
			owner.settle = func(row store.FlushQueueItem) error {
				if wakeID, _, ok := agentWakeFromSendID(sendID); ok {
					a.noteAgentWakeUndelivered(threadID, wakeID, agentWakeOutcomeWithdrawn, row.Message)
				}
				return nil
			}
		}
	case queuedOutsideSend:
		owner.settle = func(row store.FlushQueueItem) error {
			if by.byPerson {
				a.noteAgentSendNotDelivered(threadID, "the user withdrew it from the queue")
			} else {
				a.outsideSendNotTakenIn(threadID, queuedAgentMessage(row), by.why)
			}
			return nil
		}
	case queuedWorkerPrompt:
		owner.settle = func(store.FlushQueueItem) error {
			a.handBackWorkerPromptWake(sendID)
			return nil
		}
	case queuedAssistantOpening:
	case queuedRemoteCompletion:
		computerID, requestID, ok := remoteCompletionFromSendID(sendID)
		if !ok {
			break
		}
		var paced func(int64) store.CompletionReturn
		if by.byPerson {
			paced = stoppedRemoteCompletion
		}
		owner.remove = func(row store.FlushQueueItem) error {
			return a.store.WithdrawRemoteCompletion(computerID, requestID, row.ID, store.WithdrawQueued, paced)
		}
		owner.settle = func(store.FlushQueueItem) error {
			// A completion given back is its watch's to deliver again.
			a.nudgeRemoteWatches()
			a.emitBackgroundChanged(threadID)
			return nil
		}
	case queuedThreadWake:
		token, late, ok := threadWakeFromSendID(sendID)
		if !ok {
			break
		}
		var returned []string
		owner.remove = func(row store.FlushQueueItem) error {
			var err error
			returned, err = a.store.WithdrawThreadWake(token, late, row.ID, store.WithdrawQueued)
			return err
		}
		if !by.byPerson {
			owner.settle = func(store.FlushQueueItem) error {
				a.redeliverHandedOffWakes(threadID, returned, late)
				return nil
			}
		}
	case queuedWorkerAnswer:
		owner.remove = func(row store.FlushQueueItem) error {
			return a.returnWorkerAnswer(threadID, row.SendID, by.byPerson)
		}
	case queuedThreadRequest:
		if !by.byPerson {
			return queuedOwner{}, false
		}
		owner.settle = func(store.FlushQueueItem) error {
			err := a.settleThreadReceipt(threadRequestSendIDToken(sendID), []string{store.ThreadReceiptAccepted}, store.ThreadRequestSettlement{
				State: store.ThreadReceiptCancelled, Answer: []byte(threadRequestUserWithdrew), AnswerKind: store.ThreadAnswerError,
			})
			if err != nil {
				return fmt.Errorf("tell its sender: %w", err)
			}
			return nil
		}
	case queuedWorkflowWake, queuedWorkerStopped, queuedUnowned:
		if !by.byPerson {
			return queuedOwner{}, false
		}
	}
	return owner, true
}

// errQueuedSettle wraps what an owner could not be told of a message that
// did leave the queue.
type errQueuedSettle struct{ err error }

func (e errQueuedSettle) Error() string { return e.err.Error() }
func (e errQueuedSettle) Unwrap() error { return e.err }

// withdrawQueuedMessage takes threadID's queued message sendID back out of
// the queue through its owner, for a withdrawal by. It reports what the
// queue held under the id (removeQueuedItemWith); a message an app
// withdrawal leaves to its owner reports queueRemovalAbsent. An error with
// queueRemovalRemoved is an errQueuedSettle: the message left the queue
// and its owner was not told.
//
// It takes send admission, so it runs with the thread's action lock
// released.
func (a *App) withdrawQueuedMessage(ctx context.Context, threadID, sendID string, by queuedWithdrawal) (queueRemoval, error) {
	owner, ok := a.queuedMessageOwner(threadID, sendID, by)
	if !ok {
		return queueRemovalAbsent, nil
	}
	var withdrawn store.FlushQueueItem
	removal, err := a.removeQueuedItemWith(ctx, threadID, sendID, func(row store.FlushQueueItem) error {
		withdrawn = row
		return owner.remove(row)
	})
	if err != nil || removal != queueRemovalRemoved {
		return removal, err
	}
	if err := owner.settle(withdrawn); err != nil {
		return removal, errQueuedSettle{err}
	}
	return removal, nil
}

// withdrawQueuedMessages takes the messages of threadID's queue that match
// picks back through their owners, for an app withdrawal by, whether or not
// a session is live. A rest or an archive picks those the thread held when
// it was captured (restingThreadWork.held), so a message queued since is
// left alone. Left queued, a message would reach the agent at the thread's
// next start, whoever made it.
//
// An error names each message left queued: a later start would deliver
// it, so the caller reports it. One left by shutdown is the next boot's,
// which discards it the same way (discardAgentWakeAtBoot). A message the
// dispatch committed to writing is the session's.
func (a *App) withdrawQueuedMessages(threadID string, match func(sendID string) bool, by queuedWithdrawal) error {
	rows, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		return fmt.Errorf("withdraw the queued messages of %s: %w", threadID, err)
	}
	var errs []error
	for _, row := range rows {
		if !match(row.SendID) {
			continue
		}
		if _, err := a.withdrawQueuedMessage(a.lifeCtx(), threadID, row.SendID, by); err != nil {
			errs = append(errs, fmt.Errorf("withdraw %s from %s: %w", row.SendID, threadID, err))
		}
	}
	return errors.Join(errs...)
}
