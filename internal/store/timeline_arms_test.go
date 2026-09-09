package store

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The reads in this file used to be written against the `timeline_items`
// VIEW; they are now written as the view's two PHYSICAL arms
// (timeline_arms.go) so SQLite can merge two pre-sorted index walks
// instead of pouring the whole thread into a temp b-tree. Nothing about
// the RESULT was allowed to change, so the view form is kept here as the
// oracle: every ordered read below is run both ways over a thread whose
// logical timeline genuinely spans both sources, and the two must agree
// row for row, in order.

// timelineParityThreadID is the thread every parity case reads. Its
// timeline deliberately covers each shape the arms have to reconcile:
// imported rows, local rows, an imported row hidden by an override and
// replaced by a local one, invisible plan_update notifications on both
// sides, subagent children (including a nested one) on both sides, and a
// wire-only user message that the reader-authored predicate must skip.
const timelineParityThreadID = "tl-arms"

// seedTimelineParityThread writes that timeline and returns the id of
// the local row that overrode an imported one, which several cases use
// as an anchor.
func seedTimelineParityThread(t *testing.T, s *Store) {
	t.Helper()
	newImportTargetThread(t, s, timelineParityThreadID)

	const base = 1_700_000_000_000
	importedRow := func(id string, turn, index int, kind, role, summary, parent, tool string) ImportRow {
		return ImportRow{Item: Item{
			ID: id, TurnIndex: turn, ItemIndex: index,
			Kind: kind, Role: role, Status: "completed", Summary: summary,
			ParentID: parent, ToolName: tool,
			CreatedAt: base + int64(turn*100+index), UpdatedAt: base + int64(turn*100+index),
		}}
	}
	batch := ImportBatch{
		Turns: []Turn{
			{TurnID: timelineParityThreadID + ":0", ThreadID: timelineParityThreadID, TurnIndex: 0, StartedAt: base},
			{TurnID: timelineParityThreadID + ":1", ThreadID: timelineParityThreadID, TurnIndex: 1, StartedAt: base + 100},
		},
		Rows: []ImportRow{
			importedRow("imp-user-0", 0, 0, "user_text", "user", "imported ask", "", ""),
			importedRow("imp-launch-0", 0, 1, "tool_call", "assistant", "Task", "", "Task"),
			importedRow("imp-child-0", 0, 2, "tool_call", "assistant", "Grep", "imp-launch-0", "Grep"),
			importedRow("imp-grandchild-0", 0, 3, "assistant_text", "assistant", "nested child", "imp-child-0", ""),
			importedRow("imp-plan-0", 0, 4, "notification", "system", "plan", "", "plan_update"),
			importedRow("imp-answer-0", 0, 5, "assistant_text", "assistant", "imported answer", "", ""),
			importedRow("imp-user-1", 1, 0, "user_text", "user", "overridden ask", "", ""),
			importedRow("imp-answer-1", 1, 1, "assistant_text", "assistant", "imported answer two", "", ""),
		},
	}
	if err := s.ApplyImportBatch(timelineParityThreadID, batch); err != nil {
		t.Fatalf("apply import batch: %v", err)
	}
	// Editing an imported row is copy-on-write: it materializes a local
	// `items` row and an override that hides the imported one. That is
	// the case both arms have to agree about, so the fixture must have
	// one — assertTimelineParityFixtureIsRepresentative checks it did.
	edited := "locally edited ask"
	if err := s.UpdateItemFields(timelineParityThreadID, "imp-user-1", ItemPartialUpdate{Summary: &edited}); err != nil {
		t.Fatalf("override imported item: %v", err)
	}

	localRow := func(id string, turn, index int, kind, role, summary, parent, tool, meta string) Item {
		return Item{
			ID: id, ThreadID: timelineParityThreadID, TurnIndex: turn, ItemIndex: index,
			Kind: kind, Role: role, Status: "completed", Summary: summary,
			ParentID: parent, ToolName: tool, Meta: meta,
			CreatedAt: base + int64(turn*100+index), UpdatedAt: base + int64(turn*100+index),
		}
	}
	locals := []Item{
		localRow("loc-user-2", 2, 0, "user_text", "user", "local ask", "", "", ""),
		localRow("loc-launch-2", 2, 1, "tool_call", "assistant", "Task", "", "Task", ""),
		localRow("loc-child-2", 2, 2, "tool_call", "assistant", "Bash", "loc-launch-2", "Bash", ""),
		localRow("loc-grandchild-2", 2, 3, "assistant_text", "assistant", "nested local child", "loc-child-2", "", ""),
		localRow("loc-plan-2", 2, 4, "notification", "system", "plan", "", "plan_update", ""),
		localRow("loc-answer-2", 2, 5, "assistant_text", "assistant", "local answer", "", "", ""),
		localRow("loc-wire-3", 3, 0, "user_text", "user", "injected context", "", "", `{"wire_only":true}`),
		localRow("loc-user-3", 3, 1, "user_text", "user", "final ask", "", "", ""),
		localRow("loc-answer-3", 3, 2, "assistant_text", "assistant", "final answer", "", "", ""),
	}
	if err := s.InsertTurn(Turn{
		TurnID: timelineParityThreadID + ":2", ThreadID: timelineParityThreadID, TurnIndex: 2, StartedAt: base + 200,
	}); err != nil {
		t.Fatalf("insert local turn 2: %v", err)
	}
	if err := s.InsertTurn(Turn{
		TurnID: timelineParityThreadID + ":3", ThreadID: timelineParityThreadID, TurnIndex: 3, StartedAt: base + 300,
	}); err != nil {
		t.Fatalf("insert local turn 3: %v", err)
	}
	for _, item := range locals {
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert local item %s: %v", item.ID, err)
		}
	}
}

// assertTimelineParityFixtureIsRepresentative fails if the seed stopped
// covering both physical sources or the override case. Without it a
// regression in the fixture (or in ApplyImportBatch) would turn every
// parity case below into a comparison of two identical single-arm reads
// that pass no matter what the arms do.
func assertTimelineParityFixtureIsRepresentative(t *testing.T, s *Store) {
	t.Helper()
	count := func(query string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(query, timelineParityThreadID).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", query, err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM items WHERE thread_id = ?`); n < 9 {
		t.Fatalf("fixture has %d local rows, want at least 9", n)
	}
	if n := count(`SELECT COUNT(*) FROM thread_import_chunks refs
	                 JOIN import_history_items i ON i.chunk_id = refs.chunk_id
	                WHERE refs.thread_id = ?`); n < 8 {
		t.Fatalf("fixture has %d imported rows, want at least 8", n)
	}
	if n := count(`SELECT COUNT(*) FROM thread_import_item_overrides WHERE thread_id = ?`); n != 1 {
		t.Fatalf("fixture has %d import overrides, want exactly 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM timeline_items WHERE thread_id = ?`); n != 17 {
		t.Fatalf("fixture logical timeline has %d rows, want 17", n)
	}
}

// --- the oracle: the pre-change view form of each ordered read ---

// legacyWindowFilter is the window predicate as the view form spelled it
// (unaliased, because a view read has only one table in scope).
var legacyWindowFilter = visibleItemsFilter + " AND " + topLevelItemsFilter

// viewIDs runs an id selection against the `timeline_items` view — the
// shape every read in this file used before timeline_arms.go — and
// returns the ids in the order the view produced them.
func viewIDs(t *testing.T, s *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := s.db.Query(query, args...)
	if err != nil {
		t.Fatalf("oracle query: %v\n%s", err, query)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan oracle row: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate oracle rows: %v", err)
	}
	return ids
}

func itemIDs(items []Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// ascending re-sorts a DESC oracle page into the ASC order every
// PagedItems read returns.
func ascending(ids []string) []string {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}

func assertSameIDs(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("%s: read returned no rows, so the parity check is vacuous", what)
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s: arms and view disagree\n arms: %v\n view: %v", what, got, want)
	}
}

func TestTimelineArmsMatchTheViewForWindowReads(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	assertTimelineParityFixtureIsRepresentative(t, s)

	t.Run("tail slice", func(t *testing.T) {
		page, err := s.ListThreadSliceAround(timelineParityThreadID, "", 5)
		if err != nil {
			t.Fatalf("tail slice: %v", err)
		}
		want := ascending(viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+legacyWindowFilter+`
		  ORDER BY turn_index DESC, item_index DESC LIMIT ?`, timelineParityThreadID, 5))
		assertSameIDs(t, itemIDs(page.Items), want, "tail slice")
	})

	t.Run("slice around an anchor", func(t *testing.T) {
		page, err := s.ListThreadSliceAround(timelineParityThreadID, "loc-launch-2", 6)
		if err != nil {
			t.Fatalf("slice around: %v", err)
		}
		anchor, found, err := s.GetThreadItem(timelineParityThreadID, "loc-launch-2")
		if err != nil || !found {
			t.Fatalf("resolve anchor: %v found=%v", err, found)
		}
		atOrBefore := viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+legacyWindowFilter+`
		    AND (turn_index < ? OR (turn_index = ? AND item_index <= ?))
		  ORDER BY turn_index DESC, item_index DESC LIMIT ?`,
			timelineParityThreadID, anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex, 3)
		after := viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+legacyWindowFilter+`
		    AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		  ORDER BY turn_index ASC, item_index ASC LIMIT ?`,
			timelineParityThreadID, anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex, 3)
		assertSameIDs(t, itemIDs(page.Items), append(ascending(atOrBefore), after...), "slice around")
	})

	t.Run("before cursor", func(t *testing.T) {
		cursor := TimelineCursor{TurnIndex: 2, ItemIndex: 5, ItemID: "loc-answer-2"}
		page, err := s.ListItemsBeforeCursor(timelineParityThreadID, cursor, 4)
		if err != nil {
			t.Fatalf("before cursor: %v", err)
		}
		want := ascending(viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+legacyWindowFilter+`
		    AND (turn_index < ? OR (turn_index = ? AND item_index < ?))
		  ORDER BY turn_index DESC, item_index DESC LIMIT ?`,
			timelineParityThreadID, cursor.TurnIndex, cursor.TurnIndex, cursor.ItemIndex, 4))
		assertSameIDs(t, itemIDs(page.Items), want, "before cursor")
		if !page.HasMoreOlder {
			t.Errorf("before cursor: HasMoreOlder = false, want true")
		}
	})

	t.Run("after cursor", func(t *testing.T) {
		cursor := TimelineCursor{TurnIndex: 0, ItemIndex: 1, ItemID: "imp-launch-0"}
		page, err := s.ListItemsAfterCursor(timelineParityThreadID, cursor, 4)
		if err != nil {
			t.Fatalf("after cursor: %v", err)
		}
		want := viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+legacyWindowFilter+`
		    AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		  ORDER BY turn_index ASC, item_index ASC LIMIT ?`,
			timelineParityThreadID, cursor.TurnIndex, cursor.TurnIndex, cursor.ItemIndex, 4)
		assertSameIDs(t, itemIDs(page.Items), want, "after cursor")
		if !page.HasMoreNewer {
			t.Errorf("after cursor: HasMoreNewer = false, want true")
		}
	})
}

func TestTimelineArmsMatchTheViewForUserMessageReads(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	assertTimelineParityFixtureIsRepresentative(t, s)

	t.Run("ticks", func(t *testing.T) {
		ticks, err := s.ListThreadUserMessageTicks(timelineParityThreadID)
		if err != nil {
			t.Fatalf("ticks: %v", err)
		}
		got := make([]string, 0, len(ticks))
		for _, tick := range ticks {
			got = append(got, tick.ID)
		}
		want := viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+readerAuthoredUserTextFilter+`
		  ORDER BY turn_index ASC, item_index ASC`, timelineParityThreadID)
		assertSameIDs(t, got, want, "user message ticks")
		if slices.Contains(got, "loc-wire-3") {
			t.Errorf("ticks included the wire-only injection")
		}
	})

	t.Run("history", func(t *testing.T) {
		entries, err := s.ListThreadUserMessageHistory(timelineParityThreadID, 3)
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		got := make([]string, 0, len(entries))
		for _, entry := range entries {
			got = append(got, entry.ID)
		}
		want := viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+readerAuthoredUserTextFilter+`
		  ORDER BY turn_index DESC, item_index DESC LIMIT ?`, timelineParityThreadID, 3)
		assertSameIDs(t, got, want, "user message history")
	})

	t.Run("turn preview walks to the next reader ask", func(t *testing.T) {
		preview, found, err := s.ThreadTurnPreview(timelineParityThreadID, "imp-user-0")
		if err != nil || !found {
			t.Fatalf("turn preview: %v found=%v", err, found)
		}
		if preview.UserText != "imported ask" || preview.AssistantText != "imported answer" {
			t.Errorf("turn preview = %+v, want the imported ask and its final answer", preview)
		}
		// The walk must cross the source boundary the same way the view
		// did: the local turn's answer is the last one before the next
		// reader ask, and the wire-only row in turn 3 is not that ask.
		preview, found, err = s.ThreadTurnPreview(timelineParityThreadID, "loc-user-2")
		if err != nil || !found {
			t.Fatalf("turn preview across sources: %v found=%v", err, found)
		}
		if preview.AssistantText != "local answer" {
			t.Errorf("turn preview assistant = %q, want %q", preview.AssistantText, "local answer")
		}
	})

	t.Run("title context", func(t *testing.T) {
		items, dropped, err := s.ThreadTitleContextItems(timelineParityThreadID, 3)
		if err != nil {
			t.Fatalf("title context: %v", err)
		}
		if !dropped {
			t.Errorf("title context: dropped = false, want true")
		}
		window := ascending(viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+topLevelItemsFilter+`
		    AND kind IN ('user_text', 'assistant_text')
		  ORDER BY turn_index DESC, item_index DESC LIMIT ?`, timelineParityThreadID, 3))
		earliest := viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+topLevelItemsFilter+`
		    AND kind = 'user_text'
		  ORDER BY turn_index ASC, item_index ASC LIMIT 1`, timelineParityThreadID)
		assertSameIDs(t, itemIDs(items), append(earliest, window...), "title context")
	})
}

// legacyDescendantsCTE is the two-arm, view-backed recursive walk
// ListSubagentDescendants and subagentAggregatesByRoot used before the
// per-source arms replaced it. It is the oracle for both: identical
// rows, identical order, and the only difference is the plan.
//
// Placeholder order: base hop (thread id, root), recursive hop (thread id).
const legacyDescendantsCTE = `WITH RECURSIVE rel(root, id) AS (
	SELECT i.parent_id, i.id
	  FROM timeline_items i
	 WHERE i.thread_id = ?
	   AND i.parent_id IN (?)
	   AND i.parent_id <> ''
	   AND NOT (i.kind = 'notification' AND i.tool_name = 'plan_update')
	UNION
	SELECT rel.root, i.id
	  FROM rel
	  CROSS JOIN timeline_items i ON i.parent_id = rel.id
	 WHERE i.thread_id = ?
	   AND i.parent_id <> ''
	   AND NOT (i.kind = 'notification' AND i.tool_name = 'plan_update')
)`

func TestTimelineArmsMatchTheViewForSubagentReads(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	assertTimelineParityFixtureIsRepresentative(t, s)

	for _, root := range []string{"imp-launch-0", "loc-launch-2"} {
		t.Run("descendants of "+root, func(t *testing.T) {
			items, err := s.ListSubagentDescendants(timelineParityThreadID, root)
			if err != nil {
				t.Fatalf("list descendants: %v", err)
			}
			// queryHydratedTimelineItems re-sorts its page ASC, so the
			// oracle's DESC id page (the cap picks the NEWEST rows) is
			// reversed to compare.
			want := ascending(viewIDs(t, s, legacyDescendantsCTE+`
			SELECT id FROM (
				SELECT items.id AS id
				  FROM rel
				  CROSS JOIN timeline_items AS items ON items.thread_id = ? AND items.id = rel.id
				 ORDER BY items.turn_index DESC, items.item_index DESC
				 LIMIT ?
			)`, timelineParityThreadID, root, timelineParityThreadID, timelineParityThreadID, maxSubagentDescendants))
			assertSameIDs(t, itemIDs(items), want, "descendants of "+root)
			if len(items) != 2 {
				t.Errorf("descendants of %s = %d rows, want 2 (the child and its nested child)", root, len(items))
			}
		})
	}

	t.Run("anchor aggregates", func(t *testing.T) {
		roots := []string{"imp-launch-0", "loc-launch-2", "loc-child-2"}
		got, err := s.subagentAggregatesByRoot(s.reader(), timelineParityThreadID, roots)
		if err != nil {
			t.Fatalf("aggregates: %v", err)
		}
		want := map[string]subagentAnchorAggregate{
			// Two descendants each; the preview is the newest tool_call
			// with a summary, which for the launches is their direct child.
			"imp-launch-0": {descendantCount: 2, latestChildSummary: "Grep"},
			"loc-launch-2": {descendantCount: 2, latestChildSummary: "Bash"},
			// A tool_call whose only descendant is plain text has a count
			// but no tool summary to preview.
			"loc-child-2": {descendantCount: 1, latestChildSummary: ""},
		}
		if len(got) != len(want) {
			t.Fatalf("aggregates = %v, want %v", got, want)
		}
		for root, expected := range want {
			if got[root] != expected {
				t.Errorf("aggregate for %s = %+v, want %+v", root, got[root], expected)
			}
		}
	})
}

// --- plan tripwires ---

// planRow is one EXPLAIN QUERY PLAN node. The tree matters, not just the
// text: the imported arm legitimately sorts (import_history_items has no
// (turn_index, item_index) index and the rows are few), so a raw text
// match for "USE TEMP B-TREE FOR ORDER BY" would either pass vacuously
// or fail on a plan that is correct. The rule is about the LOCAL arm's
// subtree, which is the one that scales with thread length.
type planRow struct {
	id     int
	parent int
	detail string
}

func explainPlan(t *testing.T, s *Store, query string, args ...any) []planRow {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, query)
	}
	defer rows.Close()
	var plan []planRow
	for rows.Next() {
		var r planRow
		var notUsed int
		if err := rows.Scan(&r.id, &r.parent, &notUsed, &r.detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	return plan
}

func planText(plan []planRow) string {
	var b strings.Builder
	for _, r := range plan {
		fmt.Fprintf(&b, "  %d/%d %s\n", r.id, r.parent, r.detail)
	}
	return b.String()
}

// assertLocalArmWalksAnIndex is the whole tripwire. For an arm-rendered
// selection it requires that (1) no node names the timeline_items view,
// so nothing scans or materializes it, (2) the local `items` arm is an
// index SEARCH, and (3) no sorter sits between that arm and the root —
// which is what "the ORDER BY walks an index" means in plan terms.
func assertLocalArmWalksAnIndex(t *testing.T, s *Store, what, query string, args ...any) {
	t.Helper()
	plan := explainPlan(t, s, query, args...)
	byID := make(map[int]planRow, len(plan))
	for _, r := range plan {
		byID[r.id] = r
		if strings.Contains(r.detail, "timeline_items") {
			t.Errorf("%s: plan touches the timeline_items view (%q); ordered reads go through timelineArms\n%s",
				what, r.detail, planText(plan))
		}
	}
	local := -1
	for _, r := range plan {
		// The local arm is the only node that searches the base `items`
		// table by an index; the imported arm searches
		// thread_import_chunks / import_history_items.
		if strings.HasPrefix(r.detail, "SEARCH items USING INDEX") ||
			strings.HasPrefix(r.detail, "SEARCH items USING COVERING INDEX") {
			local = r.id
			break
		}
	}
	if local < 0 {
		t.Fatalf("%s: no indexed SEARCH of the local `items` arm in the plan\n%s", what, planText(plan))
	}
	for id := local; id != 0; id = byID[id].parent {
		row, ok := byID[id]
		if !ok {
			break
		}
		if strings.Contains(row.detail, "USE TEMP B-TREE FOR ORDER BY") {
			t.Errorf("%s: the local arm sorts instead of walking its index (%q)\n%s",
				what, row.detail, planText(plan))
		}
		// Sorters are emitted as siblings of the node they order, so the
		// walk up also has to check each ancestor's other children.
		for _, sibling := range plan {
			if sibling.parent == row.parent && strings.Contains(sibling.detail, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("%s: a sorter covers the local arm (%q)\n%s", what, sibling.detail, planText(plan))
			}
		}
	}
}

func TestTimelineArmSelectionsWalkIndexes(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)

	cursorWhere := windowedTimelineFilter + `
		   AND (items.turn_index < ? OR (items.turn_index = ? AND items.item_index < ?))`

	// Each case is the SELECTION one production read builds. They are
	// re-stated here rather than reached through the store methods
	// because EXPLAIN needs the statement text;
	// TestOrderedTimelineReadsGoThroughTheArms is what keeps the
	// production side from drifting away from this shape.
	cases := []struct {
		name string
		sel  timelineSelection
	}{
		{
			name: "tail slice (listTailSlice)",
			sel: timelineSelection{
				Where:   windowedTimelineFilter,
				OrderBy: "turn_index DESC, item_index DESC",
				Limit:   50,
			},
		},
		{
			name: "older page (ListItemsBeforeCursor)",
			sel: timelineSelection{
				Where:     cursorWhere,
				WhereArgs: []any{2, 2, 5},
				OrderBy:   "turn_index DESC, item_index DESC",
				Limit:     50,
			},
		},
		{
			name: "newer page (ListItemsAfterCursor)",
			sel: timelineSelection{
				Where: windowedTimelineFilter + `
		   AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index > ?))`,
				WhereArgs: []any{0, 0, 1},
				OrderBy:   "turn_index ASC, item_index ASC",
				Limit:     50,
			},
		},
		{
			name: "nav rail ticks (ListThreadUserMessageTicks)",
			sel: timelineSelection{
				Where:   readerAuthoredUserTextFilterFor("items."),
				OrderBy: "turn_index ASC, item_index ASC",
			},
		},
		{
			name: "composer recall (ListThreadUserMessageHistory)",
			sel: timelineSelection{
				Where:   readerAuthoredUserTextFilterFor("items."),
				OrderBy: "turn_index DESC, item_index DESC",
				Limit:   20,
			},
		},
		{
			name: "turn preview walk (ThreadTurnPreview)",
			sel: timelineSelection{
				Where: topLevelItemsFilterFor("items.") + `
		   AND items.kind IN ('user_text', 'assistant_text')
		   AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index > ?))`,
				WhereArgs: []any{0, 0, 0},
				OrderBy:   "turn_index ASC, item_index ASC",
				Limit:     turnPreviewScanLimit,
			},
		},
		{
			name: "title context window (ThreadTitleContextItems)",
			sel: timelineSelection{
				Where: topLevelItemsFilterFor("items.") + `
		   AND items.kind IN ('user_text', 'assistant_text')`,
				OrderBy: "turn_index DESC, item_index DESC",
				Limit:   201,
			},
		},
		{
			name: "title context earliest ask (ThreadTitleContextItems)",
			sel: timelineSelection{
				Where: topLevelItemsFilterFor("items.") + `
		   AND items.kind = 'user_text'`,
				OrderBy: "turn_index ASC, item_index ASC",
				Limit:   1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, args := timelineIDSelection(timelineParityThreadID, tc.sel)
			assertLocalArmWalksAnIndex(t, s, tc.name, query, args...)
		})
	}

	// The negative control. Without it the assertion above could be
	// passing because it is looking for something no plan ever has, and
	// the whole file would go green against the regression it exists to
	// catch. The same selection through the view must produce exactly the
	// two nodes the rule prohibits.
	t.Run("the view form is what this rule prohibits", func(t *testing.T) {
		plan := explainPlan(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+legacyWindowFilter+`
		  ORDER BY turn_index DESC, item_index DESC LIMIT ?`, timelineParityThreadID, 50)
		scans, sorts := false, false
		for _, r := range plan {
			scans = scans || r.detail == "SCAN timeline_items"
			sorts = sorts || strings.Contains(r.detail, "USE TEMP B-TREE FOR ORDER BY") && r.parent == 0
		}
		if !scans || !sorts {
			t.Fatalf("the view form no longer scans-and-sorts (scan=%v sort=%v), so the rule above proves nothing\n%s",
				scans, sorts, planText(plan))
		}
	})
}

// TestSubagentWalksDoNotMaterializeTheView is the descendant half of the
// same rule. A recursive step that names `timeline_items` makes SQLite
// materialize the whole thread — twice, counting the final resolution
// join — which measured 129 ms against 13 ms for the same window over
// physical tables.
func TestSubagentWalksDoNotMaterializeTheView(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)

	t.Run("ListSubagentDescendants", func(t *testing.T) {
		selectedSQL, selectedArgs := timelineIDSelection(timelineParityThreadID, timelineSelection{
			Source:  "rel",
			Where:   "items.id = rel.id",
			OrderBy: "turn_index DESC, item_index DESC",
			Limit:   maxSubagentDescendants,
		})
		query := descendantsCTEFromRoots(1) + "\n" + selectedSQL
		args := append(descendantsCTEArgs(timelineParityThreadID, []string{"loc-launch-2"}), selectedArgs...)
		for _, r := range explainPlan(t, s, query, args...) {
			if strings.Contains(r.detail, "timeline_items") {
				t.Errorf("descendant walk touches the view: %q", r.detail)
			}
		}
	})

	t.Run("subagentAggregatesByRoot", func(t *testing.T) {
		// Exercised through the store method so the aggregate query the
		// decorator actually runs is the one under test; the plan is
		// asserted on the same statement shape below.
		if _, err := s.subagentAggregatesByRoot(s.reader(), timelineParityThreadID, []string{"loc-launch-2"}); err != nil {
			t.Fatalf("aggregates: %v", err)
		}
		resolvedSQL, resolvedArgs := timelineArms(timelineParityThreadID, timelineSelection{
			Columns: func(string) string {
				return `rel.root AS root, items.id AS id, items.kind AS kind,
			        items.status AS status, items.summary AS summary,
			        items.turn_index AS turn_index, items.item_index AS item_index`
			},
			Source: "rel",
			Where:  "items.id = rel.id",
		})
		query := descendantsCTEFromRoots(1) + `
		SELECT root FROM (` + resolvedSQL + `)`
		args := append(descendantsCTEArgs(timelineParityThreadID, []string{"loc-launch-2"}), resolvedArgs...)
		for _, r := range explainPlan(t, s, query, args...) {
			if strings.Contains(r.detail, "timeline_items") {
				t.Errorf("aggregate resolution touches the view: %q", r.detail)
			}
		}
	})
}

// TestOrderedTimelineReadsGoThroughTheArms closes the class rather than
// the instances: it is a SOURCE rule, because the hazard is a NEW read
// written the obvious way. `timeline_items` stays the right source for
// an unordered set read, an EXISTS probe, a single-turn read and a
// single-row lookup — but the moment a statement orders a whole thread
// by its TIMELINE COORDINATE through the view, SQLite is back to
// pouring every row of both arms into a temp b-tree, and no test of the
// existing readers would notice.
func TestOrderedTimelineReadsGoThroughTheArms(t *testing.T) {
	// Shrink-only. Every entry predates timeline_arms.go, and none is a
	// window page: their cost is the scan their own predicate forces, not
	// the ordering. An entry that gets converted must be DELETED.
	allowed := []struct{ marker, why string }{
		{
			marker: "json_extract(meta, '$.task_id')",
			why:    "FindNotificationItemByTaskID resolves ONE row through a partial expression index on meta.task_id; the ORDER BY only breaks ties among that task's rows",
		},
		{
			marker: "LOWER(i.summary) LIKE ?",
			why:    "SearchThreadItems returns every match rather than a page, so it scans the thread either way",
		},
		{
			marker: "MIN(item_index)",
			why:    "ListTurnUserSummaries is a GROUP BY aggregate over the whole thread, not a page; the view pushes its predicate down to idx_items_user_text on the local arm already",
		},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, literal := range rawStringLiterals(string(source)) {
			if !strings.Contains(literal, "timeline_items") || strings.Contains(literal, "CREATE VIEW") {
				continue
			}
			if !ordersByTimelineCoordinate(literal) {
				continue
			}
			allowedBy := ""
			for _, entry := range allowed {
				if strings.Contains(literal, entry.marker) {
					allowedBy = entry.why
					break
				}
			}
			if allowedBy != "" {
				continue
			}
			t.Errorf("%s: a read orders the timeline_items view by the timeline coordinate; "+
				"render the physical arms with timelineArms (timeline_arms.go) instead:\n%s",
				name, literal)
		}
	}
}

// ordersByTimelineCoordinate reports whether a statement's trailing
// ORDER BY sorts on the TIMELINE coordinate. `turns.turn_index` does not
// count: one literal in threads.go names the view in one subquery and
// orders a `turns` subquery in another, and ordering a handful of turn
// rows is not the shape this rule is about.
func ordersByTimelineCoordinate(literal string) bool {
	at := strings.LastIndex(literal, "ORDER BY")
	if at < 0 {
		return false
	}
	tail := literal[at:]
	for {
		i := strings.Index(tail, "turn_index")
		if i < 0 {
			return false
		}
		if !strings.HasSuffix(tail[:i], "turns.") {
			return true
		}
		tail = tail[i+len("turn_index"):]
	}
}

// splicedLiteral matches the Go concatenation glue between two raw
// literals of ONE statement — `…` + visibleItemsFilter + `…`. Removing
// it joins them, which is what makes the rule above see a whole
// statement: nearly every query here splices a shared predicate in, and
// per-literal matching would put the FROM clause and the ORDER BY in
// different strings and miss the pair (ListTurnUserSummaries is exactly
// that shape).
var splicedLiteral = regexp.MustCompile("`[ \t\n]*\\+[^`]*\\+[ \t\n]*`")

// rawStringLiterals returns the backtick-quoted literals in Go source,
// with spliced concatenations already joined, so each element is one
// statement rather than one fragment.
func rawStringLiterals(source string) []string {
	parts := strings.Split(splicedLiteral.ReplaceAllString(source, ""), "`")
	literals := make([]string, 0, len(parts)/2)
	for i := 1; i < len(parts); i += 2 {
		literals = append(literals, parts[i])
	}
	return literals
}
