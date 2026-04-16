package store

import (
	"database/sql"
	"fmt"
)

func requireRowsAffected(result sql.Result, action string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", action, err)
	}
	if rows == 0 {
		return fmt.Errorf("%s: %w", action, sql.ErrNoRows)
	}
	return nil
}
