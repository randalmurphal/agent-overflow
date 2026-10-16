package triage

import (
	"context"
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
)

// ThreadAnnouncer builds every thread:updated frame.
//
// Clients keep the last row they receive and apply a patch over it, so no
// frame may leave after a frame that describes a later state. A writer that
// emitted on its own could break that whatever lock it took: a row it read
// before another writer's commit, or a patch it sent while another frame
// carrying the row from before that patch was still on its way. Every frame
// therefore leaves through one per-thread lock, held from the read (for a
// row frame) or after the commit (for a patch or a deletion) until the emit.
// Frames then leave in the order their rows were read, a row frame read
// before a patch's commit leaves before that patch, and the last row frame
// for a thread is read after the last write that announced. The lock is a
// leaf: nothing else is taken while it is held, so a caller may hold any
// other lock.
//
// One announcer serves the whole process (the App's, shared with the
// Router), because two would order frames only among their own callers.
type ThreadAnnouncer struct {
	read  func(threadID string) (store.Thread, error)
	locks *keyedlock.Registry
}

// NewThreadAnnouncer returns an announcer that reads rows with read.
func NewThreadAnnouncer(read func(threadID string) (store.Thread, error)) *ThreadAnnouncer {
	return &ThreadAnnouncer{read: read, locks: keyedlock.New()}
}

// Announce emits threadID's current row with action (ThreadActionFull,
// Listed or Unlisted). A read failure is logged: the write that prompted the
// frame already succeeded, and the client converges on its next list read.
func (an *ThreadAnnouncer) Announce(emit func(eventchan.Channel, any), action, threadID string) {
	an.AnnounceWithin(context.Background(), emit, action, threadID)
}

// AnnounceWithin is Announce for a caller whose wait for the lock ctx bounds:
// a ctx that ends first emits nothing, logged. Callers pass a context that
// ends only at shutdown, when no client is left to read the frame.
func (an *ThreadAnnouncer) AnnounceWithin(ctx context.Context, emit func(eventchan.Channel, any), action, threadID string) {
	if threadID == "" {
		return
	}
	release, err := an.locks.LockCtx(ctx, threadID)
	if err != nil {
		log.Printf("announce thread %s: %v", threadID, err)
		return
	}
	defer release()
	row, err := an.read(threadID)
	if err != nil {
		log.Printf("announce thread %s: %v", threadID, err)
		return
	}
	emit(eventchan.ThreadUpdated, ThreadUpdateEvent{Action: action, Thread: &row})
}

// AnnounceHandOff emits threadID's current row as `unlisted` with HandOffTo
// naming successorID: an assistant's old thread leaving the list because
// the assistant moved to successorID, so a client moves the panes showing
// it instead of closing them. Announce after the successor's `listed`
// frame, so a client has the row it moves to.
func (an *ThreadAnnouncer) AnnounceHandOff(emit func(eventchan.Channel, any), threadID, successorID string) {
	if threadID == "" {
		return
	}
	defer an.locks.Lock(threadID)()
	row, err := an.read(threadID)
	if err != nil {
		log.Printf("announce thread %s handoff: %v", threadID, err)
		return
	}
	emit(eventchan.ThreadUpdated, ThreadUpdateEvent{Action: ThreadActionUnlisted, Thread: &row, HandOffTo: successorID})
}

// AnnouncePatch emits the named fields of a write the caller has committed,
// as a `patch` frame. Call it after the commit: a row frame read before the
// commit then leaves before the patch, and one read after it carries the
// patched fields already. The Title, Model and SessionRef it names carry
// the stored values, read under the lock, not the caller's: two writers of
// one field can announce in the opposite order to their commits, and the
// last frame must carry the last write. patch carries no row; its Action
// and ID are set here.
func (an *ThreadAnnouncer) AnnouncePatch(emit func(eventchan.Channel, any), threadID string, patch ThreadUpdateEvent) {
	if threadID == "" {
		return
	}
	patch.Action = ThreadActionPatch
	patch.ID = threadID
	patch.Thread = nil
	defer an.locks.Lock(threadID)()
	if patch.Title != nil || patch.Model != nil || patch.SessionRef != nil {
		row, err := an.read(threadID)
		if err != nil {
			log.Printf("announce thread %s patch: %v", threadID, err)
			return
		}
		if patch.Title != nil {
			patch.Title = &row.Title
		}
		if patch.Model != nil {
			patch.Model = &row.Model
		}
		if patch.SessionRef != nil {
			patch.SessionRef = &row.SessionRef
		}
	}
	emit(eventchan.ThreadUpdated, patch)
}

// AnnounceDeleted emits the id-only `deleted` frame for a row the caller has
// deleted. A row frame read before the delete leaves before it; one that
// reads after it finds no row and emits nothing.
func (an *ThreadAnnouncer) AnnounceDeleted(emit func(eventchan.Channel, any), threadID string) {
	if threadID == "" {
		return
	}
	defer an.locks.Lock(threadID)()
	emit(eventchan.ThreadUpdated, ThreadUpdateEvent{Action: ThreadActionDeleted, ID: threadID})
}

// Hold takes threadID's announce lock and returns its release. Tests use it
// to keep an announcement waiting while a later write lands.
func (an *ThreadAnnouncer) Hold(threadID string) func() {
	return an.locks.Lock(threadID)
}

// Refs is how many holders and waiters threadID's announce lock has.
func (an *ThreadAnnouncer) Refs(threadID string) int {
	return an.locks.Refs(threadID)
}
