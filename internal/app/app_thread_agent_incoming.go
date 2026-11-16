package app

import (
	"context"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// The Agents segment's incoming rows are the messages agents and wakes
// queued for a thread behind its current turn, read by the client from the
// thread's queue (QueuedItem.AgentMessage). A person can withdraw one before the
// thread's agent takes it in; each goes back to its owner through the one
// withdrawal every caller uses (withdrawQueuedMessage, byPerson), so its
// owner learns it was not delivered:
//
//   - a request another thread made: its receipt is cancelled, and the
//     sender is told the user withdrew it;
//   - an answer to this thread's own request: owed again and disarmed
//     (store.WithdrawThreadWake), as an archive leaves it, so thread_status
//     still returns it;
//   - a worker's question: back to the user in the worker;
//   - the spawner's answer to this worker's async questions: they are
//     unanswered again and the person's, as a hand-back leaves them
//     (returnWorkerAnswer);
//   - a loop or watch wake: recorded on its row as not delivered;
//   - an outside `thread send`: a notice in the thread;
//   - a remote job's completion: dismissed on its watch with the reason
//     (store.WithdrawRemoteCompletion), so it is never delivered again and
//     remote_jobs and remote_status still return it;
//   - a workflow run's wake: dropped, as a queue drop drops it.

// stoppedRemoteCompletion dismisses a completion a person withdrew: a
// completion handed back to its watch would be queued again on the next tick.
func stoppedRemoteCompletion(int64) store.CompletionReturn {
	return store.CompletionReturn{Issue: "The user withdrew it from the queue before the agent read it."}
}

// StopThreadAgentIncoming withdraws one message an agent or a wake queued
// for the thread, named by its send id. A message the thread's agent
// already took in reports ended.
//
//ao:scope threads:operate
func (a *App) StopThreadAgentIncoming(ctx context.Context, threadID, sendID string) (ThreadAgentStop, error) {
	for _, item := range a.queueSnapshotForThread(threadID) {
		if item.SendID == sendID && !item.AgentMessage {
			return ThreadAgentStop{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"That queued message is not one an agent or a wake sent.", nil)
		}
	}
	return a.withdrawIncomingAgentMessage(ctx, threadID, sendID), nil
}

// incomingAgentSendIDs lists the thread's queued messages that are incoming
// rows, in queue order: those the queue marks an agent's (AgentMessage).
func (a *App) incomingAgentSendIDs(threadID string) []string {
	var out []string
	for _, item := range a.queueSnapshotForThread(threadID) {
		if item.AgentMessage {
			out = append(out, item.SendID)
		}
	}
	return out
}

// withdrawIncomingAgentMessage takes one incoming message back out of the
// thread's queue and settles it with its owner (withdrawQueuedMessage).
func (a *App) withdrawIncomingAgentMessage(ctx context.Context, threadID, sendID string) ThreadAgentStop {
	result := ThreadAgentStop{Token: sendID, Outcome: threadAgentStopped, Incoming: true}
	removal, err := a.withdrawQueuedMessage(ctx, threadID, sendID, queuedWithdrawal{byPerson: true})
	switch {
	case removal == queueRemovalRemoved && err != nil:
		result.Failures = append(result.Failures, err.Error())
	case err != nil:
		result.Outcome = threadAgentFailed
		result.Failures = append(result.Failures, "withdraw it: "+err.Error())
	case removal != queueRemovalRemoved:
		result.Outcome = threadAgentEnded
	}
	return result
}
