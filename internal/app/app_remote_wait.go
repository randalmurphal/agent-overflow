package app

import (
	"context"
	"sync"
)

// remoteWaits tracks tool calls currently parked on a remote command. While a
// wait is active the completion watcher leaves that job alone: the reply the
// wait produces is the delivery, and a completion message on top of it would
// hand the agent the same result twice. An interrupt cancels every wait for a
// thread so the parked call returns a backgrounded receipt at once.
type remoteWaits struct {
	mu    sync.Mutex
	next  uint64
	waits map[uint64]remoteWait
}

type remoteWait struct {
	threadID string
	jobKey   string
	cancel   context.CancelCauseFunc
}

func remoteJobKey(computerID, requestID string) string { return computerID + ":" + requestID }

// beginRemoteWait derives the context a wait polls under and registers it. The
// returned end function releases the registration; callers invoke it after
// the reply is written, not before, so the watcher cannot slip a duplicate in
// between the terminal observation and the response.
func (a *App) beginRemoteWait(ctx context.Context, threadID, computerID, requestID string) (context.Context, func()) {
	waitCtx, cancel := context.WithCancelCause(ctx)
	a.remoteWaitsRegistry.mu.Lock()
	defer a.remoteWaitsRegistry.mu.Unlock()
	if a.remoteWaitsRegistry.waits == nil {
		a.remoteWaitsRegistry.waits = make(map[uint64]remoteWait)
	}
	a.remoteWaitsRegistry.next++
	id := a.remoteWaitsRegistry.next
	a.remoteWaitsRegistry.waits[id] = remoteWait{threadID: threadID, jobKey: remoteJobKey(computerID, requestID), cancel: cancel}
	return waitCtx, func() {
		a.remoteWaitsRegistry.mu.Lock()
		delete(a.remoteWaitsRegistry.waits, id)
		a.remoteWaitsRegistry.mu.Unlock()
		cancel(nil)
	}
}

func (a *App) remoteWaitActive(computerID, requestID string) bool {
	key := remoteJobKey(computerID, requestID)
	a.remoteWaitsRegistry.mu.Lock()
	defer a.remoteWaitsRegistry.mu.Unlock()
	for _, w := range a.remoteWaitsRegistry.waits {
		if w.jobKey == key {
			return true
		}
	}
	return false
}

// cancelRemoteWaits ends every wait parked for a thread. The commands keep
// running; only the tool calls stop waiting for them.
func (a *App) cancelRemoteWaits(threadID string) {
	a.remoteWaitsRegistry.mu.Lock()
	defer a.remoteWaitsRegistry.mu.Unlock()
	for _, w := range a.remoteWaitsRegistry.waits {
		if w.threadID == threadID {
			w.cancel(errRemoteWaitInterrupted)
		}
	}
}
