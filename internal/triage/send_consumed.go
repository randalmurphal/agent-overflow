package triage

import (
	"log"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// send_consumed.go names the turn that answers a send once the provider has
// consumed it.
//
// A send's row does not say where its answer runs. A message queued while a
// turn runs keeps its row in that turn, and the provider either drains it
// into that turn, picks it up afterwards as a turn of its own, or takes it
// at the boundary with later messages and answers them all in one turn
// (claude_merge_fold.go). The confirmation is where the difference is
// visible, so that is where it is reported, once, to an owner that keeps it.
// Nothing here predicts a turn: a send whose turn has not opened waits for
// the turn start that names it.

// boundarySend is a send consumed with no turn to name yet.
type boundarySend struct {
	aoItemID string
	sendIDs  []string
}

// SetSendConsumedHook registers the callback told which turn answers each
// send the provider consumes. PendingSendTurn reports a send consumed only
// after the callback returned for it. A call made at a confirmation runs
// under the thread's flush anchor lock; a call made when a turn opens does
// not. The callback must not call back into the Router and must not block;
// store work is fine.
func (r *Router) SetSendConsumedHook(fn func(threadID string, sendIDs []string, answerTurn int)) {
	r.mu.Lock()
	r.sendConsumed = fn
	r.mu.Unlock()
}

// reportSendConsumed tells the hook which turn answers consumed, or leaves
// the send for the next turn that opens. Caller holds the thread's flush
// anchor lock and has run the confirmation handlers.
//
// A retried confirmation keeps the turn its first attempt named and reports
// it again with the row's current send ids: a fold that succeeds only on
// the retry has widened the row with sends the first report did not carry.
func (r *Router) reportSendConsumed(threadID string, consumed *pendingSend) {
	r.mu.Lock()
	hook := r.sendConsumed
	r.mu.Unlock()
	if hook == nil {
		return
	}
	sendIDs, err := r.consumedSendIDs(threadID, consumed)
	if err != nil {
		log.Printf("triage: send ids of consumed send %s/%s: %v", threadID, consumed.AOItemID, err)
		return
	}
	if len(sendIDs) == 0 {
		return
	}
	r.mu.Lock()
	if !consumed.AnswerTurnSet {
		st := r.state(threadID)
		if listed := st.boundarySendIndex(consumed.AOItemID); listed >= 0 {
			// Still waiting for its turn: the turn start reports the
			// ids the row carries now.
			st.boundarySends[listed].sendIDs = sendIDs
			r.mu.Unlock()
			return
		}
		turn, known := r.answeringTurnLocked(threadID, consumed)
		if !known {
			st.boundarySends = append(st.boundarySends, boundarySend{aoItemID: consumed.AOItemID, sendIDs: sendIDs})
			r.mu.Unlock()
			return
		}
		consumed.AnswerTurn, consumed.AnswerTurnSet = turn, true
	}
	turn := consumed.AnswerTurn
	r.mu.Unlock()
	hook(threadID, sendIDs, turn)
}

func (st *threadState) boundarySendIndex(aoItemID string) int {
	return slices.IndexFunc(st.boundarySends, func(send boundarySend) bool { return send.aoItemID == aoItemID })
}

// answeringTurnLocked is the turn that answers a send confirmed now, in the
// order the evidence is worth:
//
//   - the provider opened a turn for the send (a turn start took it after
//     its `started`): that turn, however late the confirmation;
//   - its own turn is open: the consumption opened it;
//   - `started` arrived inside a turn: the model read it there;
//   - a turn is open: it consumed the send;
//   - a turn start took the send and nothing is open: that turn;
//   - none of these: the provider took it at a turn boundary, and the turn
//     that opens next answers (reportBoundarySends).
//
// Caller holds r.mu.
func (r *Router) answeringTurnLocked(threadID string, consumed *pendingSend) (int, bool) {
	if consumed.OpenedTurn && consumed.StartedAtClaim {
		return consumed.TurnIndex, true
	}
	st := r.threadStateIfPresent(threadID)
	open, isOpen := 0, false
	if st != nil && st.openTurnSet {
		open, isOpen = st.openTurn, true
	}
	switch {
	case isOpen && open == consumed.TurnIndex:
		return open, true
	case consumed.StartedTurnSet:
		return consumed.StartedTurn, true
	case isOpen:
		return open, true
	case consumed.OpenedTurn:
		return consumed.TurnIndex, true
	}
	return 0, false
}

// reportBoundarySends tells the hook that turnIndex, which has just opened
// and whose row is open in the store, answers every send consumed at the
// boundary before it. A send leaves boundarySends only after the hook
// returned for it, so PendingSendTurn never reports it consumed with its
// turn unwritten. An entry still registered (a failed write put it back for
// a retry) is told the turn, so the retry reports the same one.
func (r *Router) reportBoundarySends(threadID string, turnIndex int) {
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	if st == nil || len(st.boundarySends) == 0 {
		r.mu.Unlock()
		return
	}
	hook := r.sendConsumed
	sends := slices.Clone(st.boundarySends)
	r.mu.Unlock()
	if hook != nil {
		for _, send := range sends {
			hook(threadID, send.sendIDs, turnIndex)
		}
	}
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		// Appends happen only on the provider event worker, which is
		// where this runs, so the reported sends are still the head.
		st.boundarySends = slices.Delete(st.boundarySends, 0, min(len(sends), len(st.boundarySends)))
		if len(st.boundarySends) == 0 {
			st.boundarySends = nil
		}
		for i := range st.pendingSends {
			if slices.ContainsFunc(sends, func(send boundarySend) bool { return send.aoItemID == st.pendingSends[i].AOItemID }) {
				st.pendingSends[i].AnswerTurn, st.pendingSends[i].AnswerTurnSet = turnIndex, true
			}
		}
	}
	r.mu.Unlock()
}

// consumedSendIDs reads the send ids of the consumed send's row. The stored
// row is read first because a queue-boundary fold has already widened it
// with the ids of every message it absorbed; the entry's retained copy
// answers for a row that is not stored.
func (r *Router) consumedSendIDs(threadID string, consumed *pendingSend) ([]string, error) {
	row, found, err := r.store.GetThreadItem(threadID, consumed.AOItemID)
	if err != nil {
		return nil, err
	}
	if !found {
		switch {
		case consumed.DeferredItem != nil:
			row = *consumed.DeferredItem
		case consumed.QuietItem != nil:
			row = *consumed.QuietItem
		default:
			return nil, nil
		}
	}
	return rowSendIDs(row)
}

func rowSendIDs(row store.Item) ([]string, error) {
	meta, err := usermessage.FromItem(row)
	if err != nil {
		return nil, err
	}
	if len(meta.JoinedSendIDs) > 0 {
		return meta.JoinedSendIDs, nil
	}
	if meta.SendID == "" {
		return nil, nil
	}
	return []string{meta.SendID}, nil
}
