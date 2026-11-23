package harnessrpc

import (
	"fmt"
	"strings"
)

// ThreadWork is what one thread still runs or owes, from what the app holds
// in memory: the answer the thread tools and the Agents rows read
// (App.threadWaitingOn), with that read's limits. The zero value is a
// thread at rest now. A loop, a watch or a run kept open is no turn and
// does not appear here. Provider frames the backend has not handled yet
// are not read: one can still reopen a turn a Claude soft close settled.
type ThreadWork struct {
	// ActiveTurn is whether a turn's round is open.
	ActiveTurn bool `json:"activeTurn"`
	// PendingSends is whether a message is queued, or written to the
	// provider with its turn still to open, or a turn start is under way.
	PendingSends bool `json:"pendingSends"`
	// WaitingOn is "approval" or "question" while a prompt of the session
	// waits for an answer, empty otherwise.
	WaitingOn string `json:"waitingOn,omitempty"`
	// Compacting is whether the provider is compacting the thread's context.
	Compacting bool `json:"compacting"`
}

// Idle reports whether nothing runs or is owed.
func (w ThreadWork) Idle() bool {
	return !w.ActiveTurn && !w.PendingSends && w.WaitingOn == "" && !w.Compacting
}

// HarnessThreadWorkResult is the work of one thread that is not at rest.
type HarnessThreadWorkResult struct {
	ThreadID string `json:"threadId"`
	ThreadWork
}

// HarnessThreadsWorking answers which of the given threads still run or owe
// a turn, and what, in the order given; a thread at rest is left out. A
// spec that must leave a store with no turn of its own in flight asks this
// rather than a store row or the sidebar's sends-pending publication,
// which trail the turn they describe. An empty list or a blank id is
// refused.
func (h *Harness) HarnessThreadsWorking(threadIDs []string) ([]HarnessThreadWorkResult, error) {
	if h.config.Host == nil {
		return nil, fmt.Errorf("harness host unavailable")
	}
	if len(threadIDs) == 0 {
		return nil, fmt.Errorf("at least one thread id is required")
	}
	out := []HarnessThreadWorkResult{}
	for index, threadID := range threadIDs {
		threadID = strings.TrimSpace(threadID)
		if threadID == "" {
			return nil, fmt.Errorf("thread id %d is blank", index)
		}
		if work := h.config.Host.ThreadWork(threadID); !work.Idle() {
			out = append(out, HarnessThreadWorkResult{ThreadID: threadID, ThreadWork: work})
		}
	}
	return out, nil
}
