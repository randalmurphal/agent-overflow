package app

import (
	"fmt"
	"log"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// turnStop is who is stopping a turn. Every interrupt names one, because a
// turn ended as interrupted cannot say on its own whether the user, an agent
// or a workflow stopped it: the stopped thread's timeline shows it, and an
// agent whose request that turn was answering is told it.
type turnStop struct {
	// label is the stop row in the stopped thread's timeline.
	label string
	// cause is why a request the stopped turn answers was interrupted.
	cause threadtools.Cause
	// consent is set only on the user's own Stop: the background agents it
	// was confirmed to kill. Every other stop is never refused.
	consent *agentKillConsent
}

// stopByUser is the user's Stop, confirmed to kill the background agents
// named by transcriptRootIDs. A request its turn answers settles with what
// the turn said by then (turnSettlement).
func stopByUser(transcriptRootIDs []string) turnStop {
	return turnStop{
		label:   triage.StoppedByUser,
		cause:   threadtools.Cause{Code: threadtools.CauseUserStop},
		consent: personalStop(transcriptRootIDs),
	}
}

// stopForTakeover is the user taking a thread over from its workflow. It
// carries no consent: a takeover keeps the thread's background agents alive.
func stopForTakeover() turnStop {
	return turnStop{
		label: triage.StoppedByUser,
		cause: threadtools.Cause{Code: threadtools.CauseTakeover},
	}
}

// stopByWorkflow is a workflow ending the attempt the thread runs.
func stopByWorkflow() turnStop {
	return turnStop{
		label: "Stopped by its workflow",
		cause: threadtools.Cause{Code: threadtools.CauseWorkflow},
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
		cause: threadtools.Cause{Code: threadtools.CauseThreadCancel, Arg: fmt.Sprintf("%q (%s)", title, where)},
	}
}

// stoppingComputerName names a paired computer whose thread is stopping a
// turn here, the way this computer names it everywhere else. The name that
// computer reports for itself is not used. Pairing is directional, so a
// computer this one cannot reach back is named only as another computer.
func (a *App) stoppingComputerName(computerID string) string {
	if name, _ := a.threadTools().threadToolsBackendName(computerID); name != "" {
		return name
	}
	return "another computer"
}

// valid reports whether the stop names who is stopping the turn on both
// surfaces it reaches.
func (s turnStop) valid() bool { return s.label != "" && s.cause.Known() }

// stoppedTurn names the turn a stop cut. The index is the turn's from the
// stop on: the open turn, or the turn a send the provider holds will open,
// whose row does not exist yet. startedAt is the row's start once a row at
// that index has been seen, 0 before, so a later turn a revert gives the
// same index is not read as the stopped one. session is the session the
// interrupt was sent to: a send still waiting when that session ends never
// opens the stopped turn.
type stoppedTurn struct {
	index     int
	startedAt int64
	session   string
}

// stoppedTurnOf names the turn an interrupt sent to session cuts:
// cutTurn, which is openTurn or the turn of a send the provider holds.
// An open turn is pinned to its row's start, read only when a request is
// open on the thread, the only time a stop is recorded.
func (a *App) stoppedTurnOf(threadID, session string, cutTurn, openTurn int) stoppedTurn {
	stopped := stoppedTurn{index: cutTurn, session: session}
	if cutTurn < 0 || cutTurn != openTurn {
		return stopped
	}
	if _, open := a.runningReceiptTokens(threadID); !open {
		return stopped
	}
	turn, found, err := a.store.GetTurnByThreadIndex(threadID, cutTurn)
	if err != nil {
		// Unpinned, it is pinned by the first read of its end.
		log.Printf("thread tools: read the stopped turn %d of %s: %v", cutTurn, threadID, err)
		return stopped
	}
	if found {
		stopped.startedAt = turn.StartedAt
	}
	return stopped
}

// matches reports whether turn is the stopped one. A turn read with no
// start (an index alone) matches by index.
func (s stoppedTurn) matches(turn store.Turn) bool {
	return s.index == turn.TurnIndex && (s.startedAt == 0 || turn.StartedAt == 0 || s.startedAt == turn.StartedAt)
}

// recordedTurnStop is one stop recorded against one of a thread's turns:
// the one record of who stopped it, which the requests that turn answers
// settle from (turnSettlement), whether a delivery bound them to the turn
// before the stop, at the turn's end, or not until after it.
type recordedTurnStop struct {
	// seq tells this record from any other made for the same turn index,
	// so a retirement removes only the records it read.
	seq   uint64
	turn  stoppedTurn
	cause threadtools.Cause
}

// recordTurnStop remembers who is stopping a turn, for the requests it
// answers. It is recorded before the interrupt is sent because the turn can
// settle while the interrupt is acknowledged. Only a thread with open
// requests records one. A thread keeps one per stopped turn, a later stop
// of the same turn replacing it, until a settlement pass that read it
// retires it: the end pass of a turn that had ended (endedTurnStops) or of
// the session it was sent to (sessionTurnStops), when nothing that pass
// read still needs it. It also goes with the thread's last open request
// (forgetReceiptRunning), so the records are bounded by the stopped turns
// whose end a request may still read.
func (a *App) recordTurnStop(threadID string, turn stoppedTurn, stop turnStop) {
	if turn.index < 0 {
		return
	}
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if len(a.threadRequests.running[threadID]) == 0 {
		return
	}
	if a.threadRequests.stops == nil {
		a.threadRequests.stops = make(map[string][]recordedTurnStop)
	}
	a.threadRequests.stopSeq++
	recorded := recordedTurnStop{seq: a.threadRequests.stopSeq, turn: turn, cause: stop.cause}
	stops := slices.DeleteFunc(a.threadRequests.stops[threadID], func(kept recordedTurnStop) bool {
		return kept.turn.index == turn.index
	})
	a.threadRequests.stops[threadID] = append(stops, recorded)
}

// dropTurnStop withdraws a stop whose interrupt was not delivered: the turn
// runs on, and a later end of it was not this stop's doing.
func (a *App) dropTurnStop(threadID string, turnIndex int) {
	a.forgetTurnStops(threadID, func(recorded recordedTurnStop) bool { return recorded.turn.index == turnIndex })
}

// forgetTurnStops drops the thread's stops gone reports true for.
func (a *App) forgetTurnStops(threadID string, gone func(recordedTurnStop) bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	stops := slices.DeleteFunc(a.threadRequests.stops[threadID], gone)
	if len(stops) == 0 {
		delete(a.threadRequests.stops, threadID)
		return
	}
	a.threadRequests.stops[threadID] = stops
}

// turnStopOf is the stop recorded against turn, and pins the stop to the
// turn's start the first time a row of it is read.
func (a *App) turnStopOf(threadID string, turn store.Turn) (recordedTurnStop, bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	for i, recorded := range a.threadRequests.stops[threadID] {
		if !recorded.turn.matches(turn) {
			continue
		}
		if recorded.turn.startedAt == 0 && turn.StartedAt != 0 {
			a.threadRequests.stops[threadID][i].turn.startedAt = turn.StartedAt
		}
		return recorded, true
	}
	return recordedTurnStop{}, false
}

// stoppedByPerson reports whether the user's own Stop cut turn.
func (a *App) stoppedByPerson(threadID string, turn store.Turn) bool {
	recorded, ok := a.turnStopOf(threadID, turn)
	return ok && recorded.cause.Code == threadtools.CauseUserStop
}

// endedTurnStops is the thread's stops whose turn is over, read before a
// turn-end pass reads its receipts, which the pass may retire once it has
// settled them (retireTurnStops): the turn row at the stop's index is
// complete, or is a later turn a revert gave the same index, or is gone
// after the stop saw it, or the session the stop was sent to is no longer
// the thread's. A stop whose turn has not opened yet in a live session, or
// is still running, is not among them.
func (a *App) endedTurnStops(threadID string) []recordedTurnStop {
	a.threadRequests.mu.Lock()
	stops := slices.Clone(a.threadRequests.stops[threadID])
	a.threadRequests.mu.Unlock()
	if len(stops) == 0 {
		return nil
	}
	live := ""
	if sess, ok := a.sessionManager().get(threadID); ok {
		live = sess.Token
	}
	return slices.DeleteFunc(stops, func(recorded recordedTurnStop) bool {
		if recorded.turn.session != "" && recorded.turn.session != live {
			return false
		}
		turn, found, err := a.store.GetTurnByThreadIndex(threadID, recorded.turn.index)
		if err != nil {
			log.Printf("thread tools: read the stopped turn %d of %s: %v", recorded.turn.index, threadID, err)
			return true
		}
		switch {
		case !found:
			return recorded.turn.startedAt == 0
		case !recorded.turn.matches(turn):
			return false
		default:
			return turn.CompletedAt == nil
		}
	})
}

// sessionTurnStops is the thread's stops sent to session, read before its
// end's pass reads the receipts.
func (a *App) sessionTurnStops(threadID, session string) []recordedTurnStop {
	if session == "" {
		return nil
	}
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	var stops []recordedTurnStop
	for _, recorded := range a.threadRequests.stops[threadID] {
		if recorded.turn.session == session {
			stops = append(stops, recorded)
		}
	}
	return stops
}

// retireTurnStops removes the records a settlement pass read and has no
// further use for, each by its own seq: a stop recorded since, for the same
// index or any other, stays.
func (a *App) retireTurnStops(threadID string, read []recordedTurnStop) {
	if len(read) == 0 {
		return
	}
	a.forgetTurnStops(threadID, func(recorded recordedTurnStop) bool {
		return slices.ContainsFunc(read, func(retired recordedTurnStop) bool { return retired.seq == recorded.seq })
	})
}
