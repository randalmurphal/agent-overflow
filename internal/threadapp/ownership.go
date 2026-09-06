package threadapp

import (
	"context"
	"database/sql"
	"errors"

	"agent-overflow/internal/store"
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

// CheckCleanup permits local cache cleanup after the destination has confirmed
// a move. It never grants execution or ordinary edits, and never releases a
// pending handoff's recovery data. Call under the thread action lock.
func (s *Service) CheckCleanup(threadID string) (retired bool, err error) {
	err = s.CheckMutable(threadID)
	if err == nil {
		return false, nil
	}
	var moved *store.ThreadTransferError
	if !errors.As(err, &moved) || !moved.Moved {
		return false, err
	}
	database, dbErr := s.database("clean transferred conversation")
	if dbErr != nil {
		return false, dbErr
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
// (for example, deleting an absent draft is a successful no-op).
func (s *Service) CheckMutable(threadID string) error {
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
	return database.CheckThreadExecutionAccess(thread)
}
