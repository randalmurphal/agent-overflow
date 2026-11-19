package triage

// A Stop that lands after a send reached the provider and before the turn
// it opens reached the router (SampleInterrupt's waiting send) cannot know
// yet what it cut. The provider resolves the interrupt one of three ways:
// it aborts the turn the send opens, the send runs to a normal end because
// the interrupt reached an idle provider, or the provider retires the send
// unrun. MarkInterrupt therefore holds the stop until that turn settles:
// a turn that ends interrupted gets the stop row and its " — stopped"
// flips, a turn that ends normally and a turn that never runs get nothing.

// waitingStop is a Stop held for the turn of the send it cut.
type waitingStop struct {
	turn    int
	summary string
}

// settledTurnOutcome is how a turn settlement ended, kept for the latest
// settlement so a Stop marked after its turn settled still resolves.
type settledTurnOutcome struct {
	turn int
	// cut reports a turn that ended interrupted.
	cut bool
	// soft reports a soft round close: the turn's provider result is still
	// to come and settles it again (the late fold).
	soft bool
}

// waitingStopHold is what holdWaitingStop decided.
type waitingStopHold int

const (
	// stopCutNothing: the turn ended normally or will never run.
	stopCutNothing waitingStopHold = iota
	// stopHeld: the turn has not settled; its settlement resolves the stop.
	stopHeld
	// stopOwed: the turn already settled interrupted; the stop is written now.
	stopOwed
	// stopSettledEarlier: the turn settled before the latest settlement, so
	// its outcome is read from its turns row.
	stopSettledEarlier
)

// holdWaitingStop holds a Stop of the waiting send's turn until the turn
// settles. A Stop repeated before then holds nothing more.
func (r *Router) holdWaitingStop(threadID string, turnIndex int, summary string) waitingStopHold {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return stopCutNothing
	}
	if st.settledTurns[turnIndex] {
		if !st.lastSettledSet || st.lastSettled.turn != turnIndex {
			return stopSettledEarlier
		}
		switch {
		case st.lastSettled.cut:
			return stopOwed
		case !st.lastSettled.soft:
			return stopCutNothing
		}
		// A soft close: the turn's result is still to come.
	} else if (!st.openTurnSet || st.openTurn != turnIndex) && st.turnStarting == nil && !st.hasPendingSendForTurn(turnIndex) {
		// The send left without opening its turn during the interrupt's
		// ack wait: the provider retired it unrun.
		return stopCutNothing
	}
	if st.waitingStopSet && st.waitingStop.turn == turnIndex {
		return stopHeld
	}
	st.waitingStop, st.waitingStopSet = waitingStop{turn: turnIndex, summary: summary}, true
	return stopHeld
}

// hasPendingSendForTurn reports whether a pending send names turnIndex.
func (st *threadState) hasPendingSendForTurn(turnIndex int) bool {
	for _, entry := range st.pendingSends {
		if entry.TurnIndex == turnIndex {
			return true
		}
	}
	return false
}

// dropWaitingStop withdraws a held Stop of turnIndex: a Stop of the open
// turn wrote the row itself, or the turn's send was retired unrun.
func (r *Router) dropWaitingStop(threadID string, turnIndex int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		st.dropWaitingStopLocked(turnIndex)
	}
}

func (st *threadState) dropWaitingStopLocked(turnIndex int) {
	if st.waitingStopSet && st.waitingStop.turn == turnIndex {
		st.waitingStop, st.waitingStopSet = waitingStop{}, false
	}
}

// settleWaitingStopLocked records how turnIndex's settlement ended and
// resolves a Stop held for it. It returns the stop when the turn was cut,
// so the settlement writes its row; a turn that ended normally drops the
// stop, and a soft close keeps it for the result still to come. Every
// settlement calls it under r.mu in the step that claims the settlement,
// so a Stop marked concurrently either is held before it or reads its
// outcome after it. Caller holds r.mu.
func (st *threadState) settleWaitingStopLocked(turnIndex int, cut, soft bool) (waitingStop, bool) {
	st.lastSettled, st.lastSettledSet = settledTurnOutcome{turn: turnIndex, cut: cut, soft: soft}, true
	if !st.waitingStopSet || st.waitingStop.turn != turnIndex || (soft && !cut) {
		return waitingStop{}, false
	}
	stop := st.waitingStop
	st.waitingStop, st.waitingStopSet = waitingStop{}, false
	return stop, cut
}

// settleWaitingStop is settleWaitingStopLocked for a settlement that did not
// claim the turn itself (the late fold of a soft-closed turn's result). A
// retired session's frame resolves nothing: lastSettled is process state, and
// the turn index is the replacement's.
func (r *Router) settleWaitingStop(threadID string, turnIndex int, sessionToken string, cut, soft bool) (waitingStop, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frameRetiredLocked(threadID, sessionToken) {
		return waitingStop{}, false
	}
	return r.state(threadID).settleWaitingStopLocked(turnIndex, cut, soft)
}

// stopRowClaim is the newest turn whose stop row a session wrote (set), and
// the thread epoch it was claimed in.
type stopRowClaim struct {
	turn  int
	set   bool
	epoch uint64
}

// writeStopRow writes turnIndex's stop row reading summary unless the turn
// has one. A turn can be cut more than once before it reports its end (a
// Stop pressed twice, a rest that interrupts the request's turn and then
// the session's), and its stop can be resolved by MarkInterrupt or by its
// settlement; whichever writer claims the turn first writes its row and the
// rest write nothing. Turns of one session only move forward, so the claim
// is a high-water mark: a turn at or before the newest stopped one gets no
// row. A failed write gives the claim back, so a later Stop can still
// record it. It returns "" with no error when the turn's row exists.
func (r *Router) writeStopRow(threadID string, turnIndex int, summary string, now int64) (string, error) {
	prev, claimed := r.claimStopRow(threadID, turnIndex)
	if !claimed {
		return "", nil
	}
	id, err := r.persistTurnError(threadID, turnIndex, summary, now)
	if err != nil {
		r.releaseStopRow(threadID, turnIndex, prev)
		return "", err
	}
	return id, nil
}

// claimStopRow claims turnIndex's stop row, returning the claim it
// replaced, or false when the turn or a later one has its row.
func (r *Router) claimStopRow(threadID string, turnIndex int) (stopRowClaim, bool) {
	id := r.identity(threadID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if id.stopRow.set && id.stopRow.turn >= turnIndex {
		return stopRowClaim{}, false
	}
	prev := id.stopRow
	id.stopRow = stopRowClaim{turn: turnIndex, set: true, epoch: id.epoch}
	return prev, true
}

// releaseStopRow restores the claim turnIndex's failed write replaced,
// unless a session commit or another claim has replaced it since.
func (r *Router) releaseStopRow(threadID string, turnIndex int, prev stopRowClaim) {
	id := r.identity(threadID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if id.stopRow == (stopRowClaim{turn: turnIndex, set: true, epoch: id.epoch}) {
		id.stopRow = prev
	}
}

// settledTurnWasCut reads from its turns row whether a turn that settled
// before the latest settlement ended interrupted.
func (r *Router) settledTurnWasCut(threadID string, turnIndex int) (bool, error) {
	turn, found, err := r.store.GetTurnByThreadIndex(threadID, turnIndex)
	if err != nil || !found {
		return false, err
	}
	return turn.StopReason == "interrupted", nil
}
