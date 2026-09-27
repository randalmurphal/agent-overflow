package store

import (
	"context"
	"database/sql"
	"errors"
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
	if _, err := s.UpdateItemFields(timelineParityThreadID, "imp-user-1", ItemPartialUpdate{Summary: &edited}); err != nil {
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
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("insert local item %s: %v", item.ID, err)
		}
	}
}

// seedTimelineParityForks gives the parity thread an agent ask and reply
// under loc-child-2 in turn 3 and two pointer forks: one of all its
// history, and one cut inside turn 2 with rows of its own past the cut,
// top-level and under loc-child-2. The thread then writes the same two
// kinds of row past both cuts. It returns the thread and both forks.
func seedTimelineParityForks(t *testing.T, s *Store) []string {
	t.Helper()
	const wholeFork, cutFork = timelineParityThreadID + "-whole", timelineParityThreadID + "-cut"
	appendRows := func(rows ...Item) {
		t.Helper()
		for _, item := range rows {
			item.Role, item.Status, item.Summary, item.Meta = "assistant", "completed", item.ID, "{}"
			if item.Kind == "user_text" {
				item.Role = "user"
			}
			if _, err := appendCarded(s, item); err != nil {
				t.Fatalf("append %s/%s: %v", item.ThreadID, item.ID, err)
			}
		}
	}
	appendRows(
		Item{ID: "loc-agent-ask-3", ThreadID: timelineParityThreadID, TurnIndex: 3, Kind: "user_text", ParentID: "loc-child-2"},
		Item{ID: "loc-agent-reply-3", ThreadID: timelineParityThreadID, TurnIndex: 3, Kind: "assistant_text", ParentID: "loc-child-2"},
	)
	if err := s.CreatePointerFork(makeThread(wholeFork, "claude"), timelineParityThreadID, ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePointerFork(makeThread(cutFork, "claude"), timelineParityThreadID, ForkCut{BeforeItemID: "loc-answer-2"}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	appendRows(
		Item{ID: "cut-agent-2", ThreadID: cutFork, TurnIndex: 2, Kind: "assistant_text", ParentID: "loc-child-2"},
		Item{ID: "cut-answer-2", ThreadID: cutFork, TurnIndex: 2, Kind: "assistant_text"},
		Item{ID: "cut-more-2", ThreadID: cutFork, TurnIndex: 2, Kind: "assistant_text"},
		Item{ID: "late-agent-3", ThreadID: timelineParityThreadID, TurnIndex: 3, Kind: "assistant_text", ParentID: "loc-child-2"},
		Item{ID: "late-answer-3", ThreadID: timelineParityThreadID, TurnIndex: 3, Kind: "assistant_text"},
	)
	return []string{timelineParityThreadID, wholeFork, cutFork}
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
		page, err := s.ListThreadSliceAround(context.Background(), timelineParityThreadID, "", 5, testRunWindowRows, TimelineSelection{})
		if err != nil {
			t.Fatalf("tail slice: %v", err)
		}
		want := ascending(viewIDs(t, s, `SELECT id FROM timeline_items
		  WHERE thread_id = ? AND `+legacyWindowFilter+`
		  ORDER BY turn_index DESC, item_index DESC LIMIT ?`, timelineParityThreadID, 5))
		assertSameIDs(t, itemIDs(page.Items), want, "tail slice")
	})

	t.Run("slice around an anchor", func(t *testing.T) {
		page, err := s.ListThreadSliceAround(context.Background(), timelineParityThreadID, "loc-launch-2", 6, testRunWindowRows, TimelineSelection{})
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
		page, err := s.ListItemsBeforeCursor(context.Background(), timelineParityThreadID, cursor, 4, testRunWindowRows, TimelineSelection{})
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
		page, err := s.ListItemsAfterCursor(context.Background(), timelineParityThreadID, cursor, 4, testRunWindowRows, TimelineSelection{})
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
		ticks, err := s.ListThreadUserMessageTicks(timelineParityThreadID, TimelineSelection{})
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
// ListSubagentDescendants and subagentAggregatesByRound used before the
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
		got, err := subagentAggregatesByRound(s.reader(), timelineParityThreadID, roots, subagentRoundBoundsFor(roots, nil, nil))
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
// text: the imported arm legitimately sorts (import_history_items is keyed
// by chunk, so no index orders a thread's imported rows; the arm visits
// the chunks in the read's order, chunkRefsIndex, which bounds what its
// sorter reads), so a raw text match for "USE TEMP B-TREE FOR ORDER BY"
// would either pass vacuously or fail on a plan that is correct. The rule
// is about the LOCAL arm's subtree.
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

// mustTimelineArms renders timelineArms for a test that holds s.
func mustTimelineArms(t *testing.T, s *Store, threadID string, sel timelineSelection) (string, []any) {
	t.Helper()
	query, args, err := timelineArms(s.db, threadID, sel)
	if err != nil {
		t.Fatalf("render timeline arms: %v", err)
	}
	return query, args
}

// mustTimelineIDSelection renders timelineIDSelection for a test that holds s.
func mustTimelineIDSelection(t *testing.T, s *Store, threadID string, sel timelineSelection) (string, []any) {
	t.Helper()
	query, args, err := timelineIDSelection(s.db, threadID, sel)
	if err != nil {
		t.Fatalf("render timeline id selection: %v", err)
	}
	return query, args
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

	mainFilter := mainTimelineFilterFor("items.")

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
				Where:   mainFilter,
				OrderBy: "turn_index DESC, item_index DESC",
				Limit:   50,
			},
		},
		{
			name: "older page (ListItemsBeforeCursor)",
			sel: func() timelineSelection {
				sel := beyondCursor(timelineScope{}, TimelineCursor{TurnIndex: 2, ItemIndex: 5}, false)
				sel.OrderBy, sel.Limit = "turn_index DESC, item_index DESC", 50
				return sel
			}(),
		},
		{
			name: "newer page (ListItemsAfterCursor)",
			sel: func() timelineSelection {
				sel := beyondCursor(timelineScope{}, TimelineCursor{TurnIndex: 0, ItemIndex: 1}, true)
				sel.OrderBy, sel.Limit = "turn_index ASC, item_index ASC", 50
				return sel
			}(),
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
			sel:  turnPreviewWalk(mainFilter, nil, TimelineCursor{}),
		},
		{
			name: "title context window (ThreadTitleContextItems)",
			sel: timelineSelection{
				RowIDs: true,
				Where: mainFilter + `
		   AND items.kind IN ('user_text', 'assistant_text')`,
				OrderBy: "turn_index DESC, item_index DESC",
				Limit:   201,
			},
		},
		{
			// windowDigestRowsTx. A held window is verified on a cold
			// open, so the one thing this read may not do is walk the
			// thread: it is bounded by the caller's claimed count and
			// must reach both edges through the ordering index.
			name: "held window digest rows (windowDigestRowsTx)",
			sel: timelineSelection{
				Turn: "?", TurnArgs: []any{0}, FromTurn: true,
				Where: mainFilter + `
		   AND (items.turn_index, items.item_index) >= (?, ?)
		   AND (items.turn_index, items.item_index) <= (?, ?)`,
				WhereArgs: []any{0, 1, 2, 5},
				OrderBy:   "turn_index ASC, item_index ASC",
				Limit:     51,
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
			query, args := mustTimelineIDSelection(t, s, timelineParityThreadID, tc.sel)
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

// A main-timeline read keeps only top-level rows. Every arm of the
// statements a page, a cursor page, a run expansion, a held-window check
// and a turn preview run, a pointer fork's lineage arms included, walks the
// top-level index of
// its source and reads no table row to select a row: through the ordering
// index it would read every subagent child row between two top-level rows.
// Each such walk is keyed by a (turn_index, item_index) bound, so it starts
// at the read's cursor or window edge rather than at the thread's, the
// chunk's or the turn's first row. The statements are the ones the
// production calls run, on threads with children, so a projection the index
// does not cover fails here.
func TestMainTimelineArmsWalkTopLevelIndexes(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	runIDs := seedRunThread(t, s, "runs", "pttptkp")
	for i := range 3 {
		child := Item{
			ID: fmt.Sprintf("child-%d", i), ThreadID: "runs", TurnIndex: 0, ItemIndex: 100 + i,
			Kind: "tool_call", ToolName: "Grep", Role: "assistant", Status: "completed",
			ParentID: runIDs[1], Summary: "child", Meta: "{}", CreatedAt: int64(100 + i),
		}
		if err := insertCarded(s, child); err != nil {
			t.Fatal(err)
		}
	}
	threads := []string{timelineParityThreadID, "runs"}
	for _, source := range threads {
		if err := s.CreatePointerFork(makeThread(source+"-fork", "claude"), source, ForkCut{}, testInterruptedSummary, 1); err != nil {
			t.Fatal(err)
		}
		threads = append(threads, source+"-fork")
	}
	held := make(map[string]HeldWindow, len(threads))
	stale := make(map[string]HistoryStamp, len(threads))
	for _, threadID := range threads {
		held[threadID] = heldWindowFromStore(t, s, threadID)
		stamp := historyStampOf(t, s, threadID)
		stamp.Rev--
		stale[threadID] = stamp
	}
	mainFilter, _ := timelineScope{}.filter("items.")
	ctx := context.Background()
	type mainRead struct {
		name string
		read func(t *testing.T)
	}
	rec := recordStatements(t, s)
	for _, threadID := range threads {
		reads := []mainRead{
			{"held window check", func(t *testing.T) {
				window := held[threadID]
				got, err := s.SyncThreadWindow(ctx, threadID, "", 200, testRunWindowRows, stale[threadID], &window, TimelineSelection{})
				if err != nil {
					t.Fatal(err)
				}
				// The local thread's window verifies, so every check ran.
				if threadID == "runs" && (got.Status != SyncFresh || got.Page != nil) {
					t.Fatalf("held window status = %q page=%v, want a page-less fresh", got.Status, got.Page != nil)
				}
			}},
			{"slice", func(t *testing.T) {
				if _, err := s.ListThreadSliceAround(ctx, threadID, "", 3, testRunWindowRows, TimelineSelection{}); err != nil {
					t.Fatal(err)
				}
			}},
			{"older page", func(t *testing.T) {
				last := held[threadID].NewestItemID
				page, err := s.ListThreadSliceAround(ctx, threadID, last, 1, testRunWindowRows, TimelineSelection{})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.ListItemsBeforeCursor(ctx, threadID, page.OldestCursor, 3, testRunWindowRows, TimelineSelection{}); err != nil {
					t.Fatal(err)
				}
			}},
			{"newer page", func(t *testing.T) {
				if _, err := s.ListItemsAfterCursor(ctx, threadID, TimelineCursor{TurnIndex: 0, ItemIndex: 0}, 3, testRunWindowRows, TimelineSelection{}); err != nil {
					t.Fatal(err)
				}
			}},
		}
		if strings.HasPrefix(threadID, timelineParityThreadID) {
			reads = append(reads, mainRead{"turn preview", func(t *testing.T) {
				if _, found, err := s.ThreadTurnPreview(threadID, "loc-user-2"); err != nil || !found {
					t.Fatalf("preview: found=%v err=%v", found, err)
				}
			}})
		}
		if strings.HasPrefix(threadID, "runs") {
			reads = append(reads, mainRead{"run members", func(t *testing.T) {
				if _, err := s.ListActivityRunMembers(ctx, threadID, ActivityRunMembersRequest{
					RunFirstItemID: runIDs[1], Direction: ActivityRunMembersAfter, Limit: 2,
				}); err != nil {
					t.Fatal(err)
				}
			}})
		}
		for _, tc := range reads {
			t.Run(threadID+"/"+tc.name, func(t *testing.T) {
				stmts := rec.capture(func() { tc.read(t) })
				topLevel := 0
				for _, stmt := range stmts {
					if !strings.Contains(stmt.query, mainFilter) {
						continue
					}
					plan := explainPlan(t, s, stmt.query, stmt.args...)
					for _, r := range plan {
						switch {
						case strings.HasPrefix(r.detail, "SEARCH items USING COVERING INDEX idx_items_top_level "),
							strings.HasPrefix(r.detail, "SEARCH items USING COVERING INDEX idx_import_history_items_top_level "):
							topLevel++
							if !strings.Contains(r.detail, "(turn_index,item_index)>(?,?)") && !strings.Contains(r.detail, "(turn_index,item_index)<(?,?)") {
								t.Errorf("a main-timeline arm does not start its walk at the read's cursor: %q\n%s\n%s", r.detail, stmt.query, planText(plan))
							}
						case strings.Contains(r.detail, "idx_items_thread_turn_item_unique"),
							strings.Contains(r.detail, "idx_import_history_items_timeline"),
							strings.HasPrefix(r.detail, "SCAN items"):
							t.Errorf("a main-timeline arm reads items off the top-level index: %q\n%s\n%s", r.detail, stmt.query, planText(plan))
						}
					}
				}
				if topLevel == 0 {
					t.Errorf("recorded no main-timeline arm on a top-level index in %d statements; the check proved nothing", len(stmts))
				}
			})
		}
	}
}

// A read that follows a position (cursorBound) starts every arm's index
// walk at it: each search of an items source, a pointer fork's lineage
// arms included, is keyed by the (turn_index, item_index) bound, so no arm
// reads the rows of the position's turn that precede it. The statements
// are the ones the turn preview, on the main timeline and in an agent's
// transcript, and the import divergence probe run on the parity thread and
// its forks.
func TestCursorBoundReadsStartAtTheCursor(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	threads := seedTimelineParityForks(t, s)
	bound := cursorBound(TimelineCursor{}, true).Where
	preview := func(threadID, anchorID string) func(t *testing.T) {
		return func(t *testing.T) {
			if _, found, err := s.ThreadTurnPreview(threadID, anchorID); err != nil || !found {
				t.Fatalf("preview of %s: found=%v err=%v", anchorID, found, err)
			}
		}
	}
	rec := recordStatements(t, s)
	for _, threadID := range threads {
		reads := map[string]func(t *testing.T){
			"turn preview": preview(threadID, "loc-user-2"),
			"divergence probe": func(t *testing.T) {
				if _, err := s.HasItemsAfterCursor(threadID, 2, 1); err != nil {
					t.Fatal(err)
				}
			},
		}
		// The cut fork does not show the agent's ask.
		if threadID != timelineParityThreadID+"-cut" {
			reads["agent turn preview"] = preview(threadID, "loc-agent-ask-3")
		}
		for name, read := range reads {
			t.Run(threadID+"/"+name, func(t *testing.T) {
				stmts := rec.capture(func() { read(t) })
				searches := 0
				for _, stmt := range stmts {
					if !strings.Contains(stmt.query, bound) {
						continue
					}
					plan := explainPlan(t, s, stmt.query, stmt.args...)
					for _, r := range plan {
						if !strings.HasPrefix(r.detail, "SEARCH items ") {
							continue
						}
						searches++
						if !strings.Contains(r.detail, "(turn_index,item_index)>(?,?)") {
							t.Errorf("an arm does not start its walk at the cursor: %q\n%s\n%s", r.detail, stmt.query, planText(plan))
						}
					}
				}
				if searches == 0 {
					t.Errorf("recorded no items search keyed by a cursor in %d statements; the check proved nothing", len(stmts))
				}
			})
		}
	}
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
		selectedSQL, selectedArgs := mustTimelineIDSelection(t, s, timelineParityThreadID, timelineSelection{
			Source:  "rel",
			Where:   "items.id = rel.id",
			OrderBy: "turn_index DESC, item_index DESC",
			Limit:   maxSubagentDescendants,
		})
		walk, walkArgs, err := descendantsWalk(s.reader(), timelineParityThreadID, []string{"loc-launch-2"}, visibleItemsFilterFor)
		if err != nil {
			t.Fatal(err)
		}
		query := walk + "\n" + selectedSQL
		args := append(walkArgs, selectedArgs...)
		plan := explainPlan(t, s, query, args...)
		for _, r := range plan {
			if strings.Contains(r.detail, "timeline_items") {
				t.Errorf("descendant walk touches the view: %q", r.detail)
			}
			// The roots arrive as one JSON array; each is still a probe.
			if r.detail == "SCAN items" || r.detail == "SCAN import_history_items" {
				t.Errorf("descendant walk scans a table: %q", r.detail)
			}
		}
		// Each arm probes its parent index twice: the base hop from the
		// roots and the recursive hop from rel.
		text := planText(plan)
		for _, index := range []string{"idx_items_parent (thread_id=? AND parent_id=?)", "idx_import_history_items_parent_lookup (parent_id=?)"} {
			if n := strings.Count(text, index); n != 2 {
				t.Errorf("descendant walk probes %s %d times, want 2:\n%s", index, n, text)
			}
		}
	})

	t.Run("subagentAggregatesByRound", func(t *testing.T) {
		// Exercised through the store method so the aggregate query the
		// decorator actually runs is the one under test; the plan is
		// asserted on the same statement shape below.
		roots := []string{"loc-launch-2"}
		if _, err := subagentAggregatesByRound(s.reader(), timelineParityThreadID, roots, subagentRoundBoundsFor(roots, nil, nil)); err != nil {
			t.Fatalf("aggregates: %v", err)
		}
		resolvedSQL, resolvedArgs := mustTimelineArms(t, s, timelineParityThreadID, timelineSelection{
			Columns: func(string, string) string {
				return `rel.root AS root, items.id AS id, items.kind AS kind,
			        items.status AS status, items.summary AS summary,
			        items.turn_index AS turn_index, items.item_index AS item_index`
			},
			Source: "rel",
			Where:  "items.id = rel.id",
		})
		walk, walkArgs, err := descendantsWalk(s.reader(), timelineParityThreadID, []string{"loc-launch-2"}, visibleItemsFilterFor)
		if err != nil {
			t.Fatal(err)
		}
		query := walk + `
		SELECT root FROM (` + resolvedSQL + `)`
		args := append(walkArgs, resolvedArgs...)
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
			marker: "LOWER(i.summary) LIKE",
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

// TestTimelineItemsViewJoinPushesDown pins the plan of the sidebar reads
// that reach `timeline_items` through correlated terms from OUTSIDE the
// view's UNION ALL: threadColumns' proposed-plan probe joins the view on
// (thread_id, id), and ListThreadsWithItems probes it by thread_id. SQLite
// pushes such terms into the arms only when every result column has the
// same affinity across arms (importedItemRevExpr); otherwise it
// materializes the view per outer row, which is a full scan of `items`
// for every thread in the sidebar. ListProjectsWithThreadCounts carries
// the same probe once per project row. The tripwire is "no node scans a base
// table of the view and no automatic index is built", checked on the
// production statements.
func TestTimelineItemsViewJoinPushesDown(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)

	withItems, withItemsArgs := listThreadsWithItemsQuery()
	projectCounts, projectCountsArgs := listProjectsWithThreadCountsQuery()
	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "ListThreads", query: listThreadsQuery},
		{name: "ListThreadsWithItems", query: withItems, args: withItemsArgs},
		{name: "ListProjectsWithThreadCounts", query: projectCounts, args: projectCountsArgs},
	}
	for _, tc := range cases {
		plan := explainPlan(t, s, tc.query, tc.args...)
		for _, r := range plan {
			if r.detail == "SCAN items" || r.detail == "SCAN imported" || strings.Contains(r.detail, "AUTOMATIC") {
				t.Errorf("%s: timeline_items is materialized instead of searched (%q)\n%s", tc.name, r.detail, planText(plan))
				break
			}
		}
	}
}

// A turn's item index bound is taken per arm: every arm's bound, and so
// the compound's, agrees with the view, and the local arm stops at the
// first row of its turn index instead of reading the turn.
func TestTurnItemIndexQueryTakesEachArmsBound(t *testing.T) {
	s := newTestStore(t)
	seedKeyedLookupThread(t, s)
	if err := s.CreatePointerFork(makeThread(keyedForkID, "claude"), keyedThreadID, ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := appendCarded(s, Item{ID: "fork-appended", ThreadID: keyedForkID, TurnIndex: 4, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "late"}); err != nil {
		t.Fatal(err)
	}
	for _, threadID := range []string{keyedThreadID, keyedForkID} {
		for turn := range 6 {
			for _, aggregate := range []string{"MIN", "MAX"} {
				query, args, err := turnItemIndexQuery(s.db, threadID, turn, aggregate)
				if err != nil {
					t.Fatal(err)
				}
				var got, want sql.NullInt64
				if err := s.db.QueryRow(query, args...).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRow(`SELECT `+aggregate+`(item_index) FROM timeline_items WHERE thread_id = ? AND turn_index = ?`, threadID, turn).Scan(&want); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Errorf("%s turn %d %s = %v, view says %v", threadID, turn, aggregate, got, want)
				}
			}
		}
	}
	if _, _, err := turnItemIndexQuery(s.db, keyedThreadID, 4, "COUNT"); err == nil {
		t.Error("turnItemIndexQuery rendered COUNT")
	}

	query, args, err := turnItemIndexQuery(s.db, keyedThreadID, 4, "MAX")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("EXPLAIN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type op struct {
		code string
		p2   int64
		p4   string
	}
	var program []op
	for rows.Next() {
		var addr, p1, p2, p3 int64
		var code string
		var p4, p5, comment sql.NullString
		if err := rows.Scan(&addr, &code, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		program = append(program, op{code: code, p2: p2, p4: p4.String})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	earlyOut := false
	for addr := 0; addr+1 < len(program); addr++ {
		next := program[addr+1]
		earlyOut = earlyOut || (program[addr].code == "AggStep" && strings.HasPrefix(program[addr].p4, "max(") && next.code == "Goto" && next.p2 > int64(addr+1))
	}
	if !earlyOut {
		t.Errorf("no arm's MAX stops at its first row:\n%s", query)
	}
}
