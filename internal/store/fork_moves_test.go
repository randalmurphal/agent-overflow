package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// forkMoveReports records what a store reports through OnForkStampsMoved.
type forkMoveReports struct{ got [][]string }

func watchForkMoves(s *Store) *forkMoveReports {
	r := &forkMoveReports{}
	s.OnForkStampsMoved(func(ids []string) { r.got = append(r.got, slices.Clone(ids)) })
	return r
}

// take returns the reports since the last take.
func (r *forkMoveReports) take() [][]string {
	got := r.got
	r.got = nil
	return got
}

// TestForkMovesReportTheForksAWriteMoved: a committed write reports each
// fork whose stamps it moved, once, in one report per transaction, and
// only those. A write below a fork's cut reports the forks that took a
// copy; a write past every cut and a write to a thread no fork reads
// report nothing; a write that rolls back reports nothing and leaves
// nothing recorded.
func TestForkMovesReportTheForksAWriteMoved(t *testing.T) {
	s := forkHandOffFixture(t)
	seedLinearSource(t, s, "N", 2)
	reports := watchForkMoves(s)
	stamps := func() map[string]HistoryStamp {
		return map[string]HistoryStamp{"F": historyStampOf(t, s, "F"), "G": historyStampOf(t, s, "G")}
	}
	requireReports := func(what string, before map[string]HistoryStamp, want ...[]string) {
		t.Helper()
		got := reports.take()
		if !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("%s reported %v, want %v", what, got, want)
		}
		var moved []string
		for _, ids := range want {
			moved = append(moved, ids...)
		}
		for id, was := range before {
			if now := historyStampOf(t, s, id); (now != was) != slices.Contains(moved, id) {
				t.Fatalf("%s: %s's stamp %+v -> %+v, reported %v", what, id, was, now, got)
			}
		}
		if n := forkMoves.pending.Load(); n != 0 {
			t.Fatalf("%s left %d transactions recorded", what, n)
		}
	}

	before := stamps()
	if err := s.UpdateItemMeta("S", "u0", `{"changed":true}`); err != nil {
		t.Fatal(err)
	}
	// G read u0 through F, and reads F's copy of it now: the same row, so
	// its stamps stay.
	requireReports("a write below the cut", before, []string{"F"})

	before = stamps()
	if _, err := appendCarded(s, Item{ID: "late", ThreadID: "S", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "late", Meta: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateItemMeta("S", "late", `{"changed":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThreadItem("S", "late"); err != nil {
		t.Fatal(err)
	}
	requireReports("writes past every cut", before)

	before = stamps()
	if err := s.UpdateItemMeta("N", "u0", `{"changed":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThreadItem("N", "a1"); err != nil {
		t.Fatal(err)
	}
	requireReports("writes to a thread no fork reads", before)

	// A move whose meta transform fails does so after the hand-off, so the
	// copy rolls back with the write.
	mustPointerFork(t, s, "N", "M", ForkCut{})
	reports.take()
	was := historyStampOf(t, s, "M")
	refused := errors.New("refused")
	if _, err := s.BumpItemToTurnEnd("N", "u1", func(string) (string, error) { return "", refused }, 5); !errors.Is(err, refused) {
		t.Fatalf("a move whose transform fails = %v", err)
	}
	if now := historyStampOf(t, s, "M"); now != was {
		t.Fatalf("a rolled-back hand-off moved M's stamp %+v -> %+v", was, now)
	}
	requireReports("a write that rolls back", nil)

	before = stamps()
	if err := s.UpdatePayloadSpans("S", "p", `{"preview":1}`, `{"full":1}`); err != nil {
		t.Fatal(err)
	}
	requireReports("spans on a payload the forks show", before, []string{"F", "G"})

	before = stamps()
	touchItemForTest(t, s, "S", "a1")
	requireReports("a revision touch of a row the forks show", before, []string{"F", "G"})

	before = stamps()
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireReports("deleting the source", before, []string{"F", "G"})
}

// TestForkMovesCostNoStatement: a write records the forks it moved from
// what it reads anyway. A revision touch names them in its own statement,
// so it runs the same statements whether forks show the row or no fork
// reads the thread.
func TestForkMovesCostNoStatement(t *testing.T) {
	s := forkHandOffFixture(t)
	seedLinearSource(t, s, "N", 2)
	reports := watchForkMoves(s)
	rec := recordStatements(t, s)
	queries := func(fn func()) []string {
		var out []string
		for _, stmt := range rec.capture(fn) {
			out = append(out, stmt.query)
		}
		return out
	}
	alone := queries(func() { touchItemForTest(t, s, "N", "a1") })
	if got := reports.take(); len(got) != 0 {
		t.Fatalf("a touch on a thread no fork reads reported %v", got)
	}
	shown := queries(func() { touchItemForTest(t, s, "S", "a1") })
	if got := reports.take(); !slices.EqualFunc(got, [][]string{{"F", "G"}}, slices.Equal) {
		t.Fatalf("a touch of a row two forks show reported %v", got)
	}
	if len(alone) != 1 || !slices.Equal(alone, shown) {
		t.Fatalf("touch statements without forks %q, with forks %q", alone, shown)
	}
}

// TestDetachReportsEveryThreadItsMarkMoves: deleting a source reports each
// thread whose stamps the detach moves, each reached one way only:
//   - K, a fork of the tail fork P cut before P's divider, reads the
//     source's rows through P and shows no marked divider: it is detached;
//   - Q, a materialized fork of the materialized fork H, holds a copy of
//     H's divider and reads nothing through the source: the mark moves it;
//   - R, a pointer fork of H, shows H's divider in place: the mark's
//     trigger moves it.
func TestDetachReportsEveryThreadItsMarkMoves(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 2)
	mustPointerFork(t, s, "S", "H", ForkCut{})
	if err := s.MaterializeForkHistory(t.Context(), "H"); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "H", "R", ForkCut{})
	mustPointerFork(t, s, "H", "Q", ForkCut{})
	if err := s.MaterializeForkHistory(t.Context(), "Q"); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", throughTurn(0))
	mustPointerFork(t, s, "S", "P", ForkCut{})
	mustPointerFork(t, s, "P", "K", throughTurn(0))
	reports := watchForkMoves(s)
	before := map[string]HistoryStamp{}
	for _, id := range []string{"F", "H", "K", "P", "Q", "R"} {
		before[id] = historyStampOf(t, s, id)
	}
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	if got, want := reports.take(), [][]string{{"F", "H", "K", "P", "Q", "R"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("deleting the source reported %v, want %v", got, want)
	}
	for id, was := range before {
		if now := historyStampOf(t, s, id); now.Rev <= was.Rev {
			t.Fatalf("deleting the source left %s's stamp at %+v", id, now)
		}
	}
}

// TestPacedDeleteReportsEachForkOnce: a paced delete detaches the source's
// forks before it drains the rows, and again in the transaction that
// deletes the thread, for a fork made meanwhile. The second detach leaves
// the forks the first one detached alone: it neither marks their dividers
// nor moves their stamps, so each fork is reported once. F is a pointer
// fork, G reads the source through F, H is materialized, and N is made
// during the drain.
func TestPacedDeleteReportsEachForkOnce(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 1199)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		SELECT 'S', 'r' || i, i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'row', '{}', 1, 1 FROM n`)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	mustPointerFork(t, s, "S", "H", ForkCut{})
	if err := s.MaterializeForkHistory(t.Context(), "H"); err != nil {
		t.Fatal(err)
	}
	reports := watchForkMoves(s)
	type dividerRow struct {
		rev  int64
		meta string
	}
	dividers := func() map[string]dividerRow {
		out := map[string]dividerRow{}
		for _, id := range []string{"F", "G", "H"} {
			var row dividerRow
			if err := s.db.QueryRow(`SELECT rev, meta FROM items WHERE thread_id = ? AND id = ?`, id, forkDividerID(id)).Scan(&row.rev, &row.meta); err != nil {
				t.Fatalf("divider of %s: %v", id, err)
			}
			out[id] = row
		}
		return out
	}
	var detached map[string]HistoryStamp
	var marked map[string]dividerRow
	pauses := 0
	if err := s.DeleteThreadPaced("S", func() {
		pauses++
		if pauses > 1 {
			return
		}
		if got, want := reports.take(), [][]string{{"F", "G", "H"}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("the detach before the drain reported %v, want %v", got, want)
		}
		detached = map[string]HistoryStamp{}
		for _, id := range []string{"F", "G", "H"} {
			detached[id] = historyStampOf(t, s, id)
		}
		marked = dividers()
		mustPointerFork(t, s, "S", "N", ForkCut{})
	}); err != nil {
		t.Fatal(err)
	}
	if pauses == 0 {
		t.Fatal("the source drained in one chunk; the fixture must span several")
	}
	if got, want := reports.take(), [][]string{{"N"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("the delete's final detach reported %v, want %v", got, want)
	}
	for id, was := range detached {
		if now := historyStampOf(t, s, id); now != was {
			t.Errorf("the final detach moved %s's stamp %+v -> %+v", id, was, now)
		}
	}
	for id, was := range dividers() {
		if was != marked[id] {
			t.Errorf("the final detach rewrote %s's divider %+v -> %+v", id, marked[id], was)
		}
	}
	if _, origin := forkDivider(t, s, "N"); !origin.SourceDeleted {
		t.Fatalf("N divider = %+v", origin)
	}
	if rows := forkRows(t, s, "N"); len(rows) != 0 {
		t.Fatalf("N reads %d rows of the deleted source", len(rows))
	}
}

// TestDetachWithoutForksRunsNoWrite: deleting a thread no fork was made
// from probes for forks and writes nothing for them.
func TestDetachWithoutForksRunsNoWrite(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	rec := recordStatements(t, s)
	for _, stmt := range rec.capture(func() {
		if err := s.DeleteThread("S"); err != nil {
			t.Fatal(err)
		}
	}) {
		if strings.Contains(stmt.query, "fork_source_title") || strings.Contains(stmt.query, forkDividerToolName) {
			t.Errorf("deleting a thread without forks ran %q", stmt.query)
		}
	}
}
