package store

import (
	"database/sql"
	"errors"
	"testing"
)

// TestWriteCountCountsEachRowWriteStatement pins what WriteCount counts:
// each run of a statement that writes rows on the writer, however it
// reaches the driver (a direct call, a cached statement, a statement
// database/sql prepared, a RETURNING query), and nothing else.
func TestWriteCountCountsEachRowWriteStatement(t *testing.T) {
	s := newTestStore(t)
	exec := func(query string) func() error {
		return func() error {
			_, err := s.db.Exec(query)
			return err
		}
	}
	query := func(db *sql.DB, query string) func() error {
		return func() error {
			rows, err := db.Query(query)
			if err != nil {
				return err
			}
			for rows.Next() {
			}
			return errors.Join(rows.Err(), rows.Close())
		}
	}
	steps := []struct {
		name string
		run  func() error
		want uint64
	}{
		{"insert", exec(`INSERT INTO ui_state (scope, key, value, updated_at) VALUES ('wc', 'a', '1', 1)`), 1},
		{"the same insert text again, cached", exec(`INSERT INTO ui_state (scope, key, value, updated_at) VALUES ('wc', 'b', '1', 1)`), 1},
		{"update that matches no row", exec(`UPDATE ui_state SET value = '2' WHERE scope = 'none'`), 1},
		{"executed WITH", exec(`WITH k(v) AS (SELECT 'c') INSERT INTO ui_state (scope, key, value, updated_at) SELECT 'wc', v, '1', 1 FROM k`), 1},
		{"DELETE RETURNING run as a query", query(s.db, `DELETE FROM ui_state WHERE scope = 'wc' AND key = 'c' RETURNING key`), 1},
		{"read on the writer", query(s.db, `SELECT COUNT(*) FROM ui_state`), 0},
		{"WITH read on the writer", query(s.db, `WITH k(v) AS (SELECT 1) SELECT v FROM k`), 0},
		{"read on the read pool", query(s.reader(), `SELECT COUNT(*) FROM ui_state`), 0},
		{"pragma", exec(`PRAGMA wal_checkpoint(PASSIVE)`), 0},
		{"prepared statement run twice", func() error {
			tx, err := s.db.Begin()
			if err != nil {
				return err
			}
			stmt, err := tx.Prepare(`DELETE FROM ui_state WHERE scope = ? AND key = ?`)
			if err != nil {
				return errors.Join(err, tx.Rollback())
			}
			for _, key := range []string{"a", "b"} {
				if _, err := stmt.Exec("wc", key); err != nil {
					return errors.Join(err, stmt.Close(), tx.Rollback())
				}
			}
			if err := stmt.Close(); err != nil {
				return errors.Join(err, tx.Rollback())
			}
			return tx.Commit()
		}, 2},
	}
	for _, step := range steps {
		before := s.WriteCount()
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := s.WriteCount() - before; got != step.want {
			t.Errorf("%s counted %d writes, want %d", step.name, got, step.want)
		}
	}
}
