package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		return value, fmt.Errorf("store: begin %s: %w", label, err)
	}
	defer func() {
		if closeErr := tx.Rollback(); closeErr != nil && !errors.Is(closeErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("store: close %s: %w", label, closeErr))
		}
	}()
	return read(contextReadTx{Tx: tx, ctx: ctx})
}
