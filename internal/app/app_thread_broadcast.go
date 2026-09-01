package app

import (
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The `thread:updated` broadcast chokepoint.
//
// Every persisted thread-row mutation broadcasts the row it changed, so a
// second attached client converges without a refresh — the RPC's return value
// only ever reaches the client that issued it. Three rules hold at every call
// site:
//
//   - One emit per changed row. A tree deletion is several rows and several
//     emits; UpdateThreadBranch's workspace fan-out is the same shape.
//   - A write that changed nothing emits nothing. That is why the store hands
//     back a changed flag rather than a rows-affected count: SQLite counts a
//     row as affected when the assignment restates the value it held.
//   - The row broadcast is the row the RPC returns. The initiator may apply
//     its own result optimistically and then receive its own echo; because
//     both carry the same post-write row, the echo converges to the state the
//     optimistic apply already reached instead of flickering through a
//     different one.

// broadcastThreadRow emits one changed row. Action is the receiver's
// instruction (triage.ThreadActionFull / Listed / Unlisted); see
// triage.ThreadUpdateEvent for the vocabulary.
func (a *App) broadcastThreadRow(action string, row store.Thread) {
	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{Action: action, Thread: &row})
}

// broadcastThreadRowIfChanged is the guard every mutation binding routes
// through: the store's changed flag decides whether anything is said at all.
func (a *App) broadcastThreadRowIfChanged(action string, row store.Thread, changed bool) {
	if !changed {
		return
	}
	a.broadcastThreadRow(action, row)
}

// broadcastThreadRowByID re-reads the row and broadcasts it as `full`. For
// writes that move one of the DERIVED sidebar columns threadColumns computes
// (hasActionableProposedPlan, hasIncompleteTurn) rather than a threads column
// the caller already holds — the proposed-plan writes, which change no field
// of the row the RPC returned. Log-and-continue: the write already succeeded
// and the sidebar converges on the next ListThreads.
func (a *App) broadcastThreadRowByID(threadID string) {
	if threadID == "" {
		return
	}
	row, err := a.store.GetThread(threadID)
	if err != nil {
		log.Printf("broadcast thread row %s: %v", threadID, err)
		return
	}
	a.broadcastThreadRow(triage.ThreadActionFull, row)
}

// broadcastThreadDeleted announces a row that no longer exists in SQLite. It
// carries the id alone: there is no row left to send, and a client holding
// one drops it plus its caches.
func (a *App) broadcastThreadDeleted(threadID string) {
	a.emitEvent(eventchan.ThreadUpdated, triage.ThreadUpdateEvent{
		Action: triage.ThreadActionDeleted,
		ID:     threadID,
	})
}
