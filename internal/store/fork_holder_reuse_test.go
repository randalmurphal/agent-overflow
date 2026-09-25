package store

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// Consecutive splits and copies of one thread's rows share a holder
// (reusableHolderTx): a reader's lineage grows by a level only when a
// reader of the thread does not read the holder the others read.

// seedTurnedSource creates src with `turns` settled turns of a user row
// and a reply (seedLinearSource), each with its settled turn row.
func seedTurnedSource(t *testing.T, s *Store, src string, turns int) {
	t.Helper()
	seedLinearSource(t, s, src, turns)
	for turn := range turns {
		mustExec(t, s.db, `INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at, stop_reason)
			VALUES (?, ?, ?, 1, 2, 'end_turn')`, fmt.Sprintf("%s:%d", src, turn), src, turn)
	}
}

// appendSourceTurn gives src a settled turn at turn, rows id-0 and id-1
// and its turn row, the way the message sent after a revert lands.
func appendSourceTurn(t *testing.T, s *Store, src string, turn int, id string) {
	t.Helper()
	for i, kind := range []string{"user_text", "assistant_text"} {
		role := "user"
		if i == 1 {
			role = "assistant"
		}
		if err := insertCarded(s, Item{ID: fmt.Sprintf("%s-%d", id, i), ThreadID: src, TurnIndex: turn, ItemIndex: i, Kind: kind,
			Role: role, Status: "completed", Summary: id, Meta: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("append %s to %s: %v", id, src, err)
		}
	}
	turnID := fmt.Sprintf("%s:%d", src, turn)
	if err := s.InsertTurn(Turn{TurnID: turnID, ThreadID: src, TurnIndex: turn, StartedAt: 1}); err != nil {
		t.Fatalf("turn %s: %v", turnID, err)
	}
	if err := s.UpdateTurnCompleted(turnID, 2, "end_turn", "", "", ""); err != nil {
		t.Fatalf("settle turn %s: %v", turnID, err)
	}
}

// readerViews renders what each reader reads: its rows, then its turn rows.
func readerViews(t *testing.T, s *Store, readers ...string) map[string][]string {
	t.Helper()
	views := make(map[string][]string, len(readers))
	for _, reader := range readers {
		views[reader] = append(timelineShape(t, s, reader), forkTurns(t, s, reader)...)
	}
	return views
}

func requireViews(t *testing.T, s *Store, want map[string][]string, stage string) {
	t.Helper()
	for reader, view := range want {
		if got := readerViews(t, s, reader)[reader]; !slices.Equal(got, view) {
			t.Fatalf("%s: %s reads\n%v\nwant\n%v", stage, reader, got, view)
		}
	}
}

func maxLineageDepth(t *testing.T, s *Store) int {
	t.Helper()
	var depth int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(depth), 0) FROM thread_fork_lineage`).Scan(&depth); err != nil {
		t.Fatal(err)
	}
	return depth
}

// TestRepeatedRevertsReuseOneHolder: a source reverted 100 times, each
// time into the rows its two forks show, with a new message after each,
// gives them all to one holder. Every fork reads it and the source, two
// levels, and reads what it read before.
func TestRepeatedRevertsReuseOneHolder(t *testing.T) {
	const reverts = 100
	s := newTestStore(t)
	seedTurnedSource(t, s, "S", reverts+1)
	mustPointerFork(t, s, "S", "F1", ForkCut{})
	mustPointerFork(t, s, "S", "F2", ForkCut{})
	views := readerViews(t, s, "F1", "F2")
	stamps := map[string]HistoryStamp{"F1": historyStampOf(t, s, "F1"), "F2": historyStampOf(t, s, "F2")}
	for turn := reverts; turn >= 1; turn-- {
		if _, _, err := s.DeleteConversationFromTurn("S", turn); err != nil {
			t.Fatalf("revert from turn %d: %v", turn, err)
		}
		appendSourceTurn(t, s, "S", turn, fmt.Sprintf("n%d", turn))
		holders := holderIDs(t, s)
		if len(holders) != 1 {
			t.Fatalf("after the revert from turn %d: holders = %v, want one", turn, holders)
		}
		for _, reader := range []string{"F1", "F2"} {
			requireIDs(t, reader+" lineage", forkLineage(t, s, reader), []string{
				fmt.Sprintf("1:%s:%d:2", holders[0], reverts), fmt.Sprintf("2:S:%d:%d", turn, math.MinInt32),
			})
		}
		requireViews(t, s, views, fmt.Sprintf("after the revert from turn %d", turn))
	}
	if depth := maxLineageDepth(t, s); depth != 2 {
		t.Fatalf("deepest level = %d, want 2", depth)
	}
	for id, was := range stamps {
		if now := historyStampOf(t, s, id); now != was {
			t.Fatalf("%s's stamp moved %+v -> %+v", id, was, now)
		}
	}
	requireIDs(t, "S rows", ownIDs(t, s, "S"), []string{"u0", "a0", "n1-0", "n1-1"})
	h := holderIDs(t, s)[0]
	var held []string
	for turn := 1; turn <= reverts; turn++ {
		held = append(held, fmt.Sprintf("u%d", turn), fmt.Sprintf("a%d", turn))
	}
	requireIDs(t, "holder rows", ownIDs(t, s, h), held)
}

// TestNewReaderBetweenRevertsAddsOneLevel: a fork made from a reader of the
// holder reads it already, and the reverts after it reuse the holder. A
// new fork of the source does not read it: the next revert gives it and
// every earlier reader one new holder, one level each, and the reverts
// after that reuse the new one.
func TestNewReaderBetweenRevertsAddsOneLevel(t *testing.T) {
	s := newTestStore(t)
	seedTurnedSource(t, s, "S", 12)
	mustPointerFork(t, s, "S", "F1", ForkCut{})
	mustPointerFork(t, s, "S", "F2", ForkCut{})
	readers := []string{"F1", "F2"}
	views := readerViews(t, s, readers...)
	revert := func(turn int) {
		t.Helper()
		if _, _, err := s.DeleteConversationFromTurn("S", turn); err != nil {
			t.Fatalf("revert from turn %d: %v", turn, err)
		}
		appendSourceTurn(t, s, "S", turn, fmt.Sprintf("n%d", turn))
		requireViews(t, s, views, fmt.Sprintf("after the revert from turn %d", turn))
	}
	revert(11)
	revert(10)
	holders := holderIDs(t, s)
	if len(holders) != 1 {
		t.Fatalf("holders = %v, want one", holders)
	}
	h1 := holders[0]

	mustPointerFork(t, s, "F1", "G", ForkCut{})
	readers = append(readers, "G")
	views["G"] = readerViews(t, s, "G")["G"]
	revert(9)
	requireIDs(t, "holders after a fork of a reader", holderIDs(t, s), []string{h1})
	requireIDs(t, "G lineage", forkLineage(t, s, "G"), []string{"1:F1:11:2", "2:" + h1 + ":11:2", "3:S:9:-2147483648"})

	mustPointerFork(t, s, "S", "F3", ForkCut{})
	readers = append(readers, "F3")
	views["F3"] = readerViews(t, s, "F3")["F3"]
	before := map[string]int{}
	for _, reader := range readers {
		before[reader] = len(forkLineage(t, s, reader))
	}
	revert(8)
	holders = holderIDs(t, s)
	if len(holders) != 2 || holders[0] != h1 {
		t.Fatalf("holders after a new fork of the source = %v, want %s and one new", holders, h1)
	}
	h2 := holders[1]
	for _, reader := range readers {
		if got := len(forkLineage(t, s, reader)); got != before[reader]+1 {
			t.Fatalf("%s reads %d levels, want %d", reader, got, before[reader]+1)
		}
	}
	requireIDs(t, "F1 lineage", forkLineage(t, s, "F1"), []string{"1:" + h1 + ":11:2", "2:" + h2 + ":9:-2147483648", "3:S:8:-2147483648"})
	requireIDs(t, "F3 lineage", forkLineage(t, s, "F3"), []string{"1:" + h2 + ":9:2", "2:S:8:-2147483648"})
	requireIDs(t, "second holder rows", ownIDs(t, s, h2), []string{"u8", "a8", "n9-0", "n9-1"})

	for turn := 7; turn >= 1; turn-- {
		revert(turn)
	}
	requireIDs(t, "holders after more reverts", holderIDs(t, s), []string{h1, h2})
	for _, reader := range readers {
		if got := len(forkLineage(t, s, reader)); got != before[reader]+1 {
			t.Fatalf("%s reads %d levels after more reverts, want %d", reader, got, before[reader]+1)
		}
	}
}

// TestForkChainDepthCapUnreachableThroughSourceWrites: whatever a source
// does to the rows its forks show (reverts, deletes, late writes to rows
// and turn rows, new turns), a reader's lineage grows by at most one level
// per reader of the source, never near forkLineageMaxDepth, and every
// reader reads what it read before.
func TestForkChainDepthCapUnreachableThroughSourceWrites(t *testing.T) {
	s := newTestStore(t)
	seedTurnedSource(t, s, "S", 40)
	mustPointerFork(t, s, "S", "F1", ForkCut{})
	mustPointerFork(t, s, "S", "F2", throughTurn(25))
	mustPointerFork(t, s, "S", "F3", throughTurn(10))
	mustPointerFork(t, s, "F2", "G", ForkCut{})
	readers := []string{"F1", "F2", "F3", "G"}
	views := readerViews(t, s, readers...)
	// G reads F2 before the source; every other level a reader gains is a
	// holder of the source's, one per reader of it at most.
	bound := 1 + len(readers) + 1
	rng := rand.New(rand.NewPCG(7, 11))
	lastTurn := func() int {
		t.Helper()
		var turn int
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(turn_index), 0) FROM (
			SELECT turn_index FROM items WHERE thread_id = 'S' UNION ALL SELECT turn_index FROM turns WHERE thread_id = 'S')`).Scan(&turn); err != nil {
			t.Fatal(err)
		}
		return turn
	}
	pick := func(query string) string {
		t.Helper()
		ids, err := queryIDs(s.db, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 0 {
			return ""
		}
		return ids[rng.IntN(len(ids))]
	}
	next := 0
	deepest := 0
	for step := range 300 {
		var op string
		var err error
		switch rng.IntN(6) {
		case 0:
			turn := 1 + rng.IntN(max(lastTurn(), 1))
			op = fmt.Sprintf("revert from turn %d", turn)
			if _, _, err = s.DeleteConversationFromTurn("S", turn); err == nil {
				next++
				appendSourceTurn(t, s, "S", turn, fmt.Sprintf("n%d", next))
			}
		case 1:
			id := pick(`SELECT id FROM items WHERE thread_id = 'S' AND item_index = 1 AND turn_index > 0`)
			if id == "" {
				continue
			}
			op = "revert from item " + id
			if _, _, err = s.DeleteConversationFromItem("S", id); err == nil {
				next++
				appendSourceTurn(t, s, "S", lastTurn()+1, fmt.Sprintf("n%d", next))
			}
		case 2:
			id := pick(`SELECT id FROM items WHERE thread_id = 'S'`)
			if id == "" {
				continue
			}
			op = "late write to " + id
			err = s.UpdateItemMeta("S", id, fmt.Sprintf(`{"late":%d}`, step))
		case 3:
			id := pick(`SELECT turn_id FROM turns WHERE thread_id = 'S'`)
			if id == "" {
				continue
			}
			op = "late settle of " + id
			err = s.UpdateTurnCompleted(id, int64(1000+step), "late", "", "", "")
		case 4:
			id := pick(`SELECT id FROM items WHERE thread_id = 'S'`)
			if id == "" {
				continue
			}
			op = "delete " + id
			err = s.DeleteThreadItem("S", id)
		default:
			next++
			op = "new turn"
			appendSourceTurn(t, s, "S", lastTurn()+1, fmt.Sprintf("n%d", next))
		}
		if err != nil {
			t.Fatalf("step %d, %s: %v", step, op, err)
		}
		requireViews(t, s, views, fmt.Sprintf("step %d, %s", step, op))
		deepest = max(deepest, maxLineageDepth(t, s))
		if deepest > bound {
			t.Fatalf("step %d, %s: a reader reads %d levels, want at most %d", step, op, deepest, bound)
		}
	}
	if holders := holderIDs(t, s); len(holders) > len(readers) {
		t.Fatalf("holders = %v, want at most one per reader", holders)
	}
	t.Logf("deepest lineage %d of at most %d (cap %d), %d holders", deepest, bound, forkLineageMaxDepth, len(holderIDs(t, s)))
}

// TestLateReportsReuseTheHolderForAnyOfItsReaders: two forks cut at
// different turns, and late reports alternating between rows only the
// later fork shows and rows both show. The first report of each kind
// makes a holder; every later one goes to the holder both read, whichever
// of its readers show the row, so the lineage stops growing.
func TestLateReportsReuseTheHolderForAnyOfItsReaders(t *testing.T) {
	s := newTestStore(t)
	seedTurnedSource(t, s, "S", 10)
	mustPointerFork(t, s, "S", "late", ForkCut{})
	mustPointerFork(t, s, "S", "early", throughTurn(4))
	views := readerViews(t, s, "late", "early")
	for turn := range 5 {
		for _, id := range []string{fmt.Sprintf("u%d", turn+5), fmt.Sprintf("u%d", turn), fmt.Sprintf("a%d", turn+5), fmt.Sprintf("a%d", turn)} {
			if err := s.UpdateItemMeta("S", id, `{"late":true}`); err != nil {
				t.Fatalf("late write to %s: %v", id, err)
			}
			requireViews(t, s, views, "after the late write to "+id)
		}
		if turn < 4 {
			for _, id := range []string{fmt.Sprintf("S:%d", turn+5), fmt.Sprintf("S:%d", turn)} {
				if err := s.UpdateTurnCompleted(id, 50, "late", "", "", ""); err != nil {
					t.Fatalf("late settle of %s: %v", id, err)
				}
				requireViews(t, s, views, "after the late settle of "+id)
			}
		}
	}
	holders := holderIDs(t, s)
	if len(holders) != 2 {
		t.Fatalf("holders = %v, want two", holders)
	}
	requireIDs(t, "late lineage", forkLineage(t, s, "late"), []string{"1:" + holders[0] + ":9:2", "2:" + holders[1] + ":9:2", "3:S:9:2"})
	requireIDs(t, "early lineage", forkLineage(t, s, "early"), []string{"1:" + holders[1] + ":4:2", "2:S:4:2"})
}

// TestSplitPastTheDepthCapIsRefused: a chain of forks as deep as the cap
// leaves no level for a holder, so the source's delete of a row the chain
// shows, and its late write to one, are refused whole with
// ErrForkChainTooDeep and change nothing.
func TestSplitPastTheDepthCapIsRefused(t *testing.T) {
	s := newTestStore(t)
	seedTurnedSource(t, s, "c0", 2)
	for level := 1; level <= forkLineageMaxDepth; level++ {
		mustPointerFork(t, s, fmt.Sprintf("c%d", level-1), fmt.Sprintf("c%d", level), ForkCut{})
	}
	deepest := fmt.Sprintf("c%d", forkLineageMaxDepth)
	views := readerViews(t, s, "c1", deepest)
	for name, write := range map[string]func() error{
		"delete":     func() error { return s.DeleteThreadItem("c0", "a0") },
		"late write": func() error { return s.UpdateItemMeta("c0", "a0", `{"late":true}`) },
		"late settle": func() error {
			return s.UpdateTurnCompleted("c0:0", 9, "late", "", "", "")
		},
	} {
		if err := write(); !errors.Is(err, ErrForkChainTooDeep) || !IsShownHistoryRefusal(err) {
			t.Fatalf("%s past the cap = %v, want ErrForkChainTooDeep", name, err)
		}
		requireViews(t, s, views, name)
		if holders := holderIDs(t, s); len(holders) != 0 {
			t.Fatalf("%s past the cap left holders %v", name, holders)
		}
	}
	if err := s.UpdateItemMeta("c0", "missing", `{}`); err == nil || IsShownHistoryRefusal(err) {
		t.Fatalf("a write to a missing row = %v, want an error that is no refusal", err)
	}
}

// TestRetiredForkIsNeverReusedAsAHolder: a deleted fork its forks still
// read is a holder of its own history, not of its source's rows: its rows
// sit at positions its source's rows also take, and its readers read it
// with cuts of its own timeline. A source write that must give rows to a
// reader that reads the retired fork right before the source gives them to
// a new holder, and every reader reads what it read before.
func TestRetiredForkIsNeverReusedAsAHolder(t *testing.T) {
	for name, write := range map[string]func(*Store) error{
		"late write": func(s *Store) error { return s.UpdateItemMeta("S", "x", `{"late":true}`) },
		"revert": func(s *Store) error {
			_, _, err := s.DeleteConversationFromTurn("S", 1)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			seedTurnedSource(t, s, "S", 6)
			if err := insertCarded(s, Item{ID: "x", ThreadID: "S", TurnIndex: 1, ItemIndex: 5, Kind: "tool_call", Role: "assistant",
				Status: "completed", Summary: "x", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
				t.Fatal(err)
			}
			mustPointerFork(t, s, "S", "R", ForkCut{})
			mustPointerFork(t, s, "R", "X", ForkCut{})
			// R reverts to turn 1 and writes turns of its own where S's
			// rows sit; X keeps reading S's there.
			if _, _, err := s.DeleteConversationFromTurn("R", 1); err != nil {
				t.Fatal(err)
			}
			appendSourceTurn(t, s, "R", 1, "r1")
			appendSourceTurn(t, s, "R", 2, "r2")
			mustPointerFork(t, s, "R", "G", ForkCut{})
			if err := s.DeleteThread("R"); err != nil {
				t.Fatal(err)
			}
			requireIDs(t, "holders", holderIDs(t, s), []string{"R"})
			views := readerViews(t, s, "X", "G")
			if err := write(s); err != nil {
				t.Fatal(err)
			}
			requireViews(t, s, views, name)
			holders := holderIDs(t, s)
			if len(holders) != 2 || holders[0] != "R" {
				t.Fatalf("holders = %v, want R and a new one", holders)
			}
			requireIDs(t, "R rows", ownIDs(t, s, "R"), []string{"r1-0", "r1-1", "r2-0", "r2-1"})
		})
	}
}
