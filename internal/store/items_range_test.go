package store

import (
	"strings"
	"testing"
)

func equalIDs(got []Item, want ...string) bool {
	ids := itemIDs(got)
	if len(ids) != len(want) {
		return false
	}
	for i := range ids {
		if ids[i] != want[i] {
			return false
		}
	}
	return true
}

// TestThreadTimelineBoundsNamesBothEdges pins the two coordinates a
// transcript window is expressed against, over a thread whose oldest rows
// are imported and whose newest are local.
func TestThreadTimelineBoundsNamesBothEdges(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)

	oldest, newest, ok, err := s.ThreadTimelineBounds(timelineParityThreadID)
	if err != nil || !ok {
		t.Fatalf("ThreadTimelineBounds: ok=%t err=%v", ok, err)
	}
	if oldest.TurnIndex != 0 || oldest.ItemIndex != 0 || oldest.ItemID != "imp-user-0" {
		t.Errorf("oldest = %#v, want the imported turn 0 head", oldest)
	}
	if newest.TurnIndex != 3 || newest.ItemIndex != 2 || newest.ItemID != "loc-answer-3" {
		t.Errorf("newest = %#v, want the last local row", newest)
	}
}

// TestThreadTimelineBoundsOnThreadsWithNoRows covers the two shapes that
// are not "first and last row": a thread with nothing stored, and one
// whose newest turn was recorded before its first item was.
func TestThreadTimelineBoundsOnThreadsWithNoRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "bounds-empty")

	if _, _, ok, err := s.ThreadTimelineBounds("bounds-empty"); err != nil || ok {
		t.Fatalf("empty thread: ok=%t err=%v", ok, err)
	}

	mustCreateThread(t, s, "bounds-open-turn")
	if err := s.InsertTurn(Turn{TurnID: "bt-0", ThreadID: "bounds-open-turn", TurnIndex: 0, StartedAt: 10}); err != nil {
		t.Fatalf("insert turn 0: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "bt-item", ThreadID: "bounds-open-turn", TurnIndex: 0, ItemIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "ask", CreatedAt: 10, UpdatedAt: 10,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	// The newest turn exists with no item of its own, which is the state
	// between a turn start and its first persisted row.
	if err := s.InsertTurn(Turn{TurnID: "bt-1", ThreadID: "bounds-open-turn", TurnIndex: 1, StartedAt: 20}); err != nil {
		t.Fatalf("insert turn 1: %v", err)
	}
	oldest, newest, ok, err := s.ThreadTimelineBounds("bounds-open-turn")
	if err != nil || !ok {
		t.Fatalf("open-turn thread: ok=%t err=%v", ok, err)
	}
	if oldest.TurnIndex != 0 || newest.TurnIndex != 0 || newest.ItemID != "bt-item" {
		t.Errorf("bounds = %#v / %#v, want the one stored row on both edges", oldest, newest)
	}
}

// TestThreadTimelineBoundsCountsNegativeItemIndexes pins the head-healed
// prompt: it persists below zero and is the oldest coordinate of its turn.
func TestThreadTimelineBoundsCountsNegativeItemIndexes(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "bounds-head")
	if err := s.InsertTurn(Turn{TurnID: "bh-0", ThreadID: "bounds-head", TurnIndex: 0, StartedAt: 10}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "bh-answer", ThreadID: "bounds-head", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "answer", CreatedAt: 10, UpdatedAt: 10,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if _, err := s.UpsertItemAtTurnHead(Item{
		ID: "bh-prompt", ThreadID: "bounds-head", TurnIndex: 0,
		Kind: "user_text", Role: "user", Status: "completed", Summary: "healed ask", CreatedAt: 9, UpdatedAt: 9,
	}); err != nil {
		t.Fatalf("UpsertItemAtTurnHead: %v", err)
	}

	oldest, newest, ok, err := s.ThreadTimelineBounds("bounds-head")
	if err != nil || !ok {
		t.Fatalf("ThreadTimelineBounds: ok=%t err=%v", ok, err)
	}
	if oldest.ItemID != "bh-prompt" || oldest.ItemIndex >= 0 {
		t.Errorf("oldest = %#v, want the head-healed row at a negative index", oldest)
	}
	if newest.ItemID != "bh-answer" {
		t.Errorf("newest = %#v, want the answer", newest)
	}

	// A range that starts at the bounds must hold the negative row: the
	// coordinate the bounds reported is a real bound, not a clamp.
	rows, err := s.ListItemsInRange("bounds-head", oldest, newest, 10, true)
	if err != nil {
		t.Fatalf("ListItemsInRange: %v", err)
	}
	if !equalIDs(rows, "bh-prompt", "bh-answer") {
		t.Errorf("range = %v", itemIDs(rows))
	}
}

// TestListItemsInRangeWalksTheCoordinateRange pins the tuple bounds, the
// SQL limit, and the two kinds of row the range decides about: subagent
// children, which includeChildren governs, and plan_update notifications,
// which a transcript reports and the rendered timeline does not.
func TestListItemsInRangeWalksTheCoordinateRange(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)

	whole := TimelineCursor{TurnIndex: 0, ItemIndex: -1 << 20}
	end := TimelineCursor{TurnIndex: 3, ItemIndex: 1 << 20}

	all, err := s.ListItemsInRange(timelineParityThreadID, whole, end, 100, true)
	if err != nil {
		t.Fatalf("ListItemsInRange(all): %v", err)
	}
	if !equalIDs(all,
		"imp-user-0", "imp-launch-0", "imp-child-0", "imp-grandchild-0", "imp-plan-0", "imp-answer-0",
		"imp-user-1", "imp-answer-1",
		"loc-user-2", "loc-launch-2", "loc-child-2", "loc-grandchild-2", "loc-plan-2", "loc-answer-2",
		"loc-wire-3", "loc-user-3", "loc-answer-3") {
		t.Fatalf("whole range = %v", itemIDs(all))
	}
	// The override is applied: the row carries the local edit, not the
	// imported text it replaced.
	for _, item := range all {
		if item.ID == "imp-user-1" && item.Summary != "locally edited ask" {
			t.Errorf("overridden row = %q, want the local edit", item.Summary)
		}
	}

	// Children out: the same range without the subagent descendants.
	topLevel, err := s.ListItemsInRange(timelineParityThreadID, whole, end, 100, false)
	if err != nil {
		t.Fatalf("ListItemsInRange(top level): %v", err)
	}
	for _, item := range topLevel {
		if item.ParentID != "" {
			t.Fatalf("a child row survived includeChildren=false: %#v", item)
		}
	}
	if len(topLevel) != len(all)-4 {
		t.Errorf("top-level rows = %d, want %d", len(topLevel), len(all)-4)
	}
	// plan_update notifications stay in: this is not the rendered
	// timeline's window.
	planRows := 0
	for _, item := range topLevel {
		if item.ToolName == "plan_update" {
			planRows++
		}
	}
	if planRows != 2 {
		t.Errorf("plan_update rows = %d, want both", planRows)
	}

	// Inclusive tuple bounds, across a turn boundary.
	middle, err := s.ListItemsInRange(timelineParityThreadID,
		TimelineCursor{TurnIndex: 0, ItemIndex: 5}, TimelineCursor{TurnIndex: 1, ItemIndex: 0}, 100, true)
	if err != nil {
		t.Fatalf("ListItemsInRange(middle): %v", err)
	}
	if !equalIDs(middle, "imp-answer-0", "imp-user-1") {
		t.Errorf("inclusive range = %v", itemIDs(middle))
	}

	// The limit is applied in SQL and the page is the oldest rows of the
	// range, so a caller can continue one coordinate past the last row.
	page, err := s.ListItemsInRange(timelineParityThreadID, whole, end, 3, true)
	if err != nil {
		t.Fatalf("ListItemsInRange(limit): %v", err)
	}
	if !equalIDs(page, "imp-user-0", "imp-launch-0", "imp-child-0") {
		t.Fatalf("limited page = %v", itemIDs(page))
	}
	next, err := s.ListItemsInRange(timelineParityThreadID,
		TimelineCursor{TurnIndex: page[2].TurnIndex, ItemIndex: page[2].ItemIndex + 1}, end, 2, true)
	if err != nil {
		t.Fatalf("ListItemsInRange(next page): %v", err)
	}
	if !equalIDs(next, "imp-grandchild-0", "imp-plan-0") {
		t.Errorf("next page = %v", itemIDs(next))
	}

	// A non-positive limit is an empty page without a query, and a range
	// that ends before it starts holds nothing.
	if rows, err := s.ListItemsInRange(timelineParityThreadID, whole, end, 0, true); err != nil || len(rows) != 0 {
		t.Errorf("limit 0 = %v, %v", itemIDs(rows), err)
	}
	if rows, err := s.ListItemsInRange(timelineParityThreadID, end, whole, 10, true); err != nil || len(rows) != 0 {
		t.Errorf("inverted range = %v, %v", itemIDs(rows), err)
	}
}

// TestListItemsInRangeCarriesPayloadFields pins that a range row is a
// fully hydrated item: the transcript reads a body's kind and size from
// the row it got back.
func TestListItemsInRangeCarriesPayloadFields(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "range-payload")
	if err := s.InsertTurn(Turn{TurnID: "rp-0", ThreadID: "range-payload", TurnIndex: 0, StartedAt: 10}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if _, err := s.UpsertItem(Item{
		ID: "rp-tool", ThreadID: "range-payload", TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "Bash",
		ToolName: "Bash", PayloadID: "rp-payload", CreatedAt: 10, UpdatedAt: 10,
	}, &Payload{ID: "rp-payload", Kind: "tool_result", Data: []byte("output"), CreatedAt: 10}); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	rows, err := s.ListItemsInRange("range-payload",
		TimelineCursor{}, TimelineCursor{TurnIndex: 0, ItemIndex: 10}, 10, true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListItemsInRange = %v, %v", itemIDs(rows), err)
	}
	if rows[0].PayloadID != "rp-payload" || rows[0].PayloadKind != "tool_result" {
		t.Errorf("payload fields = %#v", rows[0])
	}
}

// TestListItemsInRangeWalksTheOrderingIndex is the plan tripwire for the
// range read: the row-value bound must drive the local arm's
// (thread_id, turn_index, item_index) index rather than filter a scan.
func TestListItemsInRangeWalksTheOrderingIndex(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)

	for _, tc := range []struct {
		name            string
		includeChildren bool
	}{{"with children", true}, {"top level only", false}} {
		t.Run(tc.name, func(t *testing.T) {
			where := `(items.turn_index, items.item_index) >= (?, ?)
		   AND (items.turn_index, items.item_index) <= (?, ?)`
			if !tc.includeChildren {
				where += "\n		   AND " + topLevelItemsFilterFor("items.")
			}
			query, args := mustTimelineIDSelection(t, s, timelineParityThreadID, timelineSelection{
				Where:     where,
				WhereArgs: []any{0, 0, 3, 2},
				OrderBy:   "turn_index ASC, item_index ASC",
				Limit:     50,
			})
			assertLocalArmWalksAnIndex(t, s, "ListItemsInRange", query, args...)
			plan := explainPlan(t, s, query, args...)
			bounded := false
			for _, row := range plan {
				if strings.Contains(row.detail, "SEARCH items") && strings.Contains(row.detail, "(turn_index,item_index)>") {
					bounded = true
				}
			}
			if !bounded {
				t.Errorf("the range bound does not constrain the index walk\n%s", planText(plan))
			}
		})
	}
}

// LatestHumanUserText is what a request's message quotes: the last thing a
// PERSON typed in the sending thread. A message the app wrote on an agent's
// behalf carries an origin, and quoting one of those back would quote the
// machine to itself.
func TestLatestHumanUserTextSkipsWhatAgentsWrote(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-quote")

	if _, found, err := s.LatestHumanUserText("t-quote"); err != nil || found {
		t.Fatalf("an empty thread reported text: found=%v err=%v", found, err)
	}
	rows := []struct {
		id      string
		index   int
		summary string
		meta    string
	}{
		{id: "u0", index: 0, summary: "look at the launcher"},
		{id: "u1", index: 1, summary: "and the installer", meta: `{"sendId":"send-1"}`},
		{id: "u2", index: 2, summary: "Agent request from thread", meta: `{"origin":"agent-thread","originThread":{"threadId":"t-other"}}`},
	}
	for _, row := range rows {
		if _, err := s.AppendItem(Item{
			ID: row.id, ThreadID: "t-quote", TurnIndex: row.index, Kind: "user_text", Role: "user",
			Status: "completed", Summary: row.summary, Meta: row.meta, CreatedAt: 100, UpdatedAt: 100,
		}); err != nil {
			t.Fatalf("append %s: %v", row.id, err)
		}
	}

	text, found, err := s.LatestHumanUserText("t-quote")
	if err != nil || !found {
		t.Fatalf("LatestHumanUserText: found=%v err=%v", found, err)
	}
	if text != "and the installer" {
		t.Fatalf("text = %q, want the last thing a person typed", text)
	}
}
