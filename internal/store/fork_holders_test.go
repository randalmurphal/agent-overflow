package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/threadmode"
)

// forkChainFixture is S with two turns and a payload row, a fork F of it
// and a fork G of F, both cut after S's last row (1:6).
func forkChainFixture(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 2)
	if err := insertWithPayloadCarded(s,
		Item{ID: "tool", ThreadID: "S", TurnIndex: 1, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: "p", Meta: "{}"},
		Payload{ID: "p", Kind: "text", Meta: "{}", Data: []byte("output")},
	); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	return s
}

// holderIDs lists the holders, oldest first.
func holderIDs(t *testing.T, s *Store) []string {
	t.Helper()
	ids, err := queryIDs(s.db, `SELECT id FROM threads WHERE mode = ? ORDER BY rowid`, threadmode.ModeHolder)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// holderOf is the nearest holder reader reads.
func holderOf(t *testing.T, s *Store, reader string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT l.ancestor_id FROM thread_fork_lineage l
		JOIN threads h ON h.id = l.ancestor_id AND h.mode = ?
		WHERE l.thread_id = ? ORDER BY l.depth LIMIT 1`, threadmode.ModeHolder, reader).Scan(&id); err != nil {
		t.Fatalf("holder %s reads: %v", reader, err)
	}
	return id
}

// releasedHolders lists the holders no fork reads any more, sorted.
func releasedHolders(t *testing.T, s *Store) []string {
	t.Helper()
	ids, err := s.ListReleasedHolders()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}

// ownIDs lists the rows a thread stores itself, in timeline order.
func ownIDs(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	ids, err := queryIDs(s.db, `SELECT id FROM items WHERE thread_id = ? ORDER BY turn_index, item_index`, threadID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestSourceRevertGivesShownRowsToAHolder: a source that deletes or reverts
// rows its forks show moves those rows, once, to a new holder that every
// fork reads in its place with the fork's own cut. The forks read exactly
// what they read before, and no fork's stamp moves, so no client is told
// anything; the holder is hidden and keeps the source's title.
func TestSourceRevertGivesShownRowsToAHolder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		write      func(*Store) error
		held       []string
		sourceKept string
	}{
		{"delete row", func(s *Store) error { return s.DeleteThreadItem("S", "a0") }, []string{"a0"}, "1:6"},
		{"revert turns", func(s *Store) error { _, _, err := s.DeleteConversationFromTurn("S", 1); return err }, []string{"u1", "a1", "tool"}, "1:-2147483648"},
		{"revert from item", func(s *Store) error { _, _, err := s.DeleteConversationFromItem("S", "a0"); return err }, []string{"a0", "u1", "a1", "tool"}, "0:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := forkChainFixture(t)
			source, fork, grandchild := ownIDs(t, s, "S"), timelineShape(t, s, "F"), timelineShape(t, s, "G")
			stamps := map[string]HistoryStamp{"F": historyStampOf(t, s, "F"), "G": historyStampOf(t, s, "G")}
			released := 0
			s.OnHoldersReleased(func() { released++ })
			if err := tc.write(s); err != nil {
				t.Fatal(err)
			}
			requireShape(t, s, "F", fork)
			requireShape(t, s, "G", grandchild)
			for id, was := range stamps {
				if now := historyStampOf(t, s, id); now != was {
					t.Fatalf("%s's stamp moved %+v -> %+v", id, was, now)
				}
			}
			var kept []string
			for _, id := range source {
				if !slices.Contains(tc.held, id) {
					kept = append(kept, id)
				}
			}
			requireIDs(t, "S rows", ownIDs(t, s, "S"), kept)
			holders := holderIDs(t, s)
			if len(holders) != 1 {
				t.Fatalf("holders = %v, want one", holders)
			}
			h := holders[0]
			requireIDs(t, "holder rows", ownIDs(t, s, h), tc.held)
			requireIDs(t, "F lineage", forkLineage(t, s, "F"), []string{"1:" + h + ":1:6", "2:S:" + tc.sourceKept})
			requireIDs(t, "G lineage", forkLineage(t, s, "G"), []string{"1:F:1:6", "2:" + h + ":1:6", "3:S:" + tc.sourceKept})
			var title, origin string
			var deleting bool
			var project *string
			if err := s.db.QueryRow(`SELECT title, fork_source_thread_id, deleting, project_id FROM threads WHERE id = ?`, h).Scan(&title, &origin, &deleting, &project); err != nil {
				t.Fatal(err)
			}
			if title != "Thread S" || origin != "S" || deleting || project != nil {
				t.Fatalf("holder = title %q source %q deleting %v project %v", title, origin, deleting, project)
			}
			if _, err := s.GetOwnedThread(h); err == nil {
				t.Fatal("a holder reads as an owned thread")
			}
			if released != 0 {
				t.Fatalf("the split reported %d releases; no lineage row went", released)
			}
		})
	}
}

// TestSourceRevertReachesADeeperReader: a fork that lowered its cut no
// longer reads the rows past it, but a fork made from it earlier still does
// through its own lineage, so the source's delete gives them to a holder
// for that fork alone.
func TestSourceRevertReachesADeeperReader(t *testing.T) {
	s := forkChainFixture(t)
	grandchild := timelineShape(t, s, "G")
	if _, _, err := s.DeleteConversationFromTurn("F", 1); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
	requireShape(t, s, "G", grandchild)
	if err := s.DeleteThreadItem("S", "a1"); err != nil {
		t.Fatal(err)
	}
	requireShape(t, s, "G", grandchild)
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), []string{"1:S:0:2"})
	h := holderOf(t, s, "G")
	requireIDs(t, "holder rows", ownIDs(t, s, h), []string{"a1"})
	// A rewrite of a row only the deeper fork shows gives it a copy in the
	// holder it already reads right before the source.
	if err := s.UpdateItemMeta("S", "u1", `{"changed":true}`); err != nil {
		t.Fatalf("a rewrite of a row the deeper fork shows: %v", err)
	}
	requireShape(t, s, "G", grandchild)
	requireIDs(t, "holders", holderIDs(t, s), []string{h})
	requireIDs(t, "holder rows", ownIDs(t, s, h), []string{"u1", "a1"})
	if u1, found, err := s.GetThreadItem("S", "u1"); err != nil || !found || u1.Meta != `{"changed":true}` {
		t.Fatalf("the source's u1 = %+v found=%v, %v", u1, found, err)
	}
}

// TestForkHideKeepsTheRowForItsForks: a fork's delete of a row it inherits
// hides it, and a hide applies to every level behind the fork, so the
// forks made from it would lose the row too. A holder takes a copy for
// them first, which they read in the fork's place; the source's row is
// then shown by no thread and may change.
func TestForkHideKeepsTheRowForItsForks(t *testing.T) {
	s := forkChainFixture(t)
	grandchild := timelineShape(t, s, "G")
	stamp := historyStampOf(t, s, "G")
	if err := s.DeleteThreadItem("F", "tool"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0", "u1", "a1"})
	requireShape(t, s, "G", grandchild)
	if now := historyStampOf(t, s, "G"); now != stamp {
		t.Fatalf("G's stamp moved %+v -> %+v", stamp, now)
	}
	h := holderOf(t, s, "G")
	requireIDs(t, "holder rows", ownIDs(t, s, h), []string{"tool"})
	requireIDs(t, "G lineage", forkLineage(t, s, "G"), []string{"1:" + h + ":1:6", "2:F:1:6", "3:S:1:6"})
	if err := s.UpdateItemMeta("S", "tool", `{"changed":true}`); err != nil {
		t.Fatalf("a rewrite of the row no fork shows any more: %v", err)
	}
	if err := s.AppendPayloadData("S", "p", []byte(" more"), "{}", 5); err != nil {
		t.Fatalf("a rewrite of the payload no fork shows any more: %v", err)
	}
	requireShape(t, s, "G", grandchild)
	if data, err := s.GetPayloadData("G", "p"); err != nil || string(data) != "output" {
		t.Fatalf("G reads payload %q, %v", data, err)
	}
	if err := s.DeleteThread("G"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "released", releasedHolders(t, s), []string{h})
}

// TestSourceRevertMovesTurnRowsWithItems: the turn rows a fork reads
// through the source go to the holder with the rows: a turn the source
// reverts moves, and one it keeps part of, and may settle again, is copied.
// The fork owns the rows of its cut turn and reads the same turns.
func TestSourceRevertMovesTurnRowsWithItems(t *testing.T) {
	for _, tc := range []struct {
		name        string
		revert      func(*Store) error
		sourceTurns []string
		// held names the holder's turn rows; "H" stands for the holder.
		held []string
		// next is the source's next turn, which takes its id again.
		next int
	}{
		{"revert turns", func(s *Store) error { _, _, err := s.DeleteConversationFromTurn("S", 1); return err },
			[]string{"0:10:100"}, []string{"H:1", "H:2"}, 1},
		{"revert from item", func(s *Store) error { _, _, err := s.DeleteConversationFromItem("S", "a1"); return err },
			nil, []string{"H:1", "H:2"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedLinearSource(t, s, "S", 4)
			for turn := range 4 {
				id := fmt.Sprintf("S:%d", turn)
				if err := s.InsertTurn(Turn{TurnID: id, ThreadID: "S", TurnIndex: turn, StartedAt: int64(10 + turn)}); err != nil {
					t.Fatal(err)
				}
				if err := s.UpdateTurnCompleted(id, int64(100+turn), "end_turn", "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			mustPointerFork(t, s, "S", "F", ForkCut{})
			turns := func(thread string) []string {
				t.Helper()
				ids, err := queryIDs(s.db, `SELECT turn_index || ':' || started_at || ':' || completed_at FROM timeline_turns WHERE thread_id = ? ORDER BY turn_index`, thread)
				if err != nil {
					t.Fatal(err)
				}
				return ids
			}
			fork := turns("F")
			if err := tc.revert(s); err != nil {
				t.Fatal(err)
			}
			requireIDs(t, "F turns", turns("F"), fork)
			h := holderOf(t, s, "F")
			if tc.sourceTurns != nil {
				requireIDs(t, "S turns", turns("S"), tc.sourceTurns)
			}
			ids, err := queryIDs(s.db, `SELECT replace(turn_id, thread_id, 'H') FROM turns WHERE thread_id = ? ORDER BY turn_index`, h)
			if err != nil {
				t.Fatal(err)
			}
			next := fmt.Sprintf("S:%d", tc.next)
			if err := s.InsertTurn(Turn{TurnID: next, ThreadID: "S", TurnIndex: tc.next, StartedAt: 50}); err != nil {
				t.Fatalf("the source's next turn: %v", err)
			}
			if turn, found, err := s.GetTurn(next); err != nil || !found || turn.ThreadID != "S" {
				t.Fatalf("turn %s = %+v found=%v, %v; want the source's", next, turn, found, err)
			}
			requireIDs(t, "F turns after the source's next turn", turns("F"), fork)
			requireIDs(t, "holder turns", ids, tc.held)
		})
	}
}

// TestChainOfHolders: successive reverts give each a holder, which forks cut
// at different places read in order; a fork made through them reads the
// same; the last reader's delete releases every holder, and deleting them
// leaves nothing.
func TestChainOfHolders(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 4)
	mustPointerFork(t, s, "S", "F1", throughTurn(3))
	mustPointerFork(t, s, "S", "F2", throughTurn(1))
	shapes := map[string][]string{"F1": timelineShape(t, s, "F1"), "F2": timelineShape(t, s, "F2")}
	for _, turn := range []int{3, 1} {
		if _, _, err := s.DeleteConversationFromTurn("S", turn); err != nil {
			t.Fatal(err)
		}
		for id, shape := range shapes {
			requireShape(t, s, id, shape)
		}
	}
	holders := holderIDs(t, s)
	if len(holders) != 2 {
		t.Fatalf("holders = %v, want one per revert", holders)
	}
	h1, h2 := holders[0], holders[1]
	requireIDs(t, "F1 lineage", forkLineage(t, s, "F1"), []string{"1:" + h1 + ":3:2", "2:" + h2 + ":3:-2147483648", "3:S:1:-2147483648"})
	requireIDs(t, "F2 lineage", forkLineage(t, s, "F2"), []string{"1:" + h2 + ":1:2", "2:S:1:-2147483648"})
	requireIDs(t, "first holder rows", ownIDs(t, s, h1), []string{"u3", "a3"})
	requireIDs(t, "second holder rows", ownIDs(t, s, h2), []string{"u1", "a1", "u2", "a2"})
	mustPointerFork(t, s, "F1", "G", ForkCut{})
	requireShape(t, s, "G", shapes["F1"])

	var released []string
	s.OnHoldersReleased(func() {
		ids, err := s.ListReleasedHolders()
		if err != nil {
			t.Error(err)
		}
		released = ids
	})
	for _, id := range []string{"S", "F2", "G"} {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
		requireShape(t, s, "F1", shapes["F1"])
	}
	if len(released) != 0 {
		t.Fatalf("holders released while F1 reads them: %v", released)
	}
	if err := s.DeleteThread("F1"); err != nil {
		t.Fatal(err)
	}
	slices.Sort(released)
	want := []string{h1, h2, "S"}
	slices.Sort(want)
	requireIDs(t, "released holders", released, want)
	for _, id := range released {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
	}
	var threads, rows, lineage int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM threads), (SELECT count(*) FROM items), (SELECT count(*) FROM thread_fork_lineage)`).Scan(&threads, &rows, &lineage); err != nil {
		t.Fatal(err)
	}
	if threads != 0 || rows != 0 || lineage != 0 {
		t.Fatalf("left %d threads, %d rows, %d lineage rows", threads, rows, lineage)
	}
}

// TestSplitIsAtomic: a split that fails at any step leaves every thread,
// row and lineage row as it was, and the same write succeeds once the
// failure is gone.
func TestSplitIsAtomic(t *testing.T) {
	revert := func(s *Store) error { _, _, err := s.DeleteConversationFromTurn("S", 1); return err }
	for name, tc := range map[string]struct {
		trigger string
		write   func(*Store) error
	}{
		"row move":  {`CREATE TRIGGER fail_split BEFORE UPDATE OF thread_id ON items BEGIN SELECT RAISE(ABORT, 'injected'); END`, revert},
		"last hide": {`CREATE TRIGGER fail_split BEFORE INSERT ON thread_fork_hidden BEGIN SELECT RAISE(ABORT, 'injected'); END`, func(s *Store) error { return s.DeleteThreadItem("S", "a0") }},
		"lineage":   {`CREATE TRIGGER fail_split BEFORE INSERT ON thread_fork_lineage WHEN NEW.thread_id = 'G' BEGIN SELECT RAISE(ABORT, 'injected'); END`, revert},
	} {
		t.Run(name, func(t *testing.T) {
			s := forkChainFixture(t)
			type state struct {
				shapes   map[string][]string
				own      map[string][]string
				lineage  map[string][]string
				stamps   map[string]HistoryStamp
				holders  []string
				hidden   int
				payloads int
			}
			read := func() state {
				st := state{shapes: map[string][]string{}, own: map[string][]string{}, lineage: map[string][]string{}, stamps: map[string]HistoryStamp{}}
				for _, id := range []string{"S", "F", "G"} {
					st.shapes[id] = timelineShape(t, s, id)
					st.own[id] = ownIDs(t, s, id)
					st.lineage[id] = forkLineage(t, s, id)
					st.stamps[id] = historyStampOf(t, s, id)
				}
				st.holders = holderIDs(t, s)
				if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM thread_fork_hidden), (SELECT count(*) FROM payloads)`).Scan(&st.hidden, &st.payloads); err != nil {
					t.Fatal(err)
				}
				return st
			}
			before := read()
			mustExec(t, s.db, tc.trigger)
			if err := tc.write(s); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("split with a failing step = %v", err)
			}
			if after := read(); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("a failed split changed the store:\n got %+v\nwant %+v", after, before)
			}
			mustExec(t, s.db, `DROP TRIGGER fail_split`)
			if err := tc.write(s); err != nil {
				t.Fatal(err)
			}
			requireShape(t, s, "F", before.shapes["F"])
			requireShape(t, s, "G", before.shapes["G"])
		})
	}
}

// TestSourceRowUnderAHeldIDStaysOutOfItsForks: a revert moves the rows its
// fork shows to a holder, which hides every id it took. A row the source
// writes later under one of those ids, at a position the fork still reads
// the source at, is not the fork's history: the fork reads the holder's
// row and never the source's.
func TestSourceRowUnderAHeldIDStaysOutOfItsForks(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 4)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	view := timelineShape(t, s, "F")
	if _, _, err := s.DeleteConversationFromTurn("S", 2); err != nil {
		t.Fatal(err)
	}
	requireShape(t, s, "F", view)
	if err := insertCarded(s, Item{ID: "a3", ThreadID: "S", TurnIndex: 0, ItemIndex: 5, Kind: "assistant_text", Role: "assistant",
		Status: "completed", Summary: "the source's own a3", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	requireShape(t, s, "F", view)
	if got := ownIDs(t, s, "S"); !slices.Contains(got, "a3") {
		t.Fatalf("S rows = %v, want its own a3", got)
	}
}
