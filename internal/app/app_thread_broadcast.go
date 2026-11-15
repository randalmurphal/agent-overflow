package app

import (
	"log"

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
//     are emitted under the same lock after their commit. Frames therefore
//     leave in the order their rows were read, and no frame describing an
//     earlier state follows one describing a later state.
//     TestThreadUpdatedFramesComeFromTheAnnouncer (internal/triage) rejects
//     any other emit.
//   - An RPC that returns a row announces it before returning, and the
//     transport delivers a frame emitted before a reply ahead of that reply
//     on the caller's connection (internal/transport writeReply). A frame
//     that reaches the client after a call started is therefore one written
//     during or after the call, and the client keeps it over the reply
//     (frontend/src/lib/stores/threadRowFrames.ts).

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

// announceListing announces a write that moved the thread across the
// sidebar listing rule (store.ListingChange): `listed` when it put the
// thread on the list, `emptied` when it took a never-sent thread off. A
// screen whose catalog read came before the write learns of it only here.
// Reports whether it announced.
func (a *App) announceListing(threadID string, listing store.ListingChange) bool {
	switch listing {
	case store.ListingListed:
		a.announceThread(triage.ThreadActionListed, threadID)
	case store.ListingUnlisted:
		a.announceThread(triage.ThreadActionEmptied, threadID)
	default:
		return false
	}
	return true
}

// listedAction is the frame for a write that may have put threadID on the
// sidebar (creation, unarchive): `listed` when the row now meets the
// listing rule (store.ThreadListed), `full` when it is a never-sent thread
// with nothing that makes it real, which the write that does will list. A
// failed read answers `listed`: a row shown early is corrected by the next
// catalog read, a real one hidden is not.
func (a *App) listedAction(threadID string) string {
	listed, err := a.store.ThreadListed(threadID)
	if err != nil {
		log.Printf("thread %s: read its listing: %v", threadID, err)
		return triage.ThreadActionListed
	}
	if listed {
		return triage.ThreadActionListed
	}
	return triage.ThreadActionFull
}

// writeAppRow runs write, which persists a row the app posts on its own (a
// notice, an error) and may be a never-sent thread's first item, and
// announces `listed` when it put the thread on the list. The send path
// announces the first item it writes itself. A failed write is checked too:
// a refused one can leave an error row behind.
func (a *App) writeAppRow(threadID string, write func() error) error {
	if a.store == nil {
		return write()
	}
	before, err := a.store.ThreadListed(threadID)
	if err != nil {
		log.Printf("thread %s: read its listing: %v", threadID, err)
	}
	writeErr := write()
	if err == nil && before {
		return writeErr
	}
	after, err := a.store.ThreadListed(threadID)
	if err != nil {
		log.Printf("thread %s: read its listing: %v", threadID, err)
		return writeErr
	}
	if after {
		a.announceThread(triage.ThreadActionListed, threadID)
	}
	return writeErr
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

// writeSessionRef stores a thread's resume cursor and announces it when it
// moved. The sidebar's fork affordance gates on it, and a later session
// init that reports the same cursor finds nothing changed to announce.
func (a *App) writeSessionRef(threadID, ref string) error {
	changed, err := a.store.UpdateSessionRef(threadID, ref)
	if err != nil {
		return err
	}
	if changed {
		a.announceThreadPatch(threadID, triage.ThreadUpdateEvent{SessionRef: &ref})
	}
	return nil
}

// broadcastThreadDeleted announces a row that no longer exists in SQLite. It
// carries the id alone: there is no row left to send, and a client holding
// one drops it plus its caches.
func (a *App) broadcastThreadDeleted(threadID string) {
	a.threadAnnouncer().AnnounceDeleted(a.emitEvent, threadID)
}
