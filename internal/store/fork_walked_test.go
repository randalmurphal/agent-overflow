package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// A pointer fork serves an inherited anchor from the stamp of the level
// that holds it when that stamp describes what the fork shows, and walks
// it otherwise (fork_walked.go). Every test here holds the served read to
// the walk: the same read with the stamps of the thread and of every
// level it reads removed.

// seedWalkedForkSource creates src: turn 0 a user row and a reply, turn 1
// an agent A with a tool child, a nested agent B with a child and a later
// child of A, turn 2 a user row, a reply, an agent C with a tool child
// and a top-level tool call T with none, each child written with its
// parent's card.
func seedWalkedForkSource(t *testing.T, s *Store, src string) {
	t.Helper()
	seedLinearSource(t, s, src, 1)
	seedStampRowsForTest(t, s, src, []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 1},
		{id: "B", kind: "tool_call", tool: "Agent", summary: "Agent: B", parent: "A", turn: 1, index: 2},
		{id: "B-c1", kind: "assistant_text", summary: "B one", parent: "B", turn: 1, index: 3},
		{id: "A-c2", kind: "assistant_text", summary: "A two", parent: "A", turn: 1, index: 4},
		{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
		{id: "a2", kind: "assistant_text", summary: "reply 2", turn: 2, index: 1},
		{id: "C", kind: "tool_call", tool: "Agent", summary: "Agent: C", turn: 2, index: 2},
		{id: "C-c1", kind: "tool_call", tool: "Read", summary: "Read: c", parent: "C", turn: 2, index: 3},
		{id: "T", kind: "tool_call", tool: "Bash", summary: "Bash: top", turn: 2, index: 4},
	})
}

func seedStampRowsForTest(t *testing.T, s *Store, threadID string, rows []stampFixtureRow) {
	t.Helper()
	for _, row := range rows {
		item := row.item(threadID)
		if item.Meta == "" {
			item.Meta = "{}"
		}
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("insert %s/%s: %v", threadID, row.id, err)
		}
	}
}

// canonicalMetaForTest re-encodes a meta blob with sorted keys, so two
// reads compare byte for byte whatever order their merges wrote.
func canonicalMetaForTest(t *testing.T, meta string) string {
	t.Helper()
	if strings.TrimSpace(meta) == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// forkMetasForTest reads every row of threadID as the page
// (ListWireItems) and the tray (decorateLatestDirectSubagentTools) serve
// it, each meta canonical.
func forkMetasForTest(t *testing.T, s *Store, q sqlQueryer, threadID string) map[string]string {
	t.Helper()
	rows, err := s.listWireItemsTx(q, threadID, threadTimelineIDsForTest(t, q, threadID))
	if err != nil {
		t.Fatalf("read wire items of %s: %v", threadID, err)
	}
	rows, err = s.decorateLatestDirectSubagentTools(q, threadID, rows)
	if err != nil {
		t.Fatalf("decorate the tray of %s: %v", threadID, err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.ID] = canonicalMetaForTest(t, row.Meta)
	}
	return out
}

// assertForkWalkParity fails unless every row threadID serves is, byte
// for byte, the row the walk serves: the read in a rolled-back
// transaction without the stamps of threadID and of every level it reads,
// the thread listed for the backfill so its own anchors walk too.
func assertForkWalkParity(t *testing.T, s *Store, threadID, stage string) {
	t.Helper()
	got := forkMetasForTest(t, s, s.reader(), threadID)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stripReadStampsForTest(t, tx, threadID)
	if _, err := tx.Exec(`INSERT OR IGNORE INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, threadID); err != nil {
		t.Fatal(err)
	}
	want := forkMetasForTest(t, s, tx, threadID)
	for _, id := range slices.Sorted(maps.Keys(want)) {
		if got[id] != want[id] {
			t.Errorf("%s: %s serves %s\n  the walk serves %s", stage, id, got[id], want[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: %d rows served, the walk reads %d", stage, len(got), len(want))
	}
}

// inheritedDecisionsForTest reads threadID's timeline and decides each
// inherited anchorable row as its reads do (inheritedStampReads): served
// with a card or tray, served with nothing, or walked.
func inheritedDecisionsForTest(t *testing.T, s *Store, threadID string) (withCard, empty, walked []string) {
	t.Helper()
	q := s.reader()
	rows, err := s.listWireItemsTx(q, threadID, threadTimelineIDsForTest(t, q, threadID))
	if err != nil {
		t.Fatal(err)
	}
	inherited := make(map[string]Item)
	for _, row := range rows {
		if row.Rev < 0 {
			inherited[row.ID] = row
		}
	}
	decider, err := newSubagentWalkDecider(q, threadID, inherited)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range slices.Sorted(maps.Keys(inherited)) {
		row := inherited[id]
		if !SubagentAnchorable(row.Kind, row.ToolName) {
			continue
		}
		stamp, served := decider.served(id)
		switch {
		case !served:
			walked = append(walked, id)
		case stamp.hasCard || stamp.hasTray:
			withCard = append(withCard, id)
		default:
			empty = append(empty, id)
		}
	}
	return withCard, empty, walked
}

// servedInheritedAnchorsForTest lists the inherited anchors of threadID's
// timeline a stamp serves a card or tray for (inheritedStampReads).
func servedInheritedAnchorsForTest(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	served, _, _ := inheritedDecisionsForTest(t, s, threadID)
	return served
}

func requireServed(t *testing.T, s *Store, threadID string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	got := servedInheritedAnchorsForTest(t, s, threadID)
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s serves %v from stamps, want %v", threadID, got, want)
	}
}

// walkedMarkersForTest lists threadID's markers.
func walkedMarkersForTest(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	ids, err := queryIDs(s.db, `SELECT item_id FROM thread_fork_walked WHERE thread_id = ? ORDER BY item_id`, threadID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func requireMarkers(t *testing.T, s *Store, threadID string, want ...string) {
	t.Helper()
	if got := walkedMarkersForTest(t, s, threadID); !slices.Equal(got, want) {
		t.Errorf("%s marks %v, want %v", threadID, got, want)
	}
}

// readMarkersForTest lists the markers threadID's reads consult: its own
// and those of every level it reads.
func readMarkersForTest(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	ids, err := queryIDs(s.db, `SELECT DISTINCT item_id FROM thread_fork_walked
		 WHERE thread_id = ?1 OR thread_id IN (SELECT ancestor_id FROM thread_fork_lineage WHERE thread_id = ?1)
		 ORDER BY item_id`, threadID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// assertBackfillMatchesWriters holds migration v132 to the writers: with
// the markers they recorded replaced by those the migration records from
// the same rows, every reader consults the same markers, serves the same
// anchors and still reads what the walk reads.
func assertBackfillMatchesWriters(t *testing.T, s *Store) {
	t.Helper()
	readers, err := queryIDs(s.db, `SELECT DISTINCT thread_id FROM thread_fork_lineage ORDER BY thread_id`)
	if err != nil {
		t.Fatal(err)
	}
	type view struct{ markers, served []string }
	read := func(reader string) view {
		return view{readMarkersForTest(t, s, reader), servedInheritedAnchorsForTest(t, s, reader)}
	}
	written := make(map[string]view, len(readers))
	for _, reader := range readers {
		written[reader] = read(reader)
	}
	mustExec(t, s.db, `DROP TABLE thread_fork_walked`)
	mustExec(t, s.db, forkWalkedAnchorsV132SQL)
	for _, reader := range readers {
		got, want := read(reader), written[reader]
		if !slices.Equal(got.markers, want.markers) {
			t.Errorf("after the backfill %s reads markers %v, the writers left %v", reader, got.markers, want.markers)
		}
		if !slices.Equal(got.served, want.served) {
			t.Errorf("after the backfill %s serves %v, the writers left %v", reader, got.served, want.served)
		}
		assertForkWalkParity(t, s, reader, "after the backfill")
	}
}

// walkStatements counts the descendant walks among stmts
// (descendantsWalk's recursive step).
func walkStatements(stmts []recordedStatement) int {
	n := 0
	for _, stmt := range stmts {
		if strings.Contains(stmt.query, "rel.root") {
			n++
		}
	}
	return n
}

// TestPointerForkServesIdleSourceStamps: a fork of a source that wrote
// nothing since reads every inherited anchor from the source's stamps, as
// the source reads its own, and a fork of the fork does too. Its window
// read runs no descendant walk; a dirty stamp at the source brings one
// back.
func TestPointerForkServesIdleSourceStamps(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	requireMarkers(t, s, "F")
	requireServed(t, s, "F", "A", "B", "C")
	requireServed(t, s, "G", "A", "B", "C")
	// A tool call nothing hangs off has no stamp and is not walked.
	for _, threadID := range []string{"F", "G"} {
		if _, empty, walked := inheritedDecisionsForTest(t, s, threadID); !slices.Equal(empty, []string{"A-c1", "C-c1", "T"}) || len(walked) > 0 {
			t.Errorf("%s serves nothing for %v and walks %v, want A-c1, C-c1 and T, and no walk", threadID, empty, walked)
		}
	}
	assertForkWalkParity(t, s, "F", "idle source")
	assertForkWalkParity(t, s, "G", "fork of the fork")

	rec := recordStatements(t, s)
	window := func(threadID string) int {
		t.Helper()
		return walkStatements(rec.capture(func() {
			if _, err := s.SyncThreadWindow(context.Background(), threadID, "", 200, 30, UnknownHistoryStamp(), nil, TimelineSelection{}); err != nil {
				t.Fatal(err)
			}
		}))
	}
	for _, threadID := range []string{"S", "F", "G"} {
		if n := window(threadID); n != 0 {
			t.Errorf("%s's window read ran %d descendant walks, want none", threadID, n)
		}
	}
	setSubagentStampStateForTest(t, s, "S", "A", aggStateDirty)
	if n := window("F"); n == 0 {
		t.Error("F's window read walked nothing with the source's stamp of A dirty")
	}
	requireServed(t, s, "F", "B", "C")
	assertForkWalkParity(t, s, "F", "a dirty source stamp")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkWalksAnAnchorTheSourceGrew: a source row under an anchor
// the fork shows, past the fork's cut or below it, is one the fork does
// not show; the fork walks the anchor and every anchor above it.
func TestPointerForkWalksAnAnchorTheSourceGrew(t *testing.T) {
	t.Run("past the cut", func(t *testing.T) {
		s := newTestStore(t)
		seedWalkedForkSource(t, s, "S")
		mustPointerFork(t, s, "S", "F", ForkCut{})
		if _, err := appendCarded(s, stampFixtureRow{id: "A-c3", kind: "assistant_text", summary: "A three", parent: "A", meta: "{}", turn: 3}.item("S")); err != nil {
			t.Fatal(err)
		}
		requireMarkers(t, s, "F")
		requireServed(t, s, "F", "B", "C")
		assertForkWalkParity(t, s, "F", "a source child past the cut")
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("below the cut", func(t *testing.T) {
		s := newTestStore(t)
		seedWalkedForkSource(t, s, "S")
		mustPointerFork(t, s, "S", "F", ForkCut{})
		mustPointerFork(t, s, "F", "G", ForkCut{})
		seedStampRowsForTest(t, s, "S", []stampFixtureRow{
			{id: "B-late", kind: "tool_call", tool: "Bash", summary: "Bash: late", parent: "B", turn: 1, index: 5},
		})
		requireMarkers(t, s, "F", "A", "B")
		requireMarkers(t, s, "G", "A", "B")
		requireServed(t, s, "F", "C")
		requireServed(t, s, "G", "C")
		assertForkWalkParity(t, s, "F", "a late source child below the cut")
		assertForkWalkParity(t, s, "G", "a late source child below both cuts")
		// The source serves its own stamps, which count the row.
		assertSubagentStampParity(t, s, "S", "the source", true)
		assertBackfillMatchesWriters(t, s)
	})
}

// TestPointerForkRevertBelowAnAnchorWalksIt: a fork's own revert lowers
// its cut. Above the anchor's newest row the stamp still describes the
// fork; below it the fork walks.
func TestPointerForkRevertBelowAnAnchorWalksIt(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 1},
		{id: "q1", kind: "user_text", summary: "queued", turn: 1, index: 2},
		{id: "A-c2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: "A", turn: 1, index: 3},
		{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
		{id: "a2", kind: "assistant_text", summary: "reply 2", turn: 2, index: 1},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	requireServed(t, s, "F", "A")
	if _, _, err := s.DeleteConversationFromTurn("F", 2); err != nil {
		t.Fatal(err)
	}
	requireServed(t, s, "F", "A")
	assertForkWalkParity(t, s, "F", "a revert past the anchor's rows")
	if _, _, err := s.DeleteConversationFromItem("F", "q1"); err != nil {
		t.Fatal(err)
	}
	requireServed(t, s, "F")
	assertForkWalkParity(t, s, "F", "a revert inside the anchor's rows")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkRevertUnderItsReader: a fork's revert inside the rows of
// an anchor it holds a copy of recomputes the copy's stamp over the rows it
// still shows. A fork of it made before still shows the rest and walks
// the copy; one made after shows what the stamp counts and is served.
func TestPointerForkRevertUnderItsReader(t *testing.T) {
	seed := func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		seedLinearSource(t, s, "S", 1)
		seedStampRowsForTest(t, s, "S", []stampFixtureRow{
			{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
			{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 1},
			{id: "q1", kind: "user_text", summary: "queued", turn: 1, index: 2},
			{id: "A-c2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: "A", turn: 1, index: 3},
			{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
			{id: "a2", kind: "assistant_text", summary: "reply 2", turn: 2, index: 1},
		})
		mustPointerFork(t, s, "S", "F", ForkCut{})
		// A write without a card gives F a stamped copy of A.
		if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
			old, err := readMutableSubagentRowTx(tx, "F", "A", "test bulk write")
			if err != nil {
				return err
			}
			next := old
			next.summary = "Agent: A, edited"
			if _, err := tx.Exec(`UPDATE items SET summary = ? WHERE thread_id = 'F' AND id = 'A'`, next.summary); err != nil {
				return err
			}
			return w.updated(old, next)
		}); err != nil {
			t.Fatal(err)
		}
		if !ownsRow(t, s, "F", "A") {
			t.Fatal("fixture: F holds no copy of A")
		}
		return s
	}
	revert := func(t *testing.T, s *Store) {
		t.Helper()
		if _, _, err := s.DeleteConversationFromItem("F", "q1"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a reader made before", func(t *testing.T) {
		s := seed(t)
		mustPointerFork(t, s, "F", "G", ForkCut{})
		requireServed(t, s, "G", "A")
		revert(t, s)
		requireMarkers(t, s, "F", "A")
		requireServed(t, s, "G")
		assertForkWalkParity(t, s, "G", "a reader of a fork that reverted inside an anchor's rows")
		assertForkWalkParity(t, s, "F", "the fork that reverted")
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("a reader made after", func(t *testing.T) {
		s := seed(t)
		revert(t, s)
		mustPointerFork(t, s, "F", "G", ForkCut{})
		requireMarkers(t, s, "F")
		requireServed(t, s, "G", "A")
		assertForkWalkParity(t, s, "G", "a reader of a fork made after its revert")
		assertBackfillMatchesWriters(t, s)
	})
}

// TestPointerForkMarksAnchorsAboveHiddenLaunches: fork creation hides a
// live background launch and what hangs off it. Under an anchor the fork
// shows, the anchor is marked and walked; at the top level nothing is.
func TestPointerForkMarksAnchorsAboveHiddenLaunches(t *testing.T) {
	t.Run("nested", func(t *testing.T) {
		s := newTestStore(t)
		seedLinearSource(t, s, "S", 1)
		seedStampRowsForTest(t, s, "S", []stampFixtureRow{
			{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
			{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: L", parent: "A", status: "running", background: true, turn: 1, index: 1},
			{id: "L-c1", kind: "assistant_text", summary: "L one", parent: "L", turn: 1, index: 2},
			{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 3},
			{id: "C", kind: "tool_call", tool: "Agent", summary: "Agent: C", turn: 2},
			{id: "C-c1", kind: "tool_call", tool: "Read", summary: "Read: c", parent: "C", turn: 2, index: 1},
		})
		mustPointerFork(t, s, "S", "F", ForkCut{})
		mustPointerFork(t, s, "F", "G", ForkCut{})
		requireMarkers(t, s, "F", "A")
		requireMarkers(t, s, "G")
		requireServed(t, s, "F", "C")
		// G reads A through S and the marker through F.
		requireServed(t, s, "G", "C")
		assertForkWalkParity(t, s, "F", "a hidden nested launch")
		assertForkWalkParity(t, s, "G", "a hidden nested launch, one level on")
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("top level", func(t *testing.T) {
		s := newTestStore(t)
		seedLinearSource(t, s, "S", 1)
		seedStampRowsForTest(t, s, "S", []stampFixtureRow{
			{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
			{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 1},
			{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: L", status: "running", background: true, turn: 1, index: 2},
			{id: "L-c1", kind: "assistant_text", summary: "L one", parent: "L", turn: 1, index: 3},
		})
		mustPointerFork(t, s, "S", "F", ForkCut{})
		requireMarkers(t, s, "F")
		requireServed(t, s, "F", "A")
		assertForkWalkParity(t, s, "F", "a hidden top-level launch")
		assertBackfillMatchesWriters(t, s)
	})
}

// TestHolderSplitsKeepServableStamps: a source revert gives the rows its
// forks show to a holder. An anchor that moves whole keeps its clean
// stamp and is served from the holder; a dirty one loses it and is
// walked; an anchor whose rows straddle the split is marked at the holder
// and walked.
func TestHolderSplitsKeepServableStamps(t *testing.T) {
	seedMoved := func(t *testing.T, s *Store) {
		t.Helper()
		seedLinearSource(t, s, "S", 1)
		seedStampRowsForTest(t, s, "S", []stampFixtureRow{
			{id: "u1", kind: "user_text", summary: "user 1", turn: 1},
			{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1, index: 1},
			{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 2},
			{id: "B", kind: "tool_call", tool: "Agent", summary: "Agent: B", parent: "A", turn: 1, index: 3},
			{id: "B-c1", kind: "assistant_text", summary: "B one", parent: "B", turn: 1, index: 4},
			{id: "a1", kind: "assistant_text", summary: "reply 1", turn: 1, index: 5},
		})
		mustPointerFork(t, s, "S", "F", ForkCut{})
	}
	t.Run("moved whole", func(t *testing.T) {
		s := newTestStore(t)
		seedMoved(t, s)
		before := subagentStampRowsForTest(t, s, "S")
		if _, _, err := s.DeleteConversationFromTurn("S", 1); err != nil {
			t.Fatal(err)
		}
		holder := holderOf(t, s, "F")
		if got := subagentStampRowsForTest(t, s, holder); !maps.Equal(got, before) {
			t.Errorf("the holder keeps stamps %v, the source had %v", got, before)
		}
		requireMarkers(t, s, holder)
		requireServed(t, s, "F", "A", "B")
		assertForkWalkParity(t, s, "F", "an anchor the holder took whole")
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("dirty", func(t *testing.T) {
		s := newTestStore(t)
		seedMoved(t, s)
		setSubagentStampStateForTest(t, s, "S", "A", aggStateDirty)
		if _, _, err := s.DeleteConversationFromTurn("S", 1); err != nil {
			t.Fatal(err)
		}
		holder := holderOf(t, s, "F")
		if got := slices.Sorted(maps.Keys(subagentStampRowsForTest(t, s, holder))); !slices.Equal(got, []string{"B"}) {
			t.Errorf("the holder keeps stamps of %v, want only B's clean one", got)
		}
		requireServed(t, s, "F", "B")
		assertForkWalkParity(t, s, "F", "a dirty stamp the holder took")
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("straddled", func(t *testing.T) {
		s := newTestStore(t)
		seedLinearSource(t, s, "S", 1)
		seedStampRowsForTest(t, s, "S", []stampFixtureRow{
			{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
			{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 1},
			{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
			{id: "A-c2", kind: "tool_call", tool: "Bash", summary: "Bash: two", parent: "A", turn: 2, index: 1},
			{id: "a2", kind: "assistant_text", summary: "reply 2", turn: 2, index: 2},
		})
		mustPointerFork(t, s, "S", "F", ForkCut{})
		if _, _, err := s.DeleteConversationFromTurn("S", 2); err != nil {
			t.Fatal(err)
		}
		holder := holderOf(t, s, "F")
		requireMarkers(t, s, holder, "A")
		requireServed(t, s, "F")
		assertForkWalkParity(t, s, "F", "an anchor whose rows straddle the split")
		assertSubagentStampParity(t, s, "S", "the source after the split", true)
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("one row", func(t *testing.T) {
		// The delete of one shown row takes it alone: B goes with its
		// stamp, its child stays in the source.
		s := newTestStore(t)
		seedWalkedForkSource(t, s, "S")
		mustPointerFork(t, s, "S", "F", ForkCut{})
		if err := s.DeleteThreadItem("S", "B"); err != nil {
			t.Fatal(err)
		}
		holder := holderOf(t, s, "F")
		requireMarkers(t, s, holder, "A")
		requireServed(t, s, "F", "B", "C")
		assertForkWalkParity(t, s, "F", "a nested anchor the holder took alone")
		assertBackfillMatchesWriters(t, s)
	})
}

// TestRetiredSourceServesItsStamps: a deleted source its forks read
// becomes a holder with its clean stamps, which still serve the forks.
func TestRetiredSourceServesItsStamps(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	before := subagentStampRowsForTest(t, s, "S")
	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	if got := subagentStampRowsForTest(t, s, "S"); !maps.Equal(got, before) {
		t.Errorf("the retired source keeps stamps %v, it had %v", got, before)
	}
	requireServed(t, s, "F", "A", "B", "C")
	assertForkWalkParity(t, s, "F", "a retired source")
	assertBackfillMatchesWriters(t, s)
}

// TestHolderCopyGetsNoStamp: a fork's hide of a row its own fork shows
// gives the reader a copy in a holder. The copy carries no stamp, since
// the holder's own timeline lacks the anchor's rows, and is walked.
func TestHolderCopyGetsNoStamp(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if err := s.DeleteThreadItem("F", "A"); err != nil {
		t.Fatal(err)
	}
	holder := holderOf(t, s, "G")
	if !ownsRow(t, s, holder, "A") {
		t.Fatal("fixture: the holder holds no copy of A")
	}
	if got := subagentStampRowsForTest(t, s, holder); len(got) != 0 {
		t.Errorf("the holder's copy carries stamps %v", got)
	}
	requireServed(t, s, "G", "B", "C")
	if _, _, walked := inheritedDecisionsForTest(t, s, "G"); !slices.Equal(walked, []string{"A"}) {
		t.Errorf("G walks %v, want the copy A, whose rows the source holds", walked)
	}
	assertForkWalkParity(t, s, "G", "a copy in a holder")
	assertForkWalkParity(t, s, "F", "the fork that hid the row")
	assertBackfillMatchesWriters(t, s)
}

// TestHolderCopyOfAnAnchorWithImportedRows: a holder's unstamped copy of
// an anchor whose rows only imported history holds is walked.
func TestHolderCopyOfAnAnchorWithImportedRows(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 1},
		{id: "A-c2", kind: "assistant_text", summary: "A two", parent: "A", turn: 1, index: 2},
		{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
	})
	sealItemsForTest(t, s, "S", "A-c1", "A-c2")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if err := s.DeleteThreadItem("F", "A"); err != nil {
		t.Fatal(err)
	}
	if !ownsRow(t, s, holderOf(t, s, "G"), "A") {
		t.Fatal("fixture: the holder holds no copy of A")
	}
	// A-c1, which only imported history holds, is walked too.
	if _, _, walked := inheritedDecisionsForTest(t, s, "G"); !slices.Equal(walked, []string{"A", "A-c1"}) {
		t.Errorf("G walks %v, want the copy A and the imported A-c1", walked)
	}
	assertForkWalkParity(t, s, "G", "a copy over imported rows")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkImportUnderAnInheritedAnchor: an import batch of a fork
// that puts rows under an anchor the fork inherits marks it: the source's
// stamp does not count them.
func TestPointerForkImportUnderAnInheritedAnchor(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	row := stampFixtureRow{id: "B-c2", kind: "assistant_text", summary: "B two", parent: "B", meta: "{}", turn: 3}.item("F")
	if err := s.ApplyImportBatch("F", ImportBatch{
		Turns: []Turn{{TurnID: "F:3", ThreadID: "F", TurnIndex: 3, StartedAt: 1000}},
		Rows:  []ImportRow{{Item: row}},
	}); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "A", "B")
	requireServed(t, s, "F", "C")
	assertForkWalkParity(t, s, "F", "an imported row under an inherited anchor")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkServesResumeRounds: a root with a resumed round (§E6)
// and its carrier serve from the source's stamps. The source's rows of a
// later round sit at the root's turn: below the fork's cut the fork hides
// them and walks the root and its carriers; past it, the root's
// whole-transcript newest row passes the cut and the root walks.
func TestPointerForkServesResumeRounds(t *testing.T) {
	seed := func(t *testing.T, s *Store, turns int) {
		t.Helper()
		seedLinearSource(t, s, "S", 1)
		rows := []stampFixtureRow{
			{id: "R", kind: "tool_call", tool: "Agent", summary: "Agent: R", turn: 1},
			{id: "R-t1", kind: "tool_call", tool: "Bash", summary: "Bash: round one", parent: "R", turn: 1, index: 1},
			{id: "K", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 1, index: 2},
			{id: "user:subagent-prompt:K", kind: "user_text", summary: "again", parent: "R", meta: resumePromptMeta("K"), turn: 1, index: 3},
			{id: "R-t2", kind: "tool_call", tool: "Bash", summary: "Bash: round two", parent: "R", turn: 1, index: 4},
		}
		if turns > 2 {
			rows = append(rows,
				stampFixtureRow{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
				stampFixtureRow{id: "a2", kind: "assistant_text", summary: "reply 2", turn: 2, index: 1})
		}
		seedStampRowsForTest(t, s, "S", rows)
		mustPointerFork(t, s, "S", "F", ForkCut{})
		requireServed(t, s, "F", "K", "R")
		assertForkWalkParity(t, s, "F", "a resumed root")
	}
	resume := []stampFixtureRow{
		{id: "K2", kind: "tool_call", tool: "SendMessage", summary: "Agent: continue", meta: carrierMeta("R"), turn: 3},
		{id: "user:subagent-prompt:K2", kind: "user_text", summary: "once more", parent: "R", meta: resumePromptMeta("K2"), turn: 1, index: 5},
		{id: "R-t3", kind: "tool_call", tool: "Bash", summary: "Bash: round three", parent: "R", turn: 1, index: 6},
	}
	t.Run("a round below the cut", func(t *testing.T) {
		s := newTestStore(t)
		seed(t, s, 3)
		seedStampRowsForTest(t, s, "S", resume)
		requireMarkers(t, s, "F", "R")
		requireServed(t, s, "F")
		assertForkWalkParity(t, s, "F", "a round the source resumed below the cut")
		assertSubagentStampParity(t, s, "S", "the source's third round", true)
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("a late row of the last round", func(t *testing.T) {
		// Only the root is marked: the carrier walks through its root.
		s := newTestStore(t)
		seed(t, s, 3)
		seedStampRowsForTest(t, s, "S", []stampFixtureRow{
			{id: "R-late", kind: "tool_call", tool: "Bash", summary: "Bash: late", parent: "R", turn: 1, index: 5},
		})
		requireMarkers(t, s, "F", "R")
		requireServed(t, s, "F")
		assertForkWalkParity(t, s, "F", "a late row of the carrier's round")
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("a round past the cut", func(t *testing.T) {
		s := newTestStore(t)
		seed(t, s, 2)
		seedStampRowsForTest(t, s, "S", resume)
		requireMarkers(t, s, "F")
		requireServed(t, s, "F", "K")
		assertForkWalkParity(t, s, "F", "a round the source resumed past the cut")
		assertBackfillMatchesWriters(t, s)
	})
	t.Run("an unstamped carrier", func(t *testing.T) {
		// A carrier stored after the prompt that names it has no stamp;
		// nothing hangs off it, and its round is walked from its root.
		s := newTestStore(t)
		seed(t, s, 3)
		mustExec(t, s.db, `DELETE FROM subagent_aggregates WHERE thread_id = 'S' AND item_id = 'K'`)
		requireServed(t, s, "F", "R")
		if _, _, walked := inheritedDecisionsForTest(t, s, "F"); !slices.Equal(walked, []string{"K"}) {
			t.Errorf("F walks %v, want the unstamped carrier K", walked)
		}
		assertForkWalkParity(t, s, "F", "an unstamped carrier")
	})
}

// TestPointerForkWalksImportedAnchors: an anchor only imported history
// holds has no stamp at any level and is walked.
func TestPointerForkWalksImportedAnchors(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "I", kind: "tool_call", tool: "Agent", summary: "Agent: I", turn: 1},
		{id: "I-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "I", turn: 1, index: 1},
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 2},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: a", parent: "A", turn: 2, index: 1},
	})
	sealItemsForTest(t, s, "S", "I", "I-c1")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	requireServed(t, s, "F", "A")
	assertForkWalkParity(t, s, "F", "imported history")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkHidesAnImportedRow: a fork's hide of an imported row
// marks the imported anchors above it, which the fork walks in any case;
// a local anchor keeps being served.
func TestPointerForkHidesAnImportedRow(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "I", kind: "tool_call", tool: "Agent", summary: "Agent: I", turn: 1},
		{id: "I-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "I", turn: 1, index: 1},
		{id: "J", kind: "tool_call", tool: "Agent", summary: "Agent: J", parent: "I", turn: 1, index: 2},
		{id: "J-c1", kind: "assistant_text", summary: "J one", parent: "J", turn: 1, index: 3},
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 2},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: a", parent: "A", turn: 2, index: 1},
	})
	sealItemsForTest(t, s, "S", "I", "I-c1", "J", "J-c1")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.DeleteThreadItem("F", "J-c1"); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "I", "J")
	requireServed(t, s, "F", "A")
	assertForkWalkParity(t, s, "F", "a hidden imported row")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkOwnRowUnderAnInheritedAnchor: a write without a card
// (a bulk writer) puts a fork row under an inherited anchor without
// copying it. The source's stamp does not count the row; the fork marks
// the anchor and walks it.
func TestPointerForkOwnRowUnderAnInheritedAnchor(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	row := stampFixtureRow{id: "F-c1", kind: "tool_call", tool: "Bash", summary: "Bash: fork", parent: "B", meta: "{}", turn: 3}.item("F")
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		return insertItemTx(tx, w, row, "test bulk write")
	}); err != nil {
		t.Fatal(err)
	}
	if ownsRow(t, s, "F", "B") {
		t.Fatal("fixture: the bulk write copied B")
	}
	requireMarkers(t, s, "F", "A", "B")
	requireServed(t, s, "F", "C")
	assertForkWalkParity(t, s, "F", "a fork row under an inherited anchor")
	// Under a tool call nothing else hangs off, which has no stamp.
	under := stampFixtureRow{id: "F-c2", kind: "assistant_text", summary: "under T", parent: "T", meta: "{}", turn: 3, index: 1}.item("F")
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		return insertItemTx(tx, w, under, "test bulk write")
	}); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "A", "B", "T")
	if _, _, walked := inheritedDecisionsForTest(t, s, "F"); !slices.Contains(walked, "T") {
		t.Errorf("F walks %v, want T among them", walked)
	}
	assertForkWalkParity(t, s, "F", "a fork row under an unstamped inherited anchor")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkRowMovedUnderAnInheritedAnchor: a write without a card
// that moves a fork's own row under an inherited anchor marks it.
func TestPointerForkRowMovedUnderAnInheritedAnchor(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	row := stampFixtureRow{id: "F-x", kind: "assistant_text", summary: "fork text", meta: "{}", turn: 3}.item("F")
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		return insertItemTx(tx, w, row, "test bulk write")
	}); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F")
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		old, err := readMutableSubagentRowTx(tx, "F", "F-x", "test bulk write")
		if err != nil {
			return err
		}
		next := old
		next.parentID = "B"
		if _, err := tx.Exec(`UPDATE items SET parent_id = 'B' WHERE thread_id = 'F' AND id = 'F-x'`); err != nil {
			return err
		}
		return w.updated(old, next)
	}); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "A", "B")
	requireServed(t, s, "F", "C")
	assertForkWalkParity(t, s, "F", "a fork row moved under an inherited anchor")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkCopyUnderItsOwnCopy: a fork's copy under an anchor it
// holds a copy of marks nothing: its own stamp counts the row.
func TestPointerForkCopyUnderItsOwnCopy(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	for _, id := range []string{"A", "A-c2"} {
		if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
			old, err := readMutableSubagentRowTx(tx, "F", id, "test bulk write")
			if err != nil {
				return err
			}
			next := old
			next.summary = old.summary + ", edited"
			if _, err := tx.Exec(`UPDATE items SET summary = ? WHERE thread_id = 'F' AND id = ?`, next.summary, id); err != nil {
				return err
			}
			return w.updated(old, next)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !ownsRow(t, s, "F", "A") {
		t.Fatal("fixture: F holds no copy of A")
	}
	requireMarkers(t, s, "F")
	mustPointerFork(t, s, "F", "G", ForkCut{})
	requireServed(t, s, "G", "A", "B", "C")
	assertForkWalkParity(t, s, "F", "a copy under the fork's own copy")
	assertForkWalkParity(t, s, "G", "a reader of both copies")
	assertBackfillMatchesWriters(t, s)
	// A row of its own under the inherited B marks B, not A above it.
	row := stampFixtureRow{id: "F-b", kind: "assistant_text", summary: "under B", parent: "B", meta: "{}", turn: 3}.item("F")
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		return insertItemTx(tx, w, row, "test bulk write")
	}); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "B")
	assertForkWalkParity(t, s, "F", "an own row under an inherited anchor below the fork's copy")
	assertBackfillMatchesWriters(t, s)
}

// TestInheritedStampReadsPlan: the batched read of a fork's inherited
// stamps probes each table by key: the ids are the only scan, and nothing
// is sorted or indexed on the fly.
func TestInheritedStampReadsPlan(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	ids, err := json.Marshal([]inheritedAnchor{{ID: "A"}, {ID: "K", Root: "A"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		query string
		args  []any
		uses  []string
	}{
		{"inherited stamp reads", inheritedStampReadsSQL, []any{"G", string(ids)},
			[]string{"idx_items_parent", "idx_import_history_items_parent_lookup"}},
		{"placed anchors", placedAnchorsSQL, []any{"S", `[{"id":"x","parent":"A","turn":1,"item":9}]`, true, true}, nil},
		{"wider readers", widerReaderSQL, []any{"F"}, []string{"idx_thread_fork_lineage_ancestor"}},
	} {
		text := assertBoundedPlan(t, s, tc.name, boundedPlan{scans: map[string]bool{"json_each": true, "placed": true}}, tc.query, tc.args...)
		for _, forbidden := range []string{"TEMP B-TREE", "AUTOMATIC"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s plans %s:\n%s", tc.name, forbidden, text)
			}
		}
		for _, index := range tc.uses {
			if !strings.Contains(text, index+" ") {
				t.Errorf("%s does not probe %s:\n%s", tc.name, index, text)
			}
		}
	}
}

// holderStampRowsForTest reads a thread's stamp rows with their
// generations, which move with any write.
func holderStampRowsForTest(t *testing.T, s *Store, threadID string) map[string]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT item_id, json_array(state, gen, `+strings.Join(subagentAggregateValueColumns, ", ")+`)
		  FROM subagent_aggregates WHERE thread_id = ?`, threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, row string
		if err := rows.Scan(&id, &row); err != nil {
			t.Fatal(err)
		}
		out[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHolderStampsAreFrozen: no maintenance path writes, marks or deletes
// a holder's stamps, or fails on a holder: the boot pass over a running
// agent in it, the recompute of its dirty and legacy anchors while it is
// listed for the backfill, a flush, a restamp and a chain recompute.
func TestHolderStampsAreFrozen(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "u1", kind: "user_text", summary: "user 1", turn: 1},
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1, index: 1},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 2},
		{id: "B", kind: "tool_call", tool: "Agent", summary: "Agent: B", parent: "A", turn: 1, index: 3},
		{id: "B-c1", kind: "assistant_text", summary: "B one", parent: "B", turn: 1, index: 4},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if _, _, err := s.DeleteConversationFromTurn("S", 1); err != nil {
		t.Fatal(err)
	}
	holder := holderOf(t, s, "F")
	// Past the reader's cut: a running agent under A, and an unstamped
	// anchor with a child, which a listed thread's recompute would stamp.
	for _, row := range []stampFixtureRow{
		{id: "Z", kind: "tool_call", tool: "Agent", summary: "Agent: Z", parent: "A", status: "running", turn: 5},
		{id: "Y", kind: "tool_call", tool: "Agent", summary: "Agent: Y", turn: 5, index: 1},
		{id: "Y-c1", kind: "assistant_text", summary: "Y one", parent: "Y", turn: 5, index: 2},
	} {
		item := row.item(holder)
		mustExec(t, s.db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, tool_name, meta, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', 1, 1)`,
			item.ID, holder, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Status, item.Summary, item.ParentID, item.ToolName)
	}
	mustExec(t, s.db, `INSERT INTO subagent_aggregate_backfill(thread_id) VALUES (?)`, holder)
	before := holderStampRowsForTest(t, s, holder)
	if got := slices.Sorted(maps.Keys(before)); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("fixture: the holder keeps stamps of %v, want A and B", got)
	}

	if _, err := s.RecoverSubagentCards(context.Background()); err != nil {
		t.Fatalf("boot pass: %v", err)
	}
	if _, err := s.RecomputeSubagentAggregates(context.Background(), holder, 100); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if _, err := s.FlushSubagentCards(holder); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for name, run := range map[string]func(tx *sql.Tx) error{
		"restamp": func(tx *sql.Tx) error { return s.restampSubagentAggregatesTx(tx, holder) },
		"chain recompute": func(tx *sql.Tx) error {
			_, err := s.recomputeSubagentChainsTx(tx, holder, []string{"A-c1"}, []string{"A", "Y"}, []string{"B"}, nil)
			return err
		},
	} {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := run(tx); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s: %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if dirty, err := subagentAnchorIDs(s.reader(), subagentDirtyAnchorsSQL(10), holder); err != nil || len(dirty) > 0 {
		t.Errorf("the holder's dirty anchors = %v, %v", dirty, err)
	}
	if after := holderStampRowsForTest(t, s, holder); !maps.Equal(after, before) {
		t.Errorf("the holder's stamps moved:\n before %v\n  after %v", before, after)
	}
	requireServed(t, s, "F", "A", "B")
	assertForkWalkParity(t, s, "F", "a holder after every maintenance path")
}

// TestPointerForkServesACompletedLaunchCard: a settled background launch
// and its completion sibling, both inherited, serve the launch's stamp,
// the sibling its card.
func TestPointerForkServesACompletedLaunchCard(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "D", kind: "tool_call", tool: "Agent", summary: "Agent: D", status: "running", background: true, turn: 1},
		{id: "D-c1", kind: "tool_call", tool: "Bash", summary: "Bash: d", parent: "D", turn: 1, index: 1},
		{id: "D-c2", kind: "assistant_text", summary: "D two", parent: "D", turn: 1, index: 2},
		{id: "D-done", kind: "tool_completion", tool: "Agent", summary: "done", background: true, completionOf: "D", turn: 1, index: 3},
		{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	requireServed(t, s, "F", "D")
	if card := subagentCardsForTest(t, s, s.reader(), "F")["D-done"]; card[metaKeySubagentDescendantCount] != float64(2) {
		t.Errorf("the inherited completion serves %v, want the launch's card of 2", card)
	}
	assertForkWalkParity(t, s, "F", "a completed background launch")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkCopyMarksTheAnchorsAboveIt: a fork's copy of an
// inherited row is its own to change; a writer without a card changes it
// under an inherited anchor, which the fork then walks.
func TestPointerForkCopyMarksTheAnchorsAboveIt(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		old, err := readMutableSubagentRowTx(tx, "F", "A-c2", "test bulk write")
		if err != nil {
			return err
		}
		next := old
		next.summary = "A two, edited"
		if _, err := tx.Exec(`UPDATE items SET summary = ? WHERE thread_id = 'F' AND id = 'A-c2'`, next.summary); err != nil {
			return err
		}
		return w.updated(old, next)
	}); err != nil {
		t.Fatal(err)
	}
	if ownsRow(t, s, "F", "A") {
		t.Fatal("fixture: the write copied A")
	}
	requireMarkers(t, s, "F", "A")
	requireServed(t, s, "F", "B", "C")
	assertForkWalkParity(t, s, "F", "a changed copy under an inherited anchor")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkLateRowUnderAHeldID: a row the source writes below the
// cut under an id a holder took is hidden from the reader by the holder's
// hide, not its own; the anchors above it in the source are marked too.
func TestPointerForkLateRowUnderAHeldID(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.DeleteThreadItem("S", "A-c1"); err != nil {
		t.Fatal(err)
	}
	holder := holderOf(t, s, "F")
	requireMarkers(t, s, holder, "A")
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: again", parent: "C", turn: 1, index: 5},
	})
	requireMarkers(t, s, "F", "C")
	requireServed(t, s, "F", "B")
	assertForkWalkParity(t, s, "F", "a late row under a held id")
	assertBackfillMatchesWriters(t, s)
}

// TestRetiredForkKeepsItsMarkers: a deleted fork its own fork reads keeps
// its markers with its hides.
func TestRetiredForkKeepsItsMarkers(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "L", kind: "tool_call", tool: "Agent", summary: "Agent: L", parent: "A", status: "running", background: true, turn: 1, index: 1},
		{id: "A-c1", kind: "tool_call", tool: "Bash", summary: "Bash: one", parent: "A", turn: 1, index: 2},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if err := s.DeleteThread("F"); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "A")
	requireServed(t, s, "G")
	assertForkWalkParity(t, s, "G", "a retired fork's reader")
	assertBackfillMatchesWriters(t, s)
}

// TestHolderCopyOfANestedRowMarksItsAnchor: once a fork hides a nested
// row its own fork shows, only the holder's copy shows it, and the source
// may change its own row; the reader walks the anchor above.
func TestHolderCopyOfANestedRowMarksItsAnchor(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "A-c1", kind: "assistant_text", summary: "A one", parent: "A", turn: 1, index: 1},
		{id: "X", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "A", turn: 1, index: 2},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if err := s.DeleteThreadItem("F", "X"); err != nil {
		t.Fatal(err)
	}
	holder := holderOf(t, s, "G")
	requireMarkers(t, s, holder, "A")
	summary := "Bash: x, rewritten"
	if err := s.WithSubagentCard("S", "A", func(card *SubagentCard) error {
		_, err := s.UpdateItemFields("S", "X", ItemPartialUpdate{Summary: &summary, SubagentCard: card})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireServed(t, s, "G")
	assertForkWalkParity(t, s, "G", "a source row only the holder's copy shows")
	assertForkWalkParity(t, s, "F", "the fork that hid the row")
	assertBackfillMatchesWriters(t, s)
}

// TestHolderSettledLaunchCopyMarksItsAnchor: a source revert that takes a
// background launch's completion gives the holder a settled copy of the
// launch, whose parent stays in the source. The source's later change to
// its own launch reaches its stamp of the parent, not the copy the fork
// reads, so the fork walks the parent.
func TestHolderSettledLaunchCopyMarksItsAnchor(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "A-c1", kind: "assistant_text", summary: "A one", parent: "A", turn: 1, index: 1},
		{id: "L", kind: "tool_call", tool: "Bash", summary: "Bash: launch", parent: "A", status: "running", background: true, turn: 1, index: 2},
		{id: "u2", kind: "user_text", summary: "user 2", turn: 2},
		{id: "L-done", kind: "tool_completion", tool: "Bash", summary: "done", background: true, completionOf: "L", turn: 2, index: 1},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	requireServed(t, s, "F", "A")
	if _, _, err := s.DeleteConversationFromTurn("S", 2); err != nil {
		t.Fatal(err)
	}
	holder := holderOf(t, s, "F")
	requireIDs(t, "holder rows", ownIDs(t, s, holder), []string{"L", "u2", "L-done"})
	requireMarkers(t, s, holder, "A")
	summary := "Bash: launch, rewritten"
	if err := s.WithSubagentCard("S", "A", func(card *SubagentCard) error {
		_, err := s.UpdateItemFields("S", "L", ItemPartialUpdate{Summary: &summary, SubagentCard: card})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireServed(t, s, "F")
	assertForkWalkParity(t, s, "F", "a settled launch copy under a source anchor")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkHideBelowAnUncountedRowMarksNothing: an anchor's card
// does not count the rows below a row no card counts, so a fork's hide of
// one of them leaves the anchor's stamp serving the fork.
func TestPointerForkHideBelowAnUncountedRowMarksNothing(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	seedStampRowsForTest(t, s, "S", []stampFixtureRow{
		{id: "A", kind: "tool_call", tool: "Agent", summary: "Agent: A", turn: 1},
		{id: "A-c1", kind: "assistant_text", summary: "A one", parent: "A", turn: 1, index: 1},
		{id: "P", kind: "notification", tool: "plan_update", summary: "plan", parent: "A", turn: 1, index: 2},
		{id: "X", kind: "tool_call", tool: "Bash", summary: "Bash: x", parent: "P", turn: 1, index: 3},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.DeleteThreadItem("F", "X"); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F")
	requireServed(t, s, "F", "A")
	assertForkWalkParity(t, s, "F", "a hide below an uncounted row")
	assertBackfillMatchesWriters(t, s)
}

// TestPointerForkOwnRowCountedUnderAnInheritedAnchor: a fork's own row no
// card counts marks nothing; once a write makes it counted where it
// stands, the anchors above it walk.
func TestPointerForkOwnRowCountedUnderAnInheritedAnchor(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	row := stampFixtureRow{id: "F-p", kind: "notification", tool: "plan_update", summary: "plan", parent: "B", meta: "{}", turn: 3}.item("F")
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		return insertItemTx(tx, w, row, "test bulk write")
	}); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F")
	requireServed(t, s, "F", "A", "B", "C")
	assertForkWalkParity(t, s, "F", "an uncounted fork row under an inherited anchor")
	assertBackfillMatchesWriters(t, s)
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		old, err := readMutableSubagentRowTx(tx, "F", "F-p", "test bulk write")
		if err != nil {
			return err
		}
		next := old
		next.kind, next.toolName = "assistant_text", ""
		if _, err := tx.Exec(`UPDATE items SET kind = 'assistant_text', tool_name = '' WHERE thread_id = 'F' AND id = 'F-p'`); err != nil {
			return err
		}
		return w.updated(old, next)
	}); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "A", "B")
	requireServed(t, s, "F", "C")
	assertForkWalkParity(t, s, "F", "a fork row made counted under an inherited anchor")
	assertBackfillMatchesWriters(t, s)
}

// TestRetiredForkKeepsItsOwnRowMarkers: a deleted fork its own fork reads
// keeps the markers its own rows under inherited anchors recorded, and
// the backfill records them for it though it reads no lineage.
func TestRetiredForkKeepsItsOwnRowMarkers(t *testing.T) {
	s := newTestStore(t)
	seedWalkedForkSource(t, s, "S")
	mustPointerFork(t, s, "S", "F", ForkCut{})
	row := stampFixtureRow{id: "F-c1", kind: "tool_call", tool: "Bash", summary: "Bash: fork", parent: "B", meta: "{}", turn: 3}.item("F")
	if err := s.bulkWriteItems("F", "test bulk write", func(tx *sql.Tx, w *cardWrite) error {
		return insertItemTx(tx, w, row, "test bulk write")
	}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if err := s.DeleteThread("F"); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, s, "F", "A", "B")
	requireServed(t, s, "G", "C")
	assertForkWalkParity(t, s, "G", "a retired fork's own rows")
	assertBackfillMatchesWriters(t, s)
}
