package app

import (
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
//   - Every frame leaves through triage.ThreadAnnouncer. A row frame is
//     announced by thread id and read under the thread's announce lock; a
//     patch (announceThreadPatch) and a deletion (broadcastThreadDeleted)
//     are emitted under the same lock after their commit, and so is an
//     assistant handoff's `unlisted` frame naming the successor
//     (ThreadAnnouncer.AnnounceHandOff), read like any row. Frames therefore
//     leave in the order their rows were read, and no frame describing an
//     earlier state follows one describing a later state.
//     TestThreadUpdatedFramesComeFromTheAnnouncer (internal/triage) rejects
//     any other emit.
//   - An RPC that returns a row returns one read before its own frame
//     leaves, and the two reach the client as separate frames in either
//     order. The client therefore applies a returned row only when no frame
//     for that thread arrived after the call started
//     (frontend/src/lib/stores/threadRowFrames.ts); otherwise the frame is
//     the newer state.

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

// announceThreadIfChanged is the guard every mutation binding routes
// through: the store's changed flag decides whether anything is said at all.
func (a *App) announceThreadIfChanged(action, threadID string, changed bool) {
	if !changed {
		return
	}
	a.announceThread(action, threadID)
}

// announceThreadPatch emits the named fields of a committed write
// (triage.ThreadAnnouncer.AnnouncePatch).
func (a *App) announceThreadPatch(threadID string, patch triage.ThreadUpdateEvent) {
	a.threadAnnouncer().AnnouncePatch(a.emitEvent, threadID, patch)
}

// broadcastThreadDeleted announces a row that no longer exists in SQLite. It
// carries the id alone: there is no row left to send, and a client holding
// one drops it plus its caches.
func (a *App) broadcastThreadDeleted(threadID string) {
	a.threadAnnouncer().AnnounceDeleted(a.emitEvent, threadID)
}
