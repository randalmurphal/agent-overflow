package triage

import (
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// send_consumed.go names the turn that answers a send at the moment the
// provider consumes it.
//
// A send's row does not say where its answer runs. A message queued while a
// turn runs keeps its row in that turn, and the provider either drains it
// into that turn, picks it up afterwards as a turn of its own, or batches it
// at the boundary with a later message and answers both in that one's turn
// (claude_merge_fold.go). The confirmation is where the difference is
// visible, so that is where it is reported, once, to an owner that keeps it.

// SetSendConsumedHook registers the callback told which turn answers each
// send the provider consumes. It runs under the thread's flush anchor lock,
// inside the confirmation transaction: PendingSendTurn reports a send
// consumed only after the callback returned. The callback must not call back
// into the Router and must not block; store work is fine.
func (r *Router) SetSendConsumedHook(fn func(threadID string, sendIDs []string, answerTurn int)) {
	r.mu.Lock()
	r.sendConsumed = fn
	r.mu.Unlock()
}

// reportSendConsumed tells the hook which turn answers consumed. Caller holds
// the thread's flush anchor lock and has run the confirmation handlers.
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
	hook(threadID, sendIDs, r.answeringTurn(threadID, consumed))
}

// answeringTurn is the turn that answers a send consumed now: the open turn,
// which consumed it. With no turn open the provider took it at a turn
// boundary, and the turn that opens next answers it: the one the next
// waiting send reserved, or its own reservation when it is the last.
func (r *Router) answeringTurn(threadID string, consumed *pendingSend) int {
	if open, ok := r.openTurnIndex(threadID); ok {
		return open
	}
	if next, ok := r.turnOpeningPendingSend(threadID); ok {
		return next.TurnIndex
	}
	return consumed.TurnIndex
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
