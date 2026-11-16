package app

import (
	"database/sql"
	"errors"
	"fmt"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
)

// forkSource reads the thread a fork is taken from, under its action lock.
// A thread whose delete has begun reads as gone (GetOwnedThread), whether
// the delete finished, failed or is waiting to be completed at boot, and
// the fork says so (store.ErrForkSourceDeleted). The store refuses the
// same fork for a caller that holds no lock.
func (a *App) forkSource(op, sourceThreadID string) (store.Thread, error) {
	source, err := a.store.GetOwnedThread(sourceThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Thread{}, forkRefusal(fmt.Errorf("%s %s: %w", op, sourceThreadID, store.ErrForkSourceDeleted))
	}
	if err != nil {
		return store.Thread{}, fmt.Errorf("%s: %w", op, err)
	}
	return source, nil
}

// checkForkSource is CheckMutable for a fork's source. A holder, the
// history of a deleted thread its forks still read, and a thread whose
// delete committed are refused as a deleted source, as forkSource refuses
// them. CheckMutable reports a holder as store.ErrThreadGone, and a thread
// a delete claimed or committed as store.ErrThreadDeleting: a claimed one
// is refused as being deleted, since its delete may yet be refused.
func (a *App) checkForkSource(op, sourceThreadID string) error {
	err := a.threadApplication().CheckMutable(sourceThreadID)
	deleted := errors.Is(err, store.ErrThreadGone)
	if !deleted && errors.Is(err, store.ErrThreadDeleting) {
		committed, readErr := a.store.ThreadDeleteCommitted(sourceThreadID)
		if readErr != nil {
			return fmt.Errorf("%s %s: %w", op, sourceThreadID, readErr)
		}
		deleted = committed
	}
	if deleted {
		return forkRefusal(fmt.Errorf("%s %s: %w", op, sourceThreadID, store.ErrForkSourceDeleted))
	}
	return err
}

// forkRefusal gives a fork refused because its source was deleted the
// sentence every client shows, on every origin; other errors pass through.
func forkRefusal(err error) error {
	if !errors.Is(err, store.ErrForkSourceDeleted) {
		return err
	}
	return errorsx.Public("fork_source_deleted", "This thread was deleted, so it cannot be forked.", err)
}

// ensureThreadCanFork rejects forks against threads that have no
// messages or where atTurnIndex points outside the existing turn range.
func (a *App) ensureThreadCanFork(source store.Thread, atTurnIndex *int) error {
	return a.threadApplication().EnsureCanFork(source, atTurnIndex)
}
