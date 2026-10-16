package triage

import (
	"context"
	"log"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
)

// ThreadAnnouncer builds every thread:updated frame that carries a row.
//
// Clients keep the last row they receive, so a frame must never carry a row
// older than one already sent. A writer that broadcast the row it held could
// break that whatever lock it took: its row is a read from before another
// writer's commit, and nothing ordered its emit after that writer's. The
// announcer takes the thread id instead and reads the row while holding a
// per-thread lock that it also holds for the emit, so frames leave in the
// order their rows were read, and the last frame for a thread is read after
// the last write that announced. The lock is a leaf: nothing else is taken
// while it is held, so a caller may hold any other lock.
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

// AnnounceWithin is Announce for a caller whose wait is bounded: a ctx that
// ends while the frame waits for the announce lock drops the frame, logged.
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

// Hold takes threadID's announce lock and returns its release. Tests use it
// to keep an announcement waiting while a later write lands.
func (an *ThreadAnnouncer) Hold(threadID string) func() {
	return an.locks.Lock(threadID)
}

// Refs is how many holders and waiters threadID's announce lock has.
func (an *ThreadAnnouncer) Refs(threadID string) int {
	return an.locks.Refs(threadID)
}
