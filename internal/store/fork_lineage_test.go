package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// seedLinearSource creates src with `turns` turns of a user row and a reply,
// u<turn> at item 0 and a<turn> at item 1.
func seedLinearSource(t *testing.T, s *Store, src string, turns int) {
	t.Helper()
	var rows []Item
	for turn := range turns {
		rows = append(rows,
			Item{ID: fmt.Sprintf("u%d", turn), TurnIndex: turn, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: fmt.Sprintf("user %d", turn), Meta: "{}"},
			Item{ID: fmt.Sprintf("a%d", turn), TurnIndex: turn, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: fmt.Sprintf("reply %d", turn), Meta: "{}"},
		)
	}
	seedForkSource(t, s, src, rows)
}

// forkLineage renders a thread's lineage rows as depth:ancestor:cut.
func forkLineage(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT depth, ancestor_id, cut_turn_index, cut_item_index
		FROM thread_fork_lineage WHERE thread_id = ? ORDER BY depth`, threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var depth, turn, item int
		var ancestor string
		if err := rows.Scan(&depth, &ancestor, &turn, &item); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d:%s:%d:%d", depth, ancestor, turn, item))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// timelineShape renders what a reader of the thread sees: every row with its
// position, content and the bytes of the payload it references.
func timelineShape(t *testing.T, s *Store, threadID string) []string {
	t.Helper()
	rows, err := s.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	out := make([]string, 0, len(rows))
	for _, it := range rows {
		line := fmt.Sprintf("%s@%d:%d %s %s %s", it.ID, it.TurnIndex, it.ItemIndex, it.Status, it.Summary, it.Meta)
		if it.PayloadID != "" {
			data, err := s.GetPayloadData(threadID, it.PayloadID)
			if err != nil {
				t.Fatalf("payload %s/%s: %v", threadID, it.PayloadID, err)
			}
			line += " payload=" + string(data)
		}
		out = append(out, line)
	}
	return out
}

func requireIDs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func requireShape(t *testing.T, s *Store, threadID string, want []string) {
	t.Helper()
	if got := timelineShape(t, s, threadID); !slices.Equal(got, want) {
		t.Fatalf("%s changed:\n got %q\nwant %q", threadID, got, want)
	}
}

// TestPointerForkOfALongThreadIsConstantTime: forking a 10,000-row thread
// writes a handful of rows whatever the length, and the fork's first page
// and an agent's page read the source's rows.
func TestPointerForkOfALongThreadIsConstantTime(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "source")
	// 1,000 turns of ten rows; turn 500 holds an agent with seven rows.
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 999)
		INSERT INTO turns(turn_id,thread_id,turn_index,started_at,completed_at) SELECT 'source:' || i, 'source', i, 1, 2 FROM n`)
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 9999)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,parent_id,tool_name,meta,created_at,updated_at)
		SELECT 'source', CASE WHEN i = 5001 THEN 'agent' ELSE 'r' || i END, i / 10, i % 10,
		       CASE WHEN i % 10 = 0 THEN 'user_text' WHEN i = 5001 THEN 'tool_call' ELSE 'assistant_text' END,
		       CASE WHEN i % 10 = 0 THEN 'user' ELSE 'assistant' END,
		       'completed', 'row ' || i,
		       CASE WHEN i BETWEEN 5002 AND 5008 THEN 'agent' ELSE '' END,
		       CASE WHEN i = 5001 THEN 'Task' ELSE '' END, '{}', 1, 1
		  FROM n`)

	// The best of three separates the fork's own cost from scheduler noise
	// on a loaded test machine.
	fastest := time.Hour
	for attempt := range 3 {
		fork := makeThread(fmt.Sprintf("fork-%d", attempt), "claude")
		started := time.Now()
		if err := s.CreatePointerFork(fork, "source", ForkCut{}, testInterruptedSummary, 999); err != nil {
			t.Fatal(err)
		}
		fastest = min(fastest, time.Since(started))
	}
	t.Logf("fork of 10,000 rows: %v", fastest)
	if fastest > 50*time.Millisecond {
		t.Fatalf("fork of 10,000 rows held the writer %v, want under 50ms", fastest)
	}
	var ownTurns int
	if err := s.db.QueryRow(`SELECT count(*) FROM turns WHERE thread_id = 'fork-0'`).Scan(&ownTurns); err != nil {
		t.Fatal(err)
	}
	if n := ownRowCount(t, s, "fork-0"); n != 1 || ownTurns != 1 {
		t.Fatalf("fork stores %d rows and %d turns, want its divider and its cut turn", n, ownTurns)
	}

	ctx := context.Background()
	started := time.Now()
	forkPage, err := s.ListThreadSliceAround(ctx, "fork-0", "", 40, testRunWindowRows, TimelineSelection{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fork first page: %v", time.Since(started))
	sourcePage, err := s.ListThreadSliceAround(ctx, "source", "", 40, testRunWindowRows, TimelineSelection{})
	if err != nil {
		t.Fatal(err)
	}
	forkIDs, sourceIDs := itemIDs(forkPage.Items), itemIDs(sourcePage.Items)
	if len(forkIDs) < 2 || forkIDs[len(forkIDs)-1] != forkDividerID("fork-0") || !forkPage.HasMoreOlder {
		t.Fatalf("fork page = %v older=%v, want the source's tail then the divider", forkIDs, forkPage.HasMoreOlder)
	}
	inherited := forkIDs[:len(forkIDs)-1]
	requireIDs(t, "fork page", inherited, sourceIDs[len(sourceIDs)-len(inherited):])
	for _, it := range forkPage.Items[:len(inherited)] {
		if it.ThreadID != "fork-0" {
			t.Fatalf("inherited row %s reads as thread %s", it.ID, it.ThreadID)
		}
	}

	agent := TimelineSelection{ScopeRootID: "agent"}
	started = time.Now()
	forkScoped, err := s.ListThreadSliceAround(ctx, "fork-0", "", 40, testRunWindowRows, agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fork agent page: %v", time.Since(started))
	sourceScoped, err := s.ListThreadSliceAround(ctx, "source", "", 40, testRunWindowRows, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceScoped.Items) != 7 {
		t.Fatalf("source agent page has %d rows, want 7", len(sourceScoped.Items))
	}
	requireIDs(t, "fork agent page", itemIDs(forkScoped.Items), itemIDs(sourceScoped.Items))
}

// TestPointerForkOfAFork: a fork of a fork points at its immediate source
// and reads its source's source through the flattened lineage, with the
// nearer cut applied. Rows the source places below the cuts later are not
// part of either fork's history.
func TestPointerForkOfAFork(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 4)
	mustPointerFork(t, s, "S", "F", throughTurn(2))
	if _, err := appendCarded(s, Item{ID: "f3", ThreadID: "F", TurnIndex: 3, Kind: "user_text", Role: "user", Status: "completed", Summary: "fork 3"}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "F", "G", ForkCut{})

	requireIDs(t, "G lineage", forkLineage(t, s, "G"), []string{"1:F:3:1", "2:S:2:2"})
	thread, err := s.GetThread("G")
	if err != nil || thread.ForkedFromThreadID != "F" {
		t.Fatalf("G forked from %q, %v; want F", thread.ForkedFromThreadID, err)
	}
	want := []string{"u0", "a0", "u1", "a1", "u2", "a2", forkDividerID("F"), "f3"}
	requireIDs(t, "G rows", itemIDs(forkRows(t, s, "G")), want)
	if _, origin := forkDivider(t, s, "G"); origin.SourceThreadID != "F" || origin.SourceItemID != "f3" || origin.SourceDeleted {
		t.Fatalf("G divider = %+v", origin)
	}
	if n := ownRowCount(t, s, "G"); n != 1 {
		t.Fatalf("G stores %d rows, want its divider", n)
	}

	// A late row the source writes below both cuts (a background child, a
	// completion) and a row it moves there are hidden from both forks.
	if err := insertCarded(s, Item{ID: "late", ThreadID: "S", TurnIndex: 1, ItemIndex: 5, Kind: "assistant_text", Role: "assistant", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `UPDATE items SET turn_index = 1, item_index = 6 WHERE thread_id = 'S' AND id = 'u3'`)
	requireIDs(t, "S rows", itemIDs(forkRows(t, s, "S")), []string{"u0", "a0", "u1", "a1", "late", "u3", "u2", "a2", "a3"})
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0", "u1", "a1", "u2", "a2", "f3"})
	requireIDs(t, "G rows after source writes", itemIDs(forkRows(t, s, "G")), want)
}

// TestPointerForkChainDepthIsCapped: 32 levels read through; the 33rd fork
// is refused and leaves no thread behind.
func TestPointerForkChainDepthIsCapped(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "c0", 1)
	for level := 1; level <= forkLineageMaxDepth; level++ {
		mustPointerFork(t, s, fmt.Sprintf("c%d", level-1), fmt.Sprintf("c%d", level), ForkCut{})
	}
	deepest := fmt.Sprintf("c%d", forkLineageMaxDepth)
	if depth, err := forkLineageDepth(s.db, deepest); err != nil || depth != forkLineageMaxDepth {
		t.Fatalf("depth=%d err=%v", depth, err)
	}
	rows := itemIDs(forkRows(t, s, deepest))
	if len(rows) != 2+forkLineageMaxDepth-1 || rows[0] != "u0" || rows[1] != "a0" {
		t.Fatalf("deepest fork reads %v", rows)
	}
	page, err := s.ListThreadSliceAround(context.Background(), deepest, "", 100, testRunWindowRows, TimelineSelection{})
	if err != nil || len(page.Items) != len(rows)+1 {
		t.Fatalf("deepest page = %d rows, %v", len(page.Items), err)
	}
	err = s.CreatePointerFork(makeThread("too-deep", "claude"), deepest, ForkCut{}, testInterruptedSummary, 999)
	if !errors.Is(err, ErrForkChainTooDeep) {
		t.Fatalf("fork beyond the cap: %v", err)
	}
	if _, err := s.GetThread("too-deep"); err == nil {
		t.Fatal("refused fork left a thread row")
	}
}

// TestPointerForkSourceDeletion: the history belongs to the source and goes
// with it. A fork keeps its own rows, and its divider records that the
// source is gone and its title, so no read has to look for it. A fork of the
// fork stops reading at the fork. Deleting a source directly is refused.
func TestPointerForkSourceDeletion(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 2)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if _, err := appendCarded(s, Item{ID: "f2", ThreadID: "F", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "fork 2"}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "F", "G", ForkCut{})
	if _, err := s.db.Exec(`DELETE FROM threads WHERE id = 'S'`); err == nil || !strings.Contains(err.Error(), "detach them before deleting it") {
		t.Fatalf("deleting a source that forks read: %v", err)
	}
	held := historyStampOf(t, s, "G")

	if err := s.DeleteThread("S"); err != nil {
		t.Fatal(err)
	}
	if _, origin := forkDivider(t, s, "F"); !origin.SourceDeleted || origin.SourceTitle != "Thread S" || origin.SourceThreadID != "S" {
		t.Fatalf("F divider = %+v", origin)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"f2"})
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), nil)
	requireIDs(t, "G rows", itemIDs(forkRows(t, s, "G")), []string{forkDividerID("F"), "f2"})
	requireIDs(t, "G lineage", forkLineage(t, s, "G"), []string{"1:F:2:1"})
	if _, origin := forkDivider(t, s, "G"); origin.SourceDeleted {
		t.Fatalf("G divider = %+v, its source still exists", origin)
	}
	if origin := dividerOrigin(t, s, "G", "F"); !origin.SourceDeleted || origin.SourceTitle != "Thread S" {
		t.Fatalf("G reads F's divider as %+v", origin)
	}
	sync, err := s.SyncThreadWindow(context.Background(), "G", "", 200, testRunWindowRows, held, nil, TimelineSelection{})
	if err != nil || sync.Status != SyncRewritten || sync.Page == nil {
		t.Fatalf("G sync = %s, %v; rows left its timeline", sync.Status, err)
	}
	requireIDs(t, "G page", itemIDs(sync.Page.Items), []string{forkDividerID("F"), "f2", forkDividerID("G")})

	if err := s.DeleteThread("F"); err != nil {
		t.Fatal(err)
	}
	if _, origin := forkDivider(t, s, "G"); !origin.SourceDeleted || origin.SourceTitle != "Thread F" {
		t.Fatalf("G divider = %+v", origin)
	}
	requireIDs(t, "G rows", itemIDs(forkRows(t, s, "G")), nil)
}

// TestPointerForkNeverReadsAPartlyDeletedSource: a long source drains in
// chunks, and its forks are detached before the first one, so between
// chunks a fork shows its own rows, never part of the source's history.
func TestPointerForkNeverReadsAPartlyDeletedSource(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	mustExec(t, s.db, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 1199)
		INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
		SELECT 'S', 'r' || i, i / 10, i % 10, 'assistant_text', 'assistant', 'completed', 'row', '{}', 1, 1 FROM n`)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	pauses := 0
	if err := s.DeleteThreadPaced("S", func() {
		pauses++
		if rows := forkRows(t, s, "F"); len(rows) != 0 {
			t.Errorf("pause %d: F reads %d rows of a source being deleted", pauses, len(rows))
		}
		if _, origin := forkDivider(t, s, "F"); !origin.SourceDeleted {
			t.Errorf("pause %d: F divider = %+v", pauses, origin)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if pauses == 0 {
		t.Fatal("the source drained in one chunk; the fixture must span several")
	}
}

// TestPointerForkSourceEmptiedThenCleanedUp: a source whose history was
// reverted away handed that history to its forks first, so the empty-draft
// cleanup that deletes it leaves them whole. A cleanup that keeps the thread
// leaves its forks linked.
func TestPointerForkSourceEmptiedThenCleanedUp(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 1)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if _, _, err := s.DeleteConversationFromTurn("S", 0); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
	if deleted, err := s.DeleteEmptyDraftThread("S"); err != nil || !deleted {
		t.Fatalf("cleanup deleted=%v err=%v", deleted, err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
	if _, origin := forkDivider(t, s, "F"); !origin.SourceDeleted {
		t.Fatalf("F divider = %+v", origin)
	}

	seedLinearSource(t, s, "kept", 1)
	mustPointerFork(t, s, "kept", "reader", ForkCut{})
	if deleted, err := s.DeleteEmptyDraftThread("kept"); err != nil || deleted {
		t.Fatalf("cleanup of a thread with history deleted=%v err=%v", deleted, err)
	}
	requireIDs(t, "reader lineage", forkLineage(t, s, "reader"), []string{"1:kept:0:2"})
	if _, origin := forkDivider(t, s, "reader"); origin.SourceDeleted {
		t.Fatalf("reader divider = %+v", origin)
	}
}

// TestPointerForkRevertBeforeTheCutRetracts: a fork's revert of rows it
// inherits lowers its cut instead of copying or deleting them. The source
// and a fork of the fork read what they read before.
// lastRowThroughTurnTx probes the turns at or below its bound newest first
// and finishes with one ordered read after lastRowProbeTurns empty turns.
// The rows sit in a fork's source, so every probe reads the lineage arms.
func TestLastRowThroughTurnFindsSparseRows(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "S", []Item{
		{ID: "early", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "early", Meta: "{}"},
		{ID: "early-child", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "early", Summary: "child", Meta: "{}"},
		{ID: "late", TurnIndex: 9, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "late", Meta: "{}"},
	})
	mustPointerFork(t, s, "S", "F", ForkCut{})
	topLevel := "items.parent_id = '' AND items.id <> ?"
	for _, tc := range []struct {
		name    string
		maxTurn int
		where   string
		args    []any
		want    string
	}{
		{"bound turn holds it", 9, topLevel, []any{forkDividerID("F")}, "late"},
		{"past the probes", 8, topLevel, []any{forkDividerID("F")}, "early"},
		{"within the probes", 3, topLevel, []any{forkDividerID("F")}, "early"},
		{"filtered past the probes", 9, topLevel + " AND items.id <> ?", []any{forkDividerID("F"), "late"}, "early"},
		{"none", 9, "items.id = ?", []any{"missing"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			row, found, err := lastRowThroughTurnTx(tx, "F", tc.maxTurn, tc.where, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if found != (tc.want != "") || row.id != tc.want {
				t.Fatalf("last row = %+v found=%v, want %q", row, found, tc.want)
			}
		})
	}
}

func TestPointerForkRevertBeforeTheCutRetracts(t *testing.T) {
	for _, mode := range []string{"turn", "item"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			seedLinearSource(t, s, "S", 4)
			mustPointerFork(t, s, "S", "F", ForkCut{})
			mustPointerFork(t, s, "F", "G", ForkCut{})
			source, grandchild := timelineShape(t, s, "S"), timelineShape(t, s, "G")
			sourceStamp := historyStampOf(t, s, "S")

			var err error
			if mode == "turn" {
				_, _, err = s.DeleteConversationFromTurn("F", 2)
			} else {
				_, _, err = s.DeleteConversationFromItem("F", "u2")
			}
			if err != nil {
				t.Fatal(err)
			}
			requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0", "u1", "a1"})
			requireIDs(t, "F lineage", forkLineage(t, s, "F"), []string{"1:S:1:2"})
			if n := ownRowCount(t, s, "F"); n != 1 {
				t.Fatalf("F stores %d rows, want its divider", n)
			}
			divider, origin := forkDivider(t, s, "F")
			if divider.TurnIndex != 1 || divider.ItemIndex != 2 || origin.SourceItemID != "a1" {
				t.Fatalf("F divider at %d:%d -> %s", divider.TurnIndex, divider.ItemIndex, origin.SourceItemID)
			}
			var cutTurn, cutItem int
			if err := s.db.QueryRow(`SELECT fork_cut_turn_index, fork_cut_item_index FROM threads WHERE id = 'F'`).Scan(&cutTurn, &cutItem); err != nil || cutTurn != 1 || cutItem != 2 {
				t.Fatalf("F cut %d:%d, %v", cutTurn, cutItem, err)
			}
			requireShape(t, s, "S", source)
			if after := historyStampOf(t, s, "S"); after != sourceStamp {
				t.Fatalf("source stamp moved %+v -> %+v", sourceStamp, after)
			}
			requireShape(t, s, "G", grandchild)

			// Reverting everything the fork inherits unlinks it.
			if _, _, err := s.DeleteConversationFromTurn("F", 0); err != nil {
				t.Fatal(err)
			}
			requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), nil)
			requireIDs(t, "F timeline", timelineShape(t, s, "F"), nil)
			requireIDs(t, "F lineage", forkLineage(t, s, "F"), nil)
			requireShape(t, s, "S", source)
			requireShape(t, s, "G", grandchild)
		})
	}
}

// TestPointerForkCopiesEmptyPayloads: a tool result with no output has a
// zero-length payload. Every copy a fork makes of an inherited row
// (materializing its history, its own edit, the source's edit handing the
// row off) must store that payload as a zero-length blob, whether the
// source holds it locally or in imported history; a copy that bound the
// scanned bytes would bind nil and fail payloads.data NOT NULL.
func TestPointerForkCopiesEmptyPayloads(t *testing.T) {
	edit := func(thread string) func(*testing.T, *Store) {
		return func(t *testing.T, s *Store) {
			summary := "edited"
			if _, err := s.UpdateItemFields(thread, "empty-tool", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}
	}
	copies := []struct {
		name string
		copy func(*testing.T, *Store)
	}{
		{"materialize", func(t *testing.T, s *Store) {
			if err := s.MaterializeForkHistory(context.Background(), "F"); err != nil {
				t.Fatal(err)
			}
		}},
		{"fork edit", edit("F")},
		{"source edit", edit("S")},
	}
	for _, imported := range []bool{false, true} {
		for _, c := range copies {
			t.Run(fmt.Sprintf("%s imported=%v", c.name, imported), func(t *testing.T) {
				s := newTestStore(t)
				seedLinearSource(t, s, "S", 2)
				if imported {
					if err := insertWithPayloadCarded(s,
						Item{ID: "empty-tool", ThreadID: "S", TurnIndex: 0, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", PayloadID: "pe", Meta: "{}"},
						Payload{ID: "pe", Kind: "text", Meta: "{}", Data: []byte("x")},
					); err != nil {
						t.Fatal(err)
					}
					sealItemsForTest(t, s, "S", "u0", "a0", "empty-tool")
					mustExec(t, s.db, `UPDATE import_history_payloads SET data = x'' WHERE id = 'pe'`)
				} else {
					mustExec(t, s.db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('S','pe','text','{}',x'',1)`)
					if err := insertCarded(s, Item{ID: "empty-tool", ThreadID: "S", TurnIndex: 0, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", PayloadID: "pe", Meta: "{}"}); err != nil {
						t.Fatal(err)
					}
				}
				mustPointerFork(t, s, "S", "F", ForkCut{})
				c.copy(t, s)

				var kind string
				var length int
				if err := s.db.QueryRow(`SELECT typeof(data), length(data) FROM payloads WHERE thread_id = 'F' AND id = 'pe'`).Scan(&kind, &length); err != nil {
					t.Fatalf("F owns no copy of the empty payload: %v", err)
				}
				if kind != "blob" || length != 0 {
					t.Fatalf("F's copy of the empty payload is a %d-byte %s", length, kind)
				}
				data, err := s.GetPayloadData("F", "pe")
				if err != nil || len(data) != 0 {
					t.Fatalf("F reads the empty payload as %q, %v", data, err)
				}
			})
		}
	}
}

// TestPointerForkMaterializes: a fork that must own its history (the
// transfer export) copies every row it reads and drops its lineage, and
// reads the same before and after. A fork of it reads what it read before.
func TestPointerForkMaterializes(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 3)
	if err := insertWithPayloadCarded(s,
		Item{ID: "tool", ThreadID: "S", TurnIndex: 1, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: "p", Meta: "{}"},
		Payload{ID: "p", Kind: "text", Meta: "{}", Data: []byte("output")},
	); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", throughTurn(1))
	if _, err := appendCarded(s, Item{ID: "f2", ThreadID: "F", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "fork 2"}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "F", "G", ForkCut{})
	fork, grandchild, source := timelineShape(t, s, "F"), timelineShape(t, s, "G"), timelineShape(t, s, "S")

	for range 2 {
		if err := s.MaterializeForkHistory(context.Background(), "F"); err != nil {
			t.Fatal(err)
		}
	}
	requireShape(t, s, "F", fork)
	requireShape(t, s, "G", grandchild)
	requireShape(t, s, "S", source)
	requireIDs(t, "F lineage", forkLineage(t, s, "F"), nil)
	if n := ownRowCount(t, s, "F"); n != len(fork) {
		t.Fatalf("F stores %d of its %d rows", n, len(fork))
	}
	var payloads, turns, timelineTurns int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM payloads WHERE thread_id = 'F'),
		(SELECT count(*) FROM turns WHERE thread_id = 'F'), (SELECT count(*) FROM timeline_turns WHERE thread_id = 'F')`).Scan(&payloads, &turns, &timelineTurns); err != nil {
		t.Fatal(err)
	}
	if payloads != 1 || turns != timelineTurns {
		t.Fatalf("F stores %d payloads and %d of %d turns", payloads, turns, timelineTurns)
	}

	var exported bytes.Buffer
	if err := s.ExportThreadHistory(context.Background(), "G", &exported); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "G lineage after export", forkLineage(t, s, "G"), nil)
	if n := ownRowCount(t, s, "G"); n != len(grandchild) {
		t.Fatalf("exported G stores %d of its %d rows", n, len(grandchild))
	}
	for _, id := range []string{"S", "F"} {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
	}
	// G's history is its own now. Its divider and its copy of F's record
	// that their sources are gone.
	after := timelineShape(t, s, "G")
	if len(after) != len(grandchild) {
		t.Fatalf("G = %q, want %q", after, grandchild)
	}
	for i, line := range grandchild {
		if !strings.HasPrefix(line, "fork-origin-") && after[i] != line {
			t.Fatalf("G row %d = %q, want %q", i, after[i], line)
		}
	}
	for owner, title := range map[string]string{"G": "Thread F", "F": "Thread S"} {
		if origin := dividerOrigin(t, s, "G", owner); !origin.SourceDeleted || origin.SourceTitle != title {
			t.Fatalf("G's divider of %s = %+v", owner, origin)
		}
	}
}

// dividerOrigin reads the divider owner placed at its cut, as thread shows
// it: its own, inherited, or copied.
func dividerOrigin(t *testing.T, s *Store, thread, owner string) forkOrigin {
	t.Helper()
	item, found, err := s.GetThreadItem(thread, forkDividerID(owner))
	if err != nil || !found {
		t.Fatalf("%s's divider of %s: found=%v err=%v", thread, owner, found, err)
	}
	var origin forkOrigin
	if err := json.Unmarshal([]byte(item.Meta), &origin); err != nil {
		t.Fatal(err)
	}
	return origin
}

// forkHandOffFixture is S with two turns and a payload row, a fork F of it
// and a fork G of F.
func forkHandOffFixture(t *testing.T) *Store {
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

// TestPointerForkSourceRewritesHandOff: a source that changes, moves or
// deletes a row a fork reads gives the fork its own copy first, so the
// fork's history is what it was when it was made.
func TestPointerForkSourceRewritesHandOff(t *testing.T) {
	for name, rewrite := range map[string]func(*Store) error{
		"delete row":       func(s *Store) error { return s.DeleteThreadItem("S", "a0") },
		"rewrite meta":     func(s *Store) error { return s.UpdateItemMeta("S", "u0", `{"changed":true}`) },
		"move row":         func(s *Store) error { _, err := s.BumpItemToTurnEnd("S", "u1", nil, 5); return err },
		"revert turns":     func(s *Store) error { _, _, err := s.DeleteConversationFromTurn("S", 1); return err },
		"revert from item": func(s *Store) error { _, _, err := s.DeleteConversationFromItem("S", "a0"); return err },
		"append payload":   func(s *Store) error { return s.AppendPayloadData("S", "p", []byte(" more"), "{}", 5) },
		"replace payload":  func(s *Store) error { return s.ReplacePayloadData("S", "p", []byte("new"), "{}", 5) },
	} {
		t.Run(name, func(t *testing.T) {
			s := forkHandOffFixture(t)
			source, fork, grandchild := timelineShape(t, s, "S"), timelineShape(t, s, "F"), timelineShape(t, s, "G")
			if err := rewrite(s); err != nil {
				t.Fatal(err)
			}
			if slices.Equal(timelineShape(t, s, "S"), source) {
				t.Fatal("the rewrite did not change the source")
			}
			requireShape(t, s, "F", fork)
			requireShape(t, s, "G", grandchild)
		})
	}
}

// TestPointerForkHandOffReachesADeeperReader: a fork that lowered its cut no
// longer reads the rows past it, but a fork made from it earlier still does
// through its own lineage, and gets the copy when the source rewrites them.
func TestPointerForkHandOffReachesADeeperReader(t *testing.T) {
	s := forkHandOffFixture(t)
	grandchild := timelineShape(t, s, "G")
	if _, _, err := s.DeleteConversationFromTurn("F", 1); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
	requireShape(t, s, "G", grandchild)
	if err := s.DeleteThreadItem("S", "a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateItemMeta("S", "u1", `{"changed":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPayloadData("S", "p", []byte(" more"), "{}", 5); err != nil {
		t.Fatal(err)
	}
	requireShape(t, s, "G", grandchild)
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0"})
}

// TestPointerForkCopyOnWriteHandsOffToItsForks: a fork's write to a row or
// payload it inherits copies it into the fork, after handing the original
// to the forks that read through it; the source is untouched.
func TestPointerForkCopyOnWriteHandsOffToItsForks(t *testing.T) {
	s := forkHandOffFixture(t)
	source, grandchild := timelineShape(t, s, "S"), timelineShape(t, s, "G")
	if err := s.UpdateItemMeta("F", "u0", `{"changed":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPayloadData("F", "p", []byte(" fork"), "{}", 5); err != nil {
		t.Fatal(err)
	}
	requireShape(t, s, "S", source)
	requireShape(t, s, "G", grandchild)
	if got, err := s.GetPayloadData("F", "p"); err != nil || string(got) != "output fork" {
		t.Fatalf("F payload = %q, %v", got, err)
	}
	item, found, err := s.GetThreadItem("F", "u0")
	if err != nil || !found || item.Meta != `{"changed":true}` {
		t.Fatalf("F u0 meta = %q found=%v err=%v", item.Meta, found, err)
	}
}

// TestPointerForkPositionsStayOnTheirSideOfTheCut: a fork's own rows sit
// after its cut, and a row the source places inside the cut after the fork
// was made is not the fork's history.
func TestPointerForkPositionsStayOnTheirSideOfTheCut(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 3)
	mustPointerFork(t, s, "S", "F", throughTurn(1))
	err := insertCarded(s, Item{ID: "early", ThreadID: "F", TurnIndex: 0, ItemIndex: 7, Kind: "user_text", Role: "user", Status: "completed"})
	if err == nil || !strings.Contains(err.Error(), "precedes the fork cut") {
		t.Fatalf("insert below the cut: %v", err)
	}
	if _, err := appendCarded(s, Item{ID: "f2", ThreadID: "F", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE items SET turn_index = 0, item_index = 9 WHERE thread_id = 'F' AND id = 'f2'`); err == nil || !strings.Contains(err.Error(), "precedes the fork cut") {
		t.Fatalf("move below the cut: %v", err)
	}

	head, err := s.UpsertItemAtTurnHead(Item{ID: "head", ThreadID: "S", TurnIndex: 0, Kind: "user_text", Role: "user", Status: "completed"})
	if err != nil || head.ItemIndex != -1 {
		t.Fatalf("head row at %d: %v", head.ItemIndex, err)
	}
	requireIDs(t, "S turn 0", itemIDs(forkRows(t, s, "S"))[:3], []string{"head", "u0", "a0"})
	requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0", "u1", "a1", "f2"})
}

// TestPointerForkStampsAndHeldWindows: a source write after a fork's cut
// leaves the fork fresh. A write that reaches a row the fork shows moves its
// stamp, and a window of the fork's own rows still verifies; inherited rows
// carry no row stamp, so a window that holds them does not. A hand-off
// leaves the fork's rows as they were.
func TestPointerForkStampsAndHeldWindows(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 2)
	mustPointerFork(t, s, "S", "F", ForkCut{})
	for _, id := range []string{"f1", "f2"} {
		if _, err := appendCarded(s, Item{ID: id, ThreadID: "F", TurnIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: id}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	page, err := s.ListThreadSliceAround(ctx, "F", "", 200, testRunWindowRows, TimelineSelection{})
	if err != nil {
		t.Fatal(err)
	}
	own := page.Items[len(page.Items)-3:]
	requireIDs(t, "F tail", itemIDs(own), []string{forkDividerID("F"), "f1", "f2"})
	for _, it := range page.Items[:len(page.Items)-3] {
		if it.Rev != -1 {
			t.Fatalf("inherited row %s rev=%d", it.ID, it.Rev)
		}
	}
	tail := heldWindowOverItems(own, true, false)
	whole := heldWindowOverItems(page.Items, false, false)
	stamp := historyStampOf(t, s, "F")
	sync := func(held *HeldWindow) SyncStatus {
		t.Helper()
		got, err := s.SyncThreadWindow(ctx, "F", "", 200, testRunWindowRows, stamp, held, TimelineSelection{})
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	if got := sync(&whole); got != SyncFresh {
		t.Fatalf("unchanged fork = %s", got)
	}

	if _, err := appendCarded(s, Item{ID: "late", ThreadID: "S", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if got := sync(&whole); got != SyncFresh {
		t.Fatalf("fork after a source write past its cut = %s, want fresh", got)
	}

	touchItemForTest(t, s, "S", "a1")
	if historyStampOf(t, s, "F") == stamp {
		t.Fatal("a revision touch of a row the fork shows left the fork's stamp")
	}
	if got := sync(nil); got != SyncStale {
		t.Fatalf("fork after a touch of a row it shows = %s, want stale", got)
	}
	if got := sync(&tail); got != SyncFresh {
		t.Fatalf("fork's own window after a touch of an inherited row = %s, want fresh", got)
	}
	if got := sync(&whole); got == SyncFresh {
		t.Fatal("verified a window holding inherited rows")
	}

	before := timelineShape(t, s, "F")
	if err := s.DeleteThreadItem("S", "a0"); err != nil {
		t.Fatal(err)
	}
	requireShape(t, s, "F", before)
	copied, found, err := s.GetThreadItem("F", "a0")
	if err != nil || !found || copied.Rev < 0 {
		t.Fatalf("handed-off row rev=%d found=%v err=%v", copied.Rev, found, err)
	}
}

// touchItemForTest is a revision touch of one row, the write a plan badge or
// a plan comment makes (bumpHistoryRevForItemTx).
func touchItemForTest(t *testing.T, s *Store, threadID, itemID string) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := bumpHistoryRevForItemTx(tx, threadID, itemID, "test touch"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// forkWindow is a thread's decorated window read.
func forkWindow(t *testing.T, s *Store, threadID string) []Item {
	t.Helper()
	page, err := s.ListThreadSliceAround(context.Background(), threadID, "", 200, testRunWindowRows, TimelineSelection{})
	if err != nil {
		t.Fatalf("window of %s: %v", threadID, err)
	}
	return page.Items
}

// TestForkStampIgnoresSourceWritesPastTheCut: a fork's stamps are its own.
// A source write after the fork's cut leaves them and the fork's window as
// they were, including a write whose row stamping reaches a settled row
// below the cut. A write that changes a row the fork shows moves them: a
// content change through the hand-off's copy, a revision touch, spans on a
// payload the fork shows, and a source deletion marking a divider the fork
// shows from a materialized fork.
func TestForkStampIgnoresSourceWritesPastTheCut(t *testing.T) {
	s := newTestStore(t)
	seedLinearSource(t, s, "S", 3)
	for _, it := range []Item{
		{ID: "root", ThreadID: "S", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Task", Summary: "Task", Meta: "{}"},
		{ID: "c1", ThreadID: "S", TurnIndex: 1, ItemIndex: 3, ParentID: "root", Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "child 1", Meta: "{}"},
	} {
		if err := insertCarded(s, it); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertWithPayloadCarded(s,
		Item{ID: "tl", ThreadID: "S", TurnIndex: 1, ItemIndex: 4, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", PayloadID: "pt", Meta: "{}"},
		Payload{ID: "pt", Kind: "text", Meta: "{}", Data: []byte("out")},
	); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "S", "F", throughTurn(1))
	mustPointerFork(t, s, "F", "G", ForkCut{})

	type view struct {
		stamp  HistoryStamp
		window []Item
	}
	read := func() map[string]view {
		out := map[string]view{}
		for _, id := range []string{"F", "G"} {
			out[id] = view{historyStampOf(t, s, id), forkWindow(t, s, id)}
		}
		return out
	}
	// requireViews fails a fork in moved whose stamp stayed, and a fork not
	// in moved whose stamp or window changed.
	requireViews := func(what string, before, after map[string]view, moved ...string) {
		t.Helper()
		for id, was := range before {
			now := after[id]
			if slices.Contains(moved, id) {
				if now.stamp.Rev <= was.stamp.Rev {
					t.Errorf("%s left %s's stamp at %+v", what, id, now.stamp)
				}
				continue
			}
			if now.stamp != was.stamp {
				t.Errorf("%s moved %s's stamp %+v -> %+v", what, id, was.stamp, now.stamp)
			}
			if !reflect.DeepEqual(now.window, was.window) {
				t.Errorf("%s changed %s's window\n got %+v\nwant %+v", what, id, now.window, was.window)
			}
		}
	}
	rootRev := func() int64 {
		t.Helper()
		root, found, err := s.GetThreadItem("S", "root")
		if err != nil || !found {
			t.Fatalf("source root: found=%v err=%v", found, err)
		}
		return root.Rev
	}

	summary := "reply 2 edited"
	for _, write := range []struct {
		name string
		run  func()
	}{
		{"a source append", func() {
			if _, err := appendCarded(s, Item{ID: "late", ThreadID: "S", TurnIndex: 3, Kind: "user_text", Role: "user", Status: "completed", Summary: "late", Meta: "{}"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a source update past the cut", func() {
			if _, err := s.UpdateItemFields("S", "a2", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a source delete past the cut", func() {
			if err := s.DeleteThreadItem("S", "u2"); err != nil {
				t.Fatal(err)
			}
		}},
		{"a child past the cut under a root below it", func() {
			was := rootRev()
			if err := insertCarded(s, Item{ID: "c2", ThreadID: "S", TurnIndex: 3, ItemIndex: 1, ParentID: "root", Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "child 2", Meta: "{}"}); err != nil {
				t.Fatal(err)
			}
			if rootRev() == was {
				t.Fatal("the child did not stamp its root below the cut")
			}
		}},
		{"spans on a source payload past the cut", func() {
			if err := insertWithPayloadCarded(s,
				Item{ID: "tl2", ThreadID: "S", TurnIndex: 3, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Bash", PayloadID: "pl", Meta: "{}"},
				Payload{ID: "pl", Kind: "text", Meta: "{}", Data: []byte("late out")},
			); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdatePayloadSpans("S", "pl", `{"late":1}`, `{"late":2}`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		before := read()
		write.run()
		requireViews(write.name, before, read())
	}

	before := read()
	if err := s.UpdatePayloadSpans("F", "pt", `{"preview":1}`, `{"full":1}`); err != nil {
		t.Fatal(err)
	}
	requireViews("spans on a payload the forks show", before, read(), "F", "G")

	before = read()
	touchItemForTest(t, s, "S", "a1")
	requireViews("a revision touch of a row the forks show", before, read(), "F", "G")

	// F takes a copy of a0 before the source changes it. G reads F's copy
	// in place of the source's row, the same row.
	before = read()
	edited := "reply 0 edited"
	if _, err := s.UpdateItemFields("S", "a0", ItemPartialUpdate{Summary: &edited}); err != nil {
		t.Fatal(err)
	}
	requireViews("a source update below the cut", before, read(), "F")

	// H materialized, so deleting its source reaches R, which reads H's
	// divider, only through the divider's in-place mark.
	seedLinearSource(t, s, "S2", 2)
	mustPointerFork(t, s, "S2", "H", ForkCut{})
	if err := s.MaterializeForkHistory(context.Background(), "H"); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "H", "R", ForkCut{})
	was := historyStampOf(t, s, "R")
	if err := s.DeleteThread("S2"); err != nil {
		t.Fatal(err)
	}
	if now := historyStampOf(t, s, "R"); now.Rev <= was.Rev {
		t.Errorf("deleting H's source left R's stamp at %+v while R shows H's divider", now)
	}
	divider, found, err := s.GetThreadItem("R", forkDividerID("H"))
	if err != nil || !found || !strings.Contains(divider.Meta, `"sourceDeleted":true`) {
		t.Fatalf("R's view of H's divider = %+v found=%v err=%v", divider, found, err)
	}
}

// TestPointerForkSearchFollowsTheCut: a fork's search finds the messages it
// inherits, only below its cut and only while it shows them.
func TestPointerForkSearchFollowsTheCut(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "S", []Item{
		{ID: "u0", Kind: "user_text", Role: "user", Status: "completed", Summary: "needle zero"},
		{ID: "u1", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "needle one"},
	})
	mustPointerFork(t, s, "S", "F", throughTurn(0))
	search := func(thread string) []string {
		t.Helper()
		hits, err := s.SearchThreads("needle", ThreadSearchFilter{ThreadIDs: []string{thread}, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, hit := range hits {
			if hit.ThreadID != thread {
				t.Fatalf("search of %s returned a hit in %s", thread, hit.ThreadID)
			}
			ids = append(ids, hit.ItemID)
		}
		slices.Sort(ids)
		return ids
	}
	requireIDs(t, "F hits", search("F"), []string{"u0"})
	requireIDs(t, "S hits", search("S"), []string{"u0", "u1"})
	if err := s.DeleteThreadItem("F", "u0"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F hits after hiding", search("F"), nil)
	requireIDs(t, "S hits after the fork hid", search("S"), []string{"u0", "u1"})
}

// TestDeletingAPointerForkLeavesNothing: a throwaway fork (thread_ask, a
// failed fork) removes every row it wrote, and its source reads exactly as
// it did.
func TestDeletingAPointerForkLeavesNothing(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	questionFixture(t, s, "S", "question")
	if err := s.InsertAttachment(Attachment{ID: "att", ThreadID: "S", Kind: AttachmentKindImage, Filename: "a.png", MimeType: "image/png", RelativePath: "S/att.png"}); err != nil {
		t.Fatal(err)
	}
	if err := insertCarded(s, Item{ID: "prompt", ThreadID: "S", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "look", Meta: `{"attachments":["att"]}`}); err != nil {
		t.Fatal(err)
	}
	if err := insertWithPayloadCarded(s,
		Item{ID: "tool", ThreadID: "S", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", PayloadID: "p", Meta: "{}"},
		Payload{ID: "p", Kind: "text", Meta: "{}", Data: []byte("output")},
	); err != nil {
		t.Fatal(err)
	}
	source := timelineShape(t, s, "S")
	stamp := historyStampOf(t, s, "S")

	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := s.UpdateItemMeta("F", "prompt", `{"attachments":["att"],"edited":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThreadItem("F", "question"); err != nil {
		t.Fatal(err)
	}
	if _, err := appendWithPayloadCarded(s, Item{ID: "answer", ThreadID: "F", TurnIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "fork answer", PayloadID: "fp", Meta: "{}"},
		Payload{ID: "fp", Kind: "text", Meta: "{}", Data: []byte("fork")}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPayloadData("F", "fp", []byte(" more"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	var written int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM items WHERE thread_id = 'F') + (SELECT count(*) FROM thread_fork_hidden WHERE thread_id = 'F')
		+ (SELECT count(*) FROM attachment_owners WHERE thread_id = 'F')`).Scan(&written); err != nil || written < 5 {
		t.Fatalf("fixture wrote %d fork rows, %v", written, err)
	}
	mustPointerFork(t, s, "F", "ask", ForkCut{})

	for _, id := range []string{"ask", "F"} {
		if err := s.DeleteThread(id); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"items", "turns", "payloads", "payload_chunks", "edit_file_snapshots", "thread_fork_lineage",
			"thread_fork_hidden", "attachment_owners", "async_questions", "thread_search_rows"} {
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE thread_id = ?`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("deleted fork %s left %d rows in %s", id, n, table)
			}
		}
		var readers int
		if err := s.db.QueryRow(`SELECT count(*) FROM thread_fork_lineage WHERE ancestor_id = ?`, id).Scan(&readers); err != nil || readers != 0 {
			t.Errorf("deleted fork %s is still read through: %d %v", id, readers, err)
		}
	}
	requireShape(t, s, "S", source)
	if after := historyStampOf(t, s, "S"); after != stamp {
		t.Fatalf("source stamp moved %+v -> %+v", stamp, after)
	}
	if owned, err := s.OwnsAttachment("S", "att"); err != nil || !owned {
		t.Fatalf("source lost its attachment: %v %v", owned, err)
	}
}

// TestPointerForkQueriesStayIndexed pins the plans the fork design depends
// on: fork creation reads only the source's unsettled rows, a lineage arm
// walks the ancestor's index in page order, and the reads before every
// write that might hand off are single index probes.
func TestPointerForkQueriesStayIndexed(t *testing.T) {
	s := forkHandOffFixture(t)
	plan := explainPlan(t, s, forkUnsettledRowsSQL, "G", 9, 0, "G", 9, 0)
	searches := 0
	for _, row := range plan {
		if strings.HasPrefix(row.detail, "SCAN ") {
			t.Errorf("unsettled rows scan: %s\n%s", row.detail, planText(plan))
		}
		if strings.HasPrefix(row.detail, "SEARCH items ") {
			searches++
			if !strings.Contains(row.detail, "idx_items_unsettled") {
				t.Errorf("unsettled rows read without idx_items_unsettled: %s\n%s", row.detail, planText(plan))
			}
		}
	}
	if searches != 2 {
		t.Errorf("unsettled rows: %d item searches, want the source's and its ancestors'\n%s", searches, planText(plan))
	}

	query, args := mustTimelineIDSelection(t, s, "G", timelineSelection{OrderBy: "turn_index DESC, item_index DESC", Limit: 20})
	assertEveryItemsArmWalksAnIndex(t, s, "fork page", query, 3, args...)

	for _, probe := range []struct {
		name, query string
		args        []any
		want        []string
	}{
		{"fork readers", forkReadersSQL, []any{"S", 0, 0}, []string{"idx_thread_fork_lineage_ancestor"}},
		{"fork reader depths", forkReaderDepthsSQL, []any{"S", 0, 0}, []string{"idx_thread_fork_lineage_ancestor"}},
		{"payload rows", payloadRowIDsSQL, repeatArgs(2*payloadRowArmCount, []any{"G", "p"}), []string{"idx_items_payload_id", "idx_items_input_payload_id", "idx_import_history_payloads_id"}},
	} {
		plan := explainPlan(t, s, probe.query, probe.args...)
		text := planText(plan)
		for _, row := range plan {
			if strings.HasPrefix(row.detail, "SCAN ") {
				t.Errorf("%s scans: %s\n%s", probe.name, row.detail, text)
			}
		}
		for _, index := range probe.want {
			if !strings.Contains(text, index) {
				t.Errorf("%s does not probe %s\n%s", probe.name, index, text)
			}
		}
	}
}

// assertEveryItemsArmWalksAnIndex is assertLocalArmWalksAnIndex for every
// arm that reads the physical `items` table: the thread's own and one per
// lineage level. None may sort, scan, or read its lineage row other than by
// primary key. Imported arms sort their chunk rows as a thread's own
// imported arm does, bounded by the lineage cut.
func assertEveryItemsArmWalksAnIndex(t *testing.T, s *Store, what, query string, arms int, args ...any) {
	t.Helper()
	plan := explainPlan(t, s, query, args...)
	text := planText(plan)
	byID := make(map[int]planRow, len(plan))
	var searches []planRow
	for _, row := range plan {
		byID[row.id] = row
		switch {
		case strings.HasPrefix(row.detail, "SCAN items"), strings.HasPrefix(row.detail, "SCAN l"),
			strings.Contains(row.detail, "timeline_items"):
			t.Errorf("%s: %s\n%s", what, row.detail, text)
		case strings.HasPrefix(row.detail, "SEARCH l ") && !strings.Contains(row.detail, "PRIMARY KEY"):
			t.Errorf("%s: lineage read off its key: %s\n%s", what, row.detail, text)
		case strings.HasPrefix(row.detail, "SEARCH items USING INDEX idx_items_"), strings.HasPrefix(row.detail, "SEARCH items USING COVERING INDEX idx_items_"):
			searches = append(searches, row)
		}
	}
	if len(searches) < arms {
		t.Fatalf("%s: %d indexed item arms, want %d\n%s", what, len(searches), arms, text)
	}
	for _, search := range searches {
		for id := search.id; id != 0; id = byID[id].parent {
			row, ok := byID[id]
			if !ok {
				break
			}
			for _, sibling := range plan {
				if (sibling.id == row.id || sibling.parent == row.parent) && strings.Contains(sibling.detail, "USE TEMP B-TREE FOR ORDER BY") {
					t.Errorf("%s: a sorter covers an item arm (%q)\n%s", what, sibling.detail, text)
				}
			}
		}
	}
}
