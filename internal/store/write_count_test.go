package store

import (
	"database/sql"
	"errors"
	"runtime"
	"strings"
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

// ObserveReads sees each read ReadCount counts on the goroutine that reads,
// so the reading stack is there to attribute it, and nothing after its stop;
// a stop that ran late does not end the observer set after it.
func TestObserveReadsSeesEachCountedReadOnTheReadingGoroutine(t *testing.T) {
	s := newTestStore(t)
	var seen int
	var onReadingStack bool
	read := func() {
		if _, _, err := s.ThreadHistoryStamp("none"); err != nil {
			t.Fatal(err)
		}
	}
	stop := s.ObserveReads(func() {
		seen++
		onReadingStack = callerOnStack("store.TestObserveReadsSeesEachCountedReadOnTheReadingGoroutine.func1")
	})
	before := s.ReadCount()
	read()
	if counted := s.ReadCount() - before; counted != 1 || seen != 1 || !onReadingStack {
		t.Fatalf("observed %d reads (on the reading stack: %v), ReadCount counted %d", seen, onReadingStack, counted)
	}
	stop()
	read()
	if seen != 1 {
		t.Fatalf("observed %d reads, want the stop to end the observer", seen)
	}

	var newer int
	stopNewer := s.ObserveReads(func() { newer++ })
	stop()
	read()
	stopNewer()
	if newer != 1 {
		t.Fatalf("the newer observer saw %d reads, want an earlier observer's stop to leave it", newer)
	}
}

// callerOnStack reports whether fn, a function name with its package's last
// path element, is on the calling goroutine's stack.
func callerOnStack(fn string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, "/"+fn) {
			return true
		}
		if !more {
			return false
		}
	}
}
