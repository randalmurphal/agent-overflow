package app

import (
	"context"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// The `thread:updated` broadcast chokepoint.
//
// Every persisted thread-row mutation announces the row it changed, so a
// second attached client converges without a refresh — the RPC's return value
// only ever reaches the client that issued it. Three rules hold at every call
// site:
//
//   - One announcement per changed row. A tree deletion is several rows and
//     several frames; UpdateThreadBranch's workspace fan-out is the same shape.
//   - A write that changed nothing announces nothing. That is why the store
//     hands back a changed flag rather than a rows-affected count: SQLite
//     counts a row as affected when the assignment restates the value it held.
//   - A row frame is announced by thread id, never built from a row the
//     caller holds. announceThread reads the row under the thread's announce
//     lock and emits it before releasing, so frames leave in the order their
//     rows were read and the last one a client keeps is never older than the
//     last write (triage.ThreadAnnouncer). A row the caller held could predate
//     another writer's commit and arrive after that writer's frame. The frame
//     can therefore be newer than the row the initiating RPC returned; the
//     initiator converges on it. TestThreadRowFramesComeFromTheAnnouncer
//     rejects a row frame built anywhere else.

// threadAnnouncer is the process's one thread row announcer, shared with the
// triage router (newTriageRouter).
func (a *App) threadAnnouncer() *triage.ThreadAnnouncer {
	a.threadAnnouncerOnce.Do(func() {
		a.threadRowAnnouncer = triage.NewThreadAnnouncer(func(threadID string) (store.Thread, error) {
			return a.store.GetThread(threadID)
		})
	})
	return a.threadRowAnnouncer
}

// announceThread emits threadID's current row. Action is the receiver's
// instruction (triage.ThreadActionFull / Listed / Unlisted); see
// triage.ThreadUpdateEvent for the vocabulary. `listed` is for a write that
// admits the row to the active sidebar, which a client that never fetched
// the row can only learn from a listed frame; a full frame for a row it
// lacks is dropped.
func (a *App) announceThread(action, threadID string) {
	a.threadAnnouncer().Announce(a.emitEvent, action, threadID)
}

// announceThreadWithin is announceThread for a caller whose wait ctx bounds.
func (a *App) announceThreadWithin(ctx context.Context, action, threadID string, changed bool) {
	if changed {
		a.threadAnnouncer().AnnounceWithin(ctx, a.emitEvent, action, threadID)
	}
}

// announceThreadIfChanged is the guard every mutation binding routes
// through: the store's changed flag decides whether anything is said at all.
func (a *App) announceThreadIfChanged(action, threadID string, changed bool) {
	if !changed {
		return
	}
	a.announceThread(action, threadID)
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
