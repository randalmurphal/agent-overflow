package store

import (
	"database/sql"
	"errors"

	"modernc.org/sqlite"
)

// IsDatabaseFailure reports whether err is the database itself failing: an
// SQLite error (a locked file, a constraint, a full disk) or a connection or
// transaction that has already finished. Such an error's text can quote a
// statement or a row and describes nothing its caller did, so code that
// reports failures to someone who can only fix their own request withholds
// it.
func IsDatabaseFailure(err error) bool {
	var engine *sqlite.Error
	return errors.As(err, &engine) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone)
}
