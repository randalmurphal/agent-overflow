package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// readSnapshot keeps selection, hydration, and decoration on one WAL snapshot.
func readSnapshot[T any](db *sql.DB, label string, read func(sqlQueryer) (T, error)) (value T, err error) {
	return readSnapshotContext(context.Background(), db, label, read)
}

func readSnapshotContext[T any](ctx context.Context, db *sql.DB, label string, read func(sqlQueryer) (T, error)) (value T, err error) {
	if ctx == nil {
		return value, fmt.Errorf("store: %s requires a context", label)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		// A deadline that fires while BEGIN runs interrupts it, and the
		// driver reports the interruption rather than the context's error.
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(ctxErr, err)
		}
		return value, fmt.Errorf("store: begin %s: %w", label, err)
	}
	defer func() {
		if closeErr := tx.Rollback(); closeErr != nil && !errors.Is(closeErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("store: close %s: %w", label, closeErr))
		}
	}()
	return read(contextReadTx{Tx: tx, ctx: ctx})
}

// historyReadSnapshot is readSnapshotContext on the read pool for a read
// whose duration grows with a thread's history. It waits for one of
// historyReadSlots before it takes a connection, and picks the pool after
// the wait, which keeps the wait out of the window quiesceReads cannot
// see.
func historyReadSnapshot[T any](ctx context.Context, s *Store, label string, read func(sqlQueryer) (T, error)) (value T, err error) {
	if ctx == nil {
		return value, fmt.Errorf("store: %s requires a context", label)
	}
	release, err := s.historyReads.acquire(ctx)
	if err != nil {
		return value, fmt.Errorf("store: wait for a history read slot for %s: %w", label, err)
	}
	defer release()
	return readSnapshotContext(ctx, s.reader(), label, read)
}

// readSlots is a counting semaphore of historyReadSlots slots. The zero
// value is ready to use.
type readSlots struct {
	once  sync.Once
	slots chan struct{}
}

func (r *readSlots) acquire(ctx context.Context) (func(), error) {
	r.once.Do(func() { r.slots = make(chan struct{}, historyReadSlots) })
	select {
	case r.slots <- struct{}{}:
		return func() { <-r.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
