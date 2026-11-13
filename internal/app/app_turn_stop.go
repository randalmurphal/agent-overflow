package app

import "fmt"

// turnStop is who is stopping a turn. Every interrupt names one, because a
// turn ended as interrupted cannot say on its own whether the user, an agent
// or a workflow stopped it, and an agent whose request that turn was
// answering is told the cause.
type turnStop struct {
	// cause is the settlement text of a request the stopped turn answers.
	cause string
	// consent is set only on the user's own Stop: the background agents it
	// was confirmed to kill. Every other stop is never refused.
	consent *agentKillConsent
}

// stopByUser is the user's Stop, confirmed to kill the background agents
// named by transcriptRootIDs.
func stopByUser(transcriptRootIDs []string) turnStop {
	return turnStop{
		cause:   "The user stopped its turn before it answered.",
		consent: personalStop(transcriptRootIDs),
	}
}

// stopByWorkflow is a workflow taking a thread over.
func stopByWorkflow() turnStop {
	return turnStop{cause: "A workflow took over the thread before it answered."}
}

// stopByThread is another thread's thread_cancel. computer names the
// stopping thread's computer when it is not this one.
func stopByThread(title, threadID, computer string) turnStop {
	where := threadID
	if computer != "" {
		where += ", on " + computer
	}
	return turnStop{cause: fmt.Sprintf("%q (%s) stopped its turn with thread_cancel before it answered.", title, where)}
}

// turnStoppedUnknown is the settlement text of an interrupted turn no stop
// was recorded for, such as one the provider aborted on its own.
const turnStoppedUnknown = "Its turn was stopped before it answered."

// recordedTurnStop is the stop recorded against one thread's turn.
type recordedTurnStop struct {
	turnIndex int
	cause     string
}

// recordTurnStop remembers who is stopping a turn, for the requests it
// answers. It is recorded before the interrupt is sent because the turn can
// settle while the interrupt is acknowledged. Only a thread with open
// requests records one, and forgetReceiptRunning drops it with the thread's
// last open request, so the map is bounded by open requests.
func (a *App) recordTurnStop(threadID string, turnIndex int, stop turnStop) {
	if turnIndex < 0 {
		return
	}
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if len(a.threadRequests.running[threadID]) == 0 {
		return
	}
	if a.threadRequests.stops == nil {
		a.threadRequests.stops = make(map[string]recordedTurnStop)
	}
	a.threadRequests.stops[threadID] = recordedTurnStop{turnIndex: turnIndex, cause: stop.cause}
}

// dropTurnStop withdraws a stop whose interrupt was not delivered: the turn
// runs on, and a later end of it was not this stop's doing.
func (a *App) dropTurnStop(threadID string, turnIndex int) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if recorded, ok := a.threadRequests.stops[threadID]; ok && recorded.turnIndex == turnIndex {
		delete(a.threadRequests.stops, threadID)
	}
}

// turnStopCause is the recorded cause of a stopped turn, or the generic one
// when nothing recorded it.
func (a *App) turnStopCause(threadID string, turnIndex int) string {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if recorded, ok := a.threadRequests.stops[threadID]; ok && recorded.turnIndex == turnIndex {
		return recorded.cause
	}
	return turnStoppedUnknown
}
