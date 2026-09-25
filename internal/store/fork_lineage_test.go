package store

import (
	"context"
	"errors"
	"fmt"
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
			meta, err := s.GetPayloadMeta(threadID, it.PayloadID)
			if err != nil {
				t.Fatalf("payload meta %s/%s: %v", threadID, it.PayloadID, err)
			}
			line += " payload=" + string(data) + " " + meta.Meta
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
	if n := ownRowCount(t, s, "fork-0"); n != 0 || ownTurns != 1 {
		t.Fatalf("fork stores %d rows and %d turns, want no row and its cut turn", n, ownTurns)
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
	if !forkPage.HasMoreOlder {
		t.Fatal("the fork's first page reports no older rows")
	}
	requireIDs(t, "fork page", itemIDs(forkPage.Items), itemIDs(sourcePage.Items))
	for _, it := range forkPage.Items {
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
// nearer cut applied. Neither stores a row of what it reads. Rows the
// source places below the cuts later are not part of either fork's history.
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
	var sourceID, sourceTitle string
	if err := s.db.QueryRow(`SELECT fork_source_thread_id, fork_source_title FROM threads WHERE id = 'G'`).Scan(&sourceID, &sourceTitle); err != nil || sourceID != "F" || sourceTitle != "Thread F" {
		t.Fatalf("G's origin = %q %q, %v; want F and its title", sourceID, sourceTitle, err)
	}
	want := []string{"u0", "a0", "u1", "a1", "u2", "a2", "f3"}
	requireIDs(t, "G rows", itemIDs(forkRows(t, s, "G")), want)
	if n := ownRowCount(t, s, "G"); n != 0 {
		t.Fatalf("G stores %d rows, want none", n)
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
	requireIDs(t, "deepest fork rows", itemIDs(forkRows(t, s, deepest)), []string{"u0", "a0"})
	page, err := s.ListThreadSliceAround(context.Background(), deepest, "", 100, testRunWindowRows, TimelineSelection{})
	if err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "deepest page", itemIDs(page.Items), []string{"u0", "a0"})
	err = s.CreatePointerFork(makeThread("too-deep", "claude"), deepest, ForkCut{}, testInterruptedSummary, 999)
	if !errors.Is(err, ErrForkChainTooDeep) {
		t.Fatalf("fork beyond the cap: %v", err)
	}
	if _, err := s.GetThread("too-deep"); err == nil {
		t.Fatal("refused fork left a thread row")
	}
}

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
	topLevel := "items.parent_id = ''"
	for _, tc := range []struct {
		name    string
		maxTurn int
		where   string
		args    []any
		want    string
	}{
		{"bound turn holds it", 9, topLevel, nil, "late"},
		{"past the probes", 8, topLevel, nil, "early"},
		{"within the probes", 3, topLevel, nil, "early"},
		{"filtered past the probes", 9, topLevel + " AND items.id <> ?", []any{"late"}, "early"},
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

// TestPointerForkRevertBeforeTheCutRetracts: a fork's revert of rows it
// inherits lowers its cut instead of copying or deleting them. The source
// and a fork of the fork read what they read before.
func TestPointerForkRevertBeforeTheCutRetracts(t *testing.T) {
	for _, mode := range []string{"turn", "item"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			seedLinearSource(t, s, "S", 4)
			mustPointerFork(t, s, "S", "F", ForkCut{})
			mustPointerFork(t, s, "F", "G", ForkCut{})
			source, grandchild := timelineShape(t, s, "S"), timelineShape(t, s, "G")
			sourceStamp, grandchildStamp := historyStampOf(t, s, "S"), historyStampOf(t, s, "G")

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
			if n := ownRowCount(t, s, "F"); n != 0 {
				t.Fatalf("F stores %d rows, want none", n)
			}
			var cutTurn, cutItem int
			if err := s.db.QueryRow(`SELECT fork_cut_turn_index, fork_cut_item_index FROM threads WHERE id = 'F'`).Scan(&cutTurn, &cutItem); err != nil || cutTurn != 1 || cutItem != 2 {
				t.Fatalf("F cut %d:%d, %v", cutTurn, cutItem, err)
			}
			requireShape(t, s, "S", source)
			requireShape(t, s, "G", grandchild)
			for id, was := range map[string]HistoryStamp{"S": sourceStamp, "G": grandchildStamp} {
				if now := historyStampOf(t, s, id); now != was {
					t.Fatalf("%s's stamp moved %+v -> %+v", id, was, now)
				}
			}

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
// zero-length payload. A fork's own edit of the inherited row copies it,
// and the copy must store that payload as a zero-length blob, whether the
// source holds it locally or in imported history; a copy that bound the
// scanned bytes would bind nil and fail payloads.data NOT NULL.
func TestPointerForkCopiesEmptyPayloads(t *testing.T) {
	for _, imported := range []bool{false, true} {
		t.Run(fmt.Sprintf("imported=%v", imported), func(t *testing.T) {
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
			summary := "edited"
			if _, err := s.UpdateItemFields("F", "empty-tool", ItemPartialUpdate{Summary: &summary}); err != nil {
				t.Fatal(err)
			}

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

// TestPointerForkSearchFollowsTheCut: a fork's search finds the messages it
// inherits, only below its cut and only while it shows them.
func TestPointerForkSearchFollowsTheCut(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "S", []Item{
		{ID: "u0", Kind: "user_text", Role: "user", Status: "completed", Summary: "needle zero"},
		{ID: "u1", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "needle one"},
	})
	mustPointerFork(t, s, "S", "F", throughTurn(0))
	requireIDs(t, "F hits", searchThreadHits(t, s, "F"), []string{"u0"})
	requireIDs(t, "S hits", searchThreadHits(t, s, "S"), []string{"u0", "u1"})
	if err := s.DeleteThreadItem("F", "u0"); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "F hits after hiding", searchThreadHits(t, s, "F"), nil)
	requireIDs(t, "S hits after the fork hid", searchThreadHits(t, s, "S"), []string{"u0", "u1"})
}

// searchThreadHits is the item ids a search for "needle" finds in thread.
func searchThreadHits(t *testing.T, s *Store, thread string) []string {
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

// TestForkRevertPastHiddenRowsLowersItsCut: a fork's revert that reverts
// no inherited row it shows, only rows it hid or replaced with its own,
// still ends its history there, so its next turn lands where the revert
// cut it.
func TestForkRevertPastHiddenRowsLowersItsCut(t *testing.T) {
	for name, revert := range map[string]func(*Store) error{
		"from turn": func(s *Store) error { _, _, err := s.DeleteConversationFromTurn("F", 2); return err },
		"from item": func(s *Store) error { _, _, err := s.DeleteConversationFromItem("F", "u2"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			seedTurnedSource(t, s, "S", 3)
			mustPointerFork(t, s, "S", "F", ForkCut{})
			if err := s.DeleteThreadItem("F", "a2"); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateItemMeta("F", "u2", `{"edited":true}`); err != nil {
				t.Fatal(err)
			}
			if err := revert(s); err != nil {
				t.Fatal(err)
			}
			appendSourceTurn(t, s, "F", 2, "next")
			requireIDs(t, "F rows", itemIDs(forkRows(t, s, "F")), []string{"u0", "a0", "u1", "a1", "next-0", "next-1"})
		})
	}
}
