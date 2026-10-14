package app

import (
	"context"
	"strings"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"
)

// The Agents segment's incoming rows are the messages agents and wakes
// queued for a thread behind its current turn, read by the client from the
// thread's queue (QueuedItem.Origin). A person can withdraw one before the
// thread's agent takes it in; each kind goes back the way a wholesale queue
// drop sends it back, so its owner learns it was not delivered:
//
//   - a request another thread made: its receipt is cancelled, and the
//     sender is told the user withdrew it;
//   - an answer to this thread's own request: owed again and disarmed
//     (store.WithdrawThreadWake), as an archive leaves it, so thread_status
//     still returns it;
//   - a worker's question: back to the user in the worker;
//   - a loop or watch wake: recorded on its row as not delivered;
//   - an outside `thread send`: a notice in the thread.

// StopThreadAgentIncoming withdraws one message an agent or a wake queued
// for the thread, named by its send id. A message the thread's agent
// already took in reports ended.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentIncoming(ctx context.Context, threadID, sendID string) (ThreadAgentStop, error) {
	if !incomingAgentSendID(sendID) {
		return ThreadAgentStop{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"That queued message is not one an agent or a wake sent.", nil)
	}
	return a.withdrawIncomingAgentMessage(ctx, threadID, sendID), nil
}

// incomingAgentSendID reports a send id the app gives a message it queues
// for an agent's thread on another's behalf. A person's composer send ids
// are client-minted and carry none of these prefixes.
func incomingAgentSendID(sendID string) bool {
	if strings.HasPrefix(sendID, threadRequestSendIDPrefix) || agentAppPartRoot(sendID) != "" {
		return true
	}
	_, _, ok := threadWakeFromSendID(sendID)
	return ok
}

// incomingAgentSendIDs lists the thread's queued messages that are incoming
// rows, in queue order. Interim: the origin and send id decide it, as the
// frontend's agentMessageKind does, until the queue carries a
// server-computed agent-message flag that replaces both checks.
func (a *App) incomingAgentSendIDs(threadID string) []string {
	var out []string
	for _, item := range a.queueSnapshotForThread(threadID) {
		if (item.Origin == usermessage.OriginAgentThread || item.Origin == usermessage.OriginAgentWake) && incomingAgentSendID(item.SendID) {
			out = append(out, item.SendID)
		}
	}
	return out
}

// withdrawIncomingAgentMessage takes one incoming message back out of the
// thread's queue and settles it with its owner.
func (a *App) withdrawIncomingAgentMessage(ctx context.Context, threadID, sendID string) ThreadAgentStop {
	result := ThreadAgentStop{Token: sendID, Outcome: threadAgentStopped, Incoming: true}
	var withdrawn store.FlushQueueItem
	deleteRow := func(row store.FlushQueueItem) error {
		withdrawn = row
		return a.store.DeleteFlushQueueItem(row.ID)
	}
	if token, late, ok := threadWakeFromSendID(sendID); ok {
		deleteRow = func(row store.FlushQueueItem) error {
			return a.store.WithdrawThreadWake(token, late, row.ID, false)
		}
	}
	removal, err := a.removeQueuedItemWith(ctx, threadID, sendID, deleteRow)
	if err != nil {
		result.Outcome = threadAgentFailed
		result.Failures = append(result.Failures, "withdraw it: "+err.Error())
		return result
	}
	if removal != queueRemovalRemoved {
		result.Outcome = threadAgentEnded
		return result
	}
	switch {
	case strings.HasPrefix(sendID, threadRequestSendIDPrefix):
		token := strings.TrimPrefix(sendID, threadRequestSendIDPrefix)
		if err := a.settleThreadReceipt(token, []string{store.ThreadReceiptAccepted}, store.ThreadRequestSettlement{
			State: store.ThreadReceiptCancelled, Answer: []byte(threadRequestUserWithdrew), AnswerKind: store.ThreadAnswerError,
		}); err != nil {
			result.Failures = append(result.Failures, "tell its sender: "+err.Error())
		}
	case a.handBackWorkerPromptWake(sendID):
	case strings.HasPrefix(sendID, agentSendIDRoot):
		a.noteAgentSendNotDelivered(threadID, "the user withdrew it from the queue")
	default:
		if wakeID, _, ok := agentWakeFromSendID(sendID); ok {
			a.noteAgentWakeUndelivered(threadID, wakeID, agentWakeOutcomeWithdrawn, withdrawn.Message)
		}
	}
	return result
}
