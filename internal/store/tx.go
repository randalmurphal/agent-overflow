package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// rollbackTx rolls back a transaction its function is leaving and joins a
// rollback that failed to that function's error. It is deferred after Begin
// with the function's named error; after a commit it does nothing.
func rollbackTx(tx *sql.Tx, action string, result *error) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		*result = errors.Join(*result, fmt.Errorf("%s: rollback: %w", action, err))
	}
}
