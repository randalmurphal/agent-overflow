package threadapp

import (
	"context"
	"database/sql"
	"errors"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// LockMutable serializes ordinary edits with transfer reservation. It is
// separate from the action lock: composer saves and queue registration must
// remain available while a send/revert holds the action lock. Reservation takes
// action then mutation; an ordinary mutation must never wait for an action lock.
// A workflow already holding the action lock uses CheckMutable instead.
func (s *Service) LockMutable(ctx context.Context, threadID string) (func(), error) {
	unlock, err := s.mutations.LockCtx(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if err := s.CheckMutable(threadID); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// LockDeliverable is LockMutable for the queue doors of a message the app
// writes for a thread: its queue write and its withdrawal. A thread a delete
// claimed takes it, since a queued message starts nothing by itself, and
// one whose delete committed refuses it (store.CheckThreadDeliverable).
func (s *Service) LockDeliverable(ctx context.Context, threadID string) (func(), error) {
	unlock, err := s.mutations.LockCtx(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if err := s.checkMutable(threadID, (*store.Store).CheckThreadDeliverable); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// LockMutations takes the mutation lock without CheckMutable, for a
// session teardown: dropping the queue of a thread that has moved, is in
// transfer or is a holder must still serialize with queue writes, and
// CheckMutable refuses those threads.
func (s *Service) LockMutations(threadID string) func() {
	return s.mutations.Lock(threadID)
}

// CheckCleanup permits local cache cleanup after the destination has confirmed
// a move, and the delete of a holder: the history a deleted thread's pointer
// forks read, which nothing runs in and every other operation refuses
// (store.CheckThreadExecutionAccess). retired reports either, and a thread
// whose delete committed, which its delete tears down: the thread has no
// provider state here for anyone else to clean. A thread a delete claimed
// and has not committed is refused, as CheckMutable refuses it. It never
// grants execution or ordinary edits, and never releases a pending
// handoff's recovery data. Call under the thread action lock.
func (s *Service) CheckCleanup(threadID string) (retired bool, err error) {
	return s.checkCleanup(threadID, true, true)
}

// CheckDelete is the check a family delete runs on a thread it claimed or
// committed: CheckCleanup's when cleanup, which admits a moved thread's
// cache and a holder as retired, else CheckMutable's, with the delete's own
// claim and commit admitted (store.CheckThreadDeleteAccess). Call under the
// thread action lock.
func (s *Service) CheckDelete(threadID string, cleanup bool) (retired bool, err error) {
	return s.checkCleanup(threadID, cleanup, false)
}

// checkCleanup is CheckCleanup, or with cleanup false CheckMutable, with
// fenced choosing whether a delete's claim refuses the thread.
func (s *Service) checkCleanup(threadID string, cleanup, fenced bool) (retired bool, err error) {
	database, err := s.database("clean up conversation")
	if err != nil {
		return false, err
	}
	thread, err := database.GetThread(threadID)
	if err == nil && cleanup && thread.Mode == threadmode.ModeHolder {
		return true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && cleanup && fenced {
		_, committed, err := database.ThreadDeleteClaim(threadID)
		if err != nil {
			return false, err
		}
		if committed {
			return true, nil
		}
	}
	access := (*store.Store).CheckThreadExecutionAccess
	if !fenced {
		access = (*store.Store).CheckThreadDeleteAccess
	}
	err = s.checkMutable(threadID, access)
	if err == nil {
		return false, nil
	}
	if !cleanup {
		return false, err
	}
	var moved *store.ThreadTransferError
	if !errors.As(err, &moved) || !moved.Moved {
		return false, err
	}
	row, readErr := database.GetThreadTransfer(moved.OperationID)
	if readErr != nil {
		return false, readErr
	}
	if row.ThreadID != threadID || row.Direction != "outgoing" || row.Kind != "move" || row.Phase != "complete" {
		return false, err
	}
	return true, nil
}

// CheckMutable must run under the action or mutation lock before changing a
// thread, its native session, or its files. Tombstones are checked even when the display
// row is missing. Otherwise missing rows retain each operation's own semantics
// (for example, deleting an absent draft is a successful no-op). A thread a
// delete claimed or committed is refused (store.ErrThreadDeleting).
func (s *Service) CheckMutable(threadID string) error {
	return s.checkMutable(threadID, (*store.Store).CheckThreadExecutionAccess)
}

// checkMutable is CheckMutable with access the check of a thread whose row
// is present: how far a delete may have gone with it is access's to say.
func (s *Service) checkMutable(threadID string, access func(*store.Store, store.Thread) error) error {
	database, err := s.database("change conversation")
	if err != nil {
		return err
	}
	thread, err := database.GetThread(threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return database.CheckThreadTransferAccess(threadID)
	}
	if err != nil {
		return err
	}
	// Pending forks only read the parent's native identity. Execution will
	// materialize their own; CheckThreadExecutionAccess owns that distinction.
	return access(database, thread)
}
