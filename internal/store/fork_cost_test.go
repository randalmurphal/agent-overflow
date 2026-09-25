package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// What a source write that changes what its forks read costs: a revert
// moves the rows its readers show to a holder, a delete keeps them where
// they are. Neither reads or writes the rows before the revert point or the
// rows the readers keep reading, and the number of readers adds only their
// lineage rows.

// readersFixture is a store whose thread S holds rows ten a turn, every
// turn settled, and readers pointer forks of S made whole, so every reader
// shows every row. The rows are inserted directly: the fixture is about
// the forks' shape, not about how the rows were written.
func readersFixture(tb testing.TB, rows, readers int) *Store {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "store.sqlite")
	if err := copyTestStoreTemplate(path); err != nil {
		tb.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	if err := s.CreateThread(makeThread("S", "claude")); err != nil {
		tb.Fatal(err)
	}
	for _, statement := range []string{
		`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ? - 1)
		 INSERT INTO items (thread_id, id, turn_index, item_index, kind, role, status, summary, meta, created_at, updated_at)
		 SELECT 'S', 'r' || i, i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'row', '{}', 1, 1 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ? / 10 - 1)
		 INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at, stop_reason)
		 SELECT 'S:' || i, 'S', i, 1, 2, 'end_turn' FROM n`,
	} {
		if _, err := s.db.Exec(statement, rows); err != nil {
			tb.Fatal(err)
		}
	}
	for i := range readers {
		fork := makeThread(fmt.Sprintf("F%d", i), "claude")
		fork.ForkedFromThreadID = "S"
		if err := s.CreatePointerFork(fork, "S", ForkCut{}, testInterruptedSummary, 999); err != nil {
			tb.Fatal(err)
		}
	}
	return s
}

// writeCost is what one write cost the store: the statements it ran and
// the rows they and their triggers changed.
type writeCost struct {
	statements  int
	rowsChanged int64
}

// measureWrite runs write on s and returns its cost.
func measureWrite(t *testing.T, s *Store, write func() error) writeCost {
	t.Helper()
	rec := recordStatements(t, s)
	changes := func() int64 {
		t.Helper()
		var n int64
		if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := changes()
	var err error
	stmts := rec.capture(func() { err = write() })
	if err != nil {
		t.Fatal(err)
	}
	return writeCost{statements: len(stmts), rowsChanged: changes() - before}
}

// requireReadersRead checks every reader still shows rows rows.
func requireReadersRead(t *testing.T, s *Store, readers, rows int) {
	t.Helper()
	for i := range readers {
		if got := forkRows(t, s, fmt.Sprintf("F%d", i)); len(got) != rows {
			t.Fatalf("F%d reads %d rows, want %d", i, len(got), rows)
		}
	}
}

// TestSourceRevertCostsTheRowsItMoves: a revert of S below its readers'
// cuts moves the rows they show to one holder, one statement per table.
// Its cost does not depend on how many rows come before the revert point,
// and each further reader adds the same few lineage rows and no statement.
func TestSourceRevertCostsTheRowsItMoves(t *testing.T) {
	const moved = 20
	revert := func(prefix, readers int) writeCost {
		t.Helper()
		s := readersFixture(t, prefix+moved, readers)
		cost := measureWrite(t, s, func() error {
			_, _, err := s.DeleteConversationFromTurn("S", prefix/10)
			return err
		})
		var held int
		if err := s.db.QueryRow(`SELECT count(*) FROM items JOIN threads ON threads.id = items.thread_id WHERE threads.mode = 'holder'`).Scan(&held); err != nil || held != moved {
			t.Fatalf("holders hold %d rows, %v; want the %d moved", held, err, moved)
		}
		requireReadersRead(t, s, readers, prefix+moved)
		return cost
	}
	short, long := revert(10, 1), revert(1000, 1)
	if short != long {
		t.Errorf("a revert after 1000 rows costs %+v, after 10 %+v", long, short)
	}
	two, eight := revert(10, 2), revert(10, 8)
	if two.statements != short.statements || eight.statements != short.statements {
		t.Errorf("statements for 1, 2 and 8 readers: %d, %d, %d", short.statements, two.statements, eight.statements)
	}
	perReader := two.rowsChanged - short.rowsChanged
	if perReader <= 0 || perReader > 4 || eight.rowsChanged != short.rowsChanged+7*perReader {
		t.Errorf("rows changed for 1, 2 and 8 readers: %d, %d, %d; want a few lineage rows a reader",
			short.rowsChanged, two.rowsChanged, eight.rowsChanged)
	}
}

// TestSourceDeleteKeepsTheSharedRowsInPlace: the delete of a source its
// forks read makes it a holder in place. The rows the forks read are not
// touched, so the delete costs the same whatever their number; a further
// fork adds only its "forked from" link, which the delete clears as the
// row's delete would have.
func TestSourceDeleteKeepsTheSharedRowsInPlace(t *testing.T) {
	remove := func(rows, readers int) writeCost {
		t.Helper()
		s := readersFixture(t, rows, readers)
		cost := measureWrite(t, s, func() error { return s.DeleteThread("S") })
		requireReadersRead(t, s, readers, rows)
		if held, err := s.GetThread("S"); err != nil || held.Mode != "holder" {
			t.Fatalf("the deleted source = %+v, %v; want a holder", held, err)
		}
		return cost
	}
	short, long := remove(20, 1), remove(2000, 1)
	if short != long {
		t.Errorf("the delete of 2000 shared rows costs %+v, of 20 %+v", long, short)
	}
	eight, eightLong := remove(20, 8), remove(2000, 8)
	if eightLong != eight {
		t.Errorf("with 8 readers the delete of 2000 shared rows costs %+v, of 20 %+v", eightLong, eight)
	}
	if eight.statements != short.statements || eight.rowsChanged != short.rowsChanged+7 {
		t.Errorf("the delete with 8 readers costs %+v, with 1 %+v; want one link row a further reader", eight, short)
	}
}

// BenchmarkSourceRevertWithReaders: a revert that moves 15,000 rows every
// reader shows to a holder, with one reader and with eight, beside the
// same revert with no reader, which deletes them.
func BenchmarkSourceRevertWithReaders(b *testing.B) {
	const rows = 15_000
	for _, readers := range []int{0, 1, 8} {
		b.Run(fmt.Sprintf("readers=%d", readers), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				s := readersFixture(b, rows, readers)
				b.StartTimer()
				if _, _, err := s.DeleteConversationFromTurn("S", 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
