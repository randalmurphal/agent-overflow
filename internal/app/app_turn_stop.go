package app

import (
	"fmt"

	"agent-overflow/internal/triage"
)

// turnStop is who is stopping a turn. Every interrupt names one, because a
// turn ended as interrupted cannot say on its own whether the user, an agent
// or a workflow stopped it: the stopped thread's timeline shows it, and an
// agent whose request that turn was answering is told it.
type turnStop struct {
	// label is the stop row in the stopped thread's timeline.
	label string
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
		label:   triage.StoppedByUser,
		cause:   "The user stopped its turn before it answered.",
		consent: personalStop(transcriptRootIDs),
	}
}

// stopForTakeover is the user taking a thread over from its workflow. It
// carries no consent: a takeover keeps the thread's background agents alive.
func stopForTakeover() turnStop {
	return turnStop{
		label: triage.StoppedByUser,
		cause: "The user took the thread over from its workflow before it answered.",
	}
}

// stopByWorkflow is a workflow ending the attempt the thread runs.
func stopByWorkflow() turnStop {
	return turnStop{
		label: "Stopped by its workflow",
		cause: "Its workflow stopped the turn before it answered.",
	}
}

// stopByThread is another thread's thread_cancel. computer names the
// stopping thread's computer when it is not this one.
func stopByThread(title, threadID, computer string) turnStop {
	where, on := threadID, ""
	if computer != "" {
		where += ", on " + computer
		on = " on " + computer
	}
	return turnStop{
		label: fmt.Sprintf("Stopped by thread %q%s", title, on),
		cause: fmt.Sprintf("%q (%s) stopped its turn with thread_cancel before it answered.", title, where),
	}
}

// valid reports whether the stop names who is stopping the turn on both
// surfaces it reaches.
func (s turnStop) valid() bool { return s.label != "" && s.cause != "" }

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
