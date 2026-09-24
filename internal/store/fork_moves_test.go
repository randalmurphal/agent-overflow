package store

import (
	"errors"
	"slices"
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

	// The delete detaches in its own transaction before it drains the
	// source, and again in the one that deletes the row, for a fork made
	// meanwhile; that one detaches and marks F again (detachForkDescendantsTx
	// finds a fork by its source), which moves both stamps again.
	before = stamps()
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	requireReports("deleting the source", before, []string{"F", "G"}, []string{"F", "G"})
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
	// The delete's second detach (see TestForkMovesReportTheForksAWriteMoved)
	// finds the forks made from the source and marks their dividers again;
	// K no longer reads through the source by then.
	if got, want := reports.take(), [][]string{{"F", "H", "K", "P", "Q", "R"}, {"F", "H", "P", "Q", "R"}}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("deleting the source reported %v, want %v", got, want)
	}
	for id, was := range before {
		if now := historyStampOf(t, s, id); now.Rev <= was.Rev {
			t.Fatalf("deleting the source left %s's stamp at %+v", id, now)
		}
	}
}
