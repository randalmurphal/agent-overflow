package store

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Paging tests cover:
//   - History windows, budgets, and probes count top-level rows only —
//     subagent children load on demand via ListSubagentDescendants and
//     are summarised on their anchor by decorateSubagentAnchors.
//   - The cursor pagers (ListItemsBeforeCursor / ListItemsAfterCursor)
//     respect their exclusive bound and report HasMoreOlder /
//     HasMoreNewer consistently.
//   - ListThreadSliceAround splits its budget around the anchor and
//     falls back to the tail when the anchor is gone.
//
// Their SQL is covered separately by timeline_arms_test.go: the pagers
// select ids through the physical timeline arms, and that file owns both
// the view-parity oracle and the plan tripwire.

// seedItem inserts one item into a thread. Caller supplies the bare
// minimum fields; the helper defaults the rest so individual tests stay
// focused on the ordering / parent / payload structure they care about.
func seedItem(t *testing.T, s *Store, threadID, id string, turnIndex, itemIndex int, parentID string) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      "assistant_text",
		Role:      "assistant",
		Summary:   id,
		ParentID:  parentID,
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}); err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
}

// seedAnchorItem persists a completed tool_call row — the shape that can
// anchor subagent children and receive decorateSubagentAnchors meta.
func seedAnchorItem(t *testing.T, s *Store, threadID, id string, turnIndex, itemIndex int) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      "tool_call",
		Role:      "assistant",
		ToolName:  "Task",
		Summary:   id,
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}); err != nil {
		t.Fatalf("seed anchor item %s: %v", id, err)
	}
}

func seedActivityItem(t *testing.T, s *Store, threadID, id string, turnIndex, itemIndex int, parentID string) {
	t.Helper()
	if err := s.InsertItem(Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      "tool_call",
		Role:      "assistant",
		ToolName:  "Read",
		Summary:   id,
		ParentID:  parentID,
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}); err != nil {
		t.Fatalf("seed activity item %s: %v", id, err)
	}
}

// seedBackgroundItem persists a background tool_call with a deterministic
// status + created_at so the tray-retention tests can stage precise
// time-windowed state without hand-rolling inserts per case.
func seedBackgroundItem(
	t *testing.T,
	s *Store,
	threadID, id string,
	turnIndex, itemIndex int,
	status, completionOf string,
	createdAt int64,
) {
	t.Helper()
	item := Item{
		ID:           id,
		ThreadID:     threadID,
		TurnIndex:    turnIndex,
		ItemIndex:    itemIndex,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       status,
		Summary:      id,
		IsBackground: completionOf == "",
		CompletionOf: completionOf,
		CreatedAt:    createdAt,
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("seed background item %s: %v", id, err)
	}
}

// seedPayloadItem persists an item carrying a payload so the
// thread-aggregate queries that filter on payloads.kind have rows to
// match. Reuses InsertItemWithPayload so the item and payload land in a
// single transaction, matching the production write path.
func seedPayloadItem(
	t *testing.T,
	s *Store,
	threadID, id string,
	turnIndex, itemIndex int,
	kind, payloadKind, payloadMeta string,
) {
	t.Helper()
	payloadID := "p-" + threadID + "-" + id
	payload := Payload{
		ID:        payloadID,
		Kind:      payloadKind,
		Meta:      payloadMeta,
		Data:      []byte("body-" + id),
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}
	item := Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      kind,
		Role:      "assistant",
		Summary:   id,
		PayloadID: payloadID,
		CreatedAt: int64(turnIndex*10 + itemIndex),
	}
	if err := s.InsertItemWithPayload(item, payload); err != nil {
		t.Fatalf("seed payload item %s: %v", id, err)
	}
}

// idForTurn and idForTurnItem synthesize stable ids for the seed helpers.
// Sequential ids make the ordered-slice assertions human-readable when a
// test fails.
func idForTurn(turn int) string {
	return "turn-" + strconv.Itoa(turn)
}

func idForTurnItem(turn, item int) string {
	return "t" + strconv.Itoa(turn) + "-i" + strconv.Itoa(item)
}

func collectIDs(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// Regression pin for the recursive-CTE's outer thread_id filter.
// Seed an id collision on thread B that is NOT an ancestor of
// anything on thread A — removing the outer `items.thread_id = ?`
// predicate would let the collider through. The cross-thread
// ancestor test already covers the ancestor branch; this test
// covers the non-ancestor branch.
func TestListItemsBeforeCursor_OuterThreadFilterRequired(t *testing.T) {
	// The items PK is `(thread_id, id)`, so the same item id can exist
	// on two threads. querySelectedPagedItems hydrates the page through
	// `JOIN selected ON selected.id = items.id` — without the outer
	// `items.thread_id = ?` filter, a colliding id on another thread
	// would join in alongside the legitimate row.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("a", "claude")); err != nil {
		t.Fatalf("create thread a: %v", err)
	}
	if err := s.CreateThread(makeThread("b", "claude")); err != nil {
		t.Fatalf("create thread b: %v", err)
	}
	seedItem(t, s, "a", "X", 0, 0, "")
	seedItem(t, s, "a", "Y", 1, 0, "")
	seedItem(t, s, "a", "Z", 2, 0, "")
	// Thread B: same id "X" at a different coordinate.
	seedItem(t, s, "b", "X", 5, 0, "")

	paged, err := s.ListItemsBeforeCursor("a", TimelineCursor{TurnIndex: 2, ItemIndex: 0, ItemID: "Z"}, 10)
	if err != nil {
		t.Fatalf("list before cursor: %v", err)
	}
	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"X", "Y"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	for _, it := range paged.Items {
		if it.ThreadID != "a" {
			t.Errorf("cross-thread leak: item %s came from %s, want a", it.ID, it.ThreadID)
		}
	}
}

func TestListThreadSliceAround_EmptyAnchorReturnsTail(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Five turns, one item each.
	for i := 0; i < 5; i++ {
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}

	paged, err := s.ListThreadSliceAround("t", "", 3)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Tail of size 3 = the three newest turns.
	wantIDs := []string{idForTurn(2), idForTurn(3), idForTurn(4)}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 2 {
		t.Errorf("oldest: got %d, want 2", paged.OldestTurnIndex)
	}
	if paged.NewestTurnIndex != 4 {
		t.Errorf("newest: got %d, want 4", paged.NewestTurnIndex)
	}
	if !paged.HasMore {
		t.Error("expected HasMore=true (turns 0,1 below floor)")
	}
	if !paged.HasMoreOlder {
		t.Error("expected HasMoreOlder=true (turns 0,1 below floor)")
	}
	if paged.HasMoreNewer {
		t.Error("tail slice should not report newer history")
	}
}

func TestListThreadSliceAround_EmptyThreadReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	paged, err := s.ListThreadSliceAround("t", "", 50)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}
	if len(paged.Items) != 0 {
		t.Errorf("empty thread items: got %d, want 0", len(paged.Items))
	}
	if paged.OldestTurnIndex != -1 {
		t.Errorf("oldest: got %d, want -1", paged.OldestTurnIndex)
	}
	if paged.NewestTurnIndex != -1 {
		t.Errorf("newest: got %d, want -1", paged.NewestTurnIndex)
	}
	if paged.HasMore {
		t.Error("empty thread should not report HasMore")
	}
	if paged.HasMoreOlder || paged.HasMoreNewer {
		t.Error("empty thread should not report older/newer history")
	}
}

func TestListThreadSliceAround_AnchorInMiddle(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Ten turns, one item each. Anchor on turn 5.
	for i := 0; i < 10; i++ {
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}

	paged, err := s.ListThreadSliceAround("t", idForTurn(5), 4)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Half budget=2 each side. Floor walks at-or-below 5 picking up
	// turns 5,4 (cumulative=2, stops). Upper walks above 5 picking up
	// turns 6,7. Window [4,7] inclusive yields four items.
	wantIDs := []string{idForTurn(4), idForTurn(5), idForTurn(6), idForTurn(7)}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 4 {
		t.Errorf("oldest: got %d, want 4", paged.OldestTurnIndex)
	}
	if paged.NewestTurnIndex != 7 {
		t.Errorf("newest: got %d, want 7", paged.NewestTurnIndex)
	}
	if !paged.HasMore {
		t.Error("expected HasMore=true (turns 0..3 below floor)")
	}
	if !paged.HasMoreOlder {
		t.Error("expected HasMoreOlder=true (turns 0..3 below floor)")
	}
	if !paged.HasMoreNewer {
		t.Error("expected HasMoreNewer=true (turns 8..9 above ceiling)")
	}
}

func TestListItemsBeforeCursor_CapsWithinDenseTurn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for index := 0; index < 10; index++ {
		seedItem(t, s, "t", fmt.Sprintf("i-%d", index), 0, index, "")
	}

	paged, err := s.ListItemsBeforeCursor("t", TimelineCursor{TurnIndex: 0, ItemIndex: 8, ItemID: "i-8"}, 3)
	if err != nil {
		t.Fatalf("items before cursor: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"i-5", "i-6", "i-7"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestCursor.ItemIndex != 5 {
		t.Errorf("oldest item cursor = %d, want 5", paged.OldestCursor.ItemIndex)
	}
	if paged.NewestCursor.ItemIndex != 7 {
		t.Errorf("newest item cursor = %d, want 7", paged.NewestCursor.ItemIndex)
	}
	if !paged.HasMoreOlder {
		t.Error("expected older items before i-5")
	}
	if !paged.HasMoreNewer {
		t.Error("expected newer items after i-7")
	}
}

// TestPagingCursorsAcceptHeadHealedNegativeIndex pins R9-1 (round 9):
// head-healed prompts persist at negative item indexes, so a page
// bounded by one must keep its cursor valid — older/newer paging from
// a {turn, -1} cursor pages normally instead of returning empty.
func TestPagingCursorsAcceptHeadHealedNegativeIndex(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedItem(t, s, "t", "t0-a", 0, 0, "")
	seedItem(t, s, "t", "t0-b", 0, 1, "")
	seedItem(t, s, "t", "t1-head", 1, -1, "")
	seedItem(t, s, "t", "t1-a", 1, 0, "")

	paged, err := s.ListItemsBeforeCursor("t", TimelineCursor{TurnIndex: 1, ItemIndex: -1, ItemID: "t1-head"}, 10)
	if err != nil {
		t.Fatalf("items before head cursor: %v", err)
	}
	if got, want := collectIDs(paged.Items), []string{"t0-a", "t0-b"}; !equalStringSlice(got, want) {
		t.Errorf("items before head-healed prompt: got %v, want %v", got, want)
	}

	paged, err = s.ListItemsAfterCursor("t", TimelineCursor{TurnIndex: 0, ItemIndex: 1, ItemID: "t0-b"}, 10)
	if err != nil {
		t.Fatalf("items after cursor: %v", err)
	}
	if got, want := collectIDs(paged.Items), []string{"t1-head", "t1-a"}; !equalStringSlice(got, want) {
		t.Errorf("items after turn 0: got %v, want %v", got, want)
	}
	if paged.OldestCursor.ItemIndex != -1 {
		t.Errorf("oldest cursor item index = %d, want -1 (the head-healed prompt)", paged.OldestCursor.ItemIndex)
	}

	// The empty sentinel stays invalid: its TurnIndex is -1.
	paged, err = s.ListItemsBeforeCursor("t", emptyTimelineCursor(), 10)
	if err != nil {
		t.Fatalf("items before sentinel: %v", err)
	}
	if len(paged.Items) != 0 {
		t.Errorf("sentinel cursor paged %v, want empty", collectIDs(paged.Items))
	}
}

func TestListItemsAfterCursor_CapsWithinDenseTurn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for index := 0; index < 10; index++ {
		seedItem(t, s, "t", fmt.Sprintf("i-%d", index), 0, index, "")
	}

	paged, err := s.ListItemsAfterCursor("t", TimelineCursor{TurnIndex: 0, ItemIndex: 1, ItemID: "i-1"}, 3)
	if err != nil {
		t.Fatalf("items after cursor: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"i-2", "i-3", "i-4"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestCursor.ItemIndex != 2 {
		t.Errorf("oldest item cursor = %d, want 2", paged.OldestCursor.ItemIndex)
	}
	if paged.NewestCursor.ItemIndex != 4 {
		t.Errorf("newest item cursor = %d, want 4", paged.NewestCursor.ItemIndex)
	}
	if !paged.HasMoreOlder {
		t.Error("expected older items before i-2")
	}
	if !paged.HasMoreNewer {
		t.Error("expected newer items after i-4")
	}
}

func TestListThreadSliceAround_CapsWithinDenseTurn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for index := 0; index < 10; index++ {
		seedItem(t, s, "t", fmt.Sprintf("i-%d", index), 0, index, "")
	}

	paged, err := s.ListThreadSliceAround("t", "i-5", 4)
	if err != nil {
		t.Fatalf("slice around dense turn: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"i-4", "i-5", "i-6", "i-7"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestCursor.ItemIndex != 4 {
		t.Errorf("oldest item cursor = %d, want 4", paged.OldestCursor.ItemIndex)
	}
	if paged.NewestCursor.ItemIndex != 7 {
		t.Errorf("newest item cursor = %d, want 7", paged.NewestCursor.ItemIndex)
	}
	if !paged.HasMoreOlder || !paged.HasMoreNewer {
		t.Error("expected both older and newer items around dense-turn slice")
	}
}

func TestListThreadSliceAround_MissingAnchorFallsBackToTail(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for i := 0; i < 4; i++ {
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}

	paged, err := s.ListThreadSliceAround("t", "ghost", 2)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{idForTurn(2), idForTurn(3)}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
}

func TestListThreadSliceAround_ChildAnchorPositionsWindow(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Subagent parent in turn 0; anchor child in turn 6. The child's
	// coordinate positions the slice (scroll-to-item lands on its
	// group), but the rows loaded are the top-level neighborhood — the
	// child itself hydrates through ListSubagentDescendants.
	seedAnchorItem(t, s, "t", "parent", 0, 0)
	for i := 1; i <= 5; i++ {
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}
	seedItem(t, s, "t", "child", 6, 0, "parent")

	paged, err := s.ListThreadSliceAround("t", "child", 4)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Half=2 at-or-before (6,0): top-level DESC → turn-5, turn-4.
	// Half=2 after (6,0): no top-level rows above. The parent (turn 0)
	// is NOT stitched in — it loads when the user pages back to it.
	wantIDs := []string{idForTurn(4), idForTurn(5)}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 4 {
		t.Errorf("oldest cursor: got %d, want 4", paged.OldestTurnIndex)
	}
	if !paged.HasMoreOlder {
		t.Error("expected HasMoreOlder=true because turns 0-3 remain omitted below the slice")
	}
	if paged.HasMoreNewer {
		t.Error("expected HasMoreNewer=false: only the subagent child sits above the slice")
	}
}

func TestListThreadSliceAround_ChildAnchorLoadsNoSiblings(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Subagent group: parent in turn 1, six children spanning turns 1-6.
	// Anchoring on a child must load only top-level rows: the parent
	// anchor (decorated with the group aggregates) and the surrounding
	// noise. Siblings hydrate on demand when the group card expands.
	seedAnchorItem(t, s, "t", "parent", 1, 0)
	seedItem(t, s, "t", "c-1", 1, 1, "parent")
	seedItem(t, s, "t", "c-2", 2, 0, "parent")
	seedItem(t, s, "t", "c-3", 3, 0, "parent")
	seedItem(t, s, "t", "c-4", 4, 0, "parent")
	seedItem(t, s, "t", "c-5", 5, 0, "parent")
	seedActivityItem(t, s, "t", "c-6", 6, 0, "parent")
	// Some unrelated noise above and below the group so the window is
	// definitely not "the whole thread."
	seedItem(t, s, "t", "noise-0", 0, 0, "")
	seedItem(t, s, "t", "noise-7", 7, 0, "")

	paged, err := s.ListThreadSliceAround("t", "c-4", 2)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Half=1 each side of c-4's coordinate (4,0): at-or-before DESC →
	// parent (1,0); after ASC → noise-7 (7,0). No sibling child loads.
	wantIDs := []string{"parent", "noise-7"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestCursor.ItemID != "parent" {
		t.Errorf("oldest cursor = %q, want parent", paged.OldestCursor.ItemID)
	}
	// The collapsed card still knows its size and latest activity via
	// the decorated aggregates.
	meta := paged.Items[0].Meta
	if !strings.Contains(meta, `"subagentDescendantCount":6`) {
		t.Errorf("parent meta = %s, want subagentDescendantCount 6", meta)
	}
	if !strings.Contains(meta, `"subagentLatestChildSummary":"c-6"`) {
		t.Errorf("parent meta = %s, want latest child summary c-6", meta)
	}
}

func TestListThreadSliceAround_CrossThreadIsolation(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("a", "claude")); err != nil {
		t.Fatalf("create thread a: %v", err)
	}
	if err := s.CreateThread(makeThread("b", "claude")); err != nil {
		t.Fatalf("create thread b: %v", err)
	}
	// Same id "anchor" lives on both threads at different turns. The
	// outer items.thread_id guard MUST prevent thread b's row from
	// leaking when we query thread a.
	seedItem(t, s, "a", "anchor", 5, 0, "")
	seedItem(t, s, "a", "neighbor", 6, 0, "")
	seedItem(t, s, "b", "anchor", 0, 0, "")
	seedItem(t, s, "b", "intruder", 1, 0, "")

	paged, err := s.ListThreadSliceAround("a", "anchor", 4)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Only thread "a" rows should appear, regardless of cross-thread id.
	for _, id := range gotIDs {
		if id == "intruder" {
			t.Errorf("thread b row leaked into thread a slice")
		}
	}
	wantIDs := []string{"anchor", "neighbor"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
}

func TestListThreadSliceAround_DefaultsToFiftyItems(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for i := 0; i < 80; i++ {
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}

	paged, err := s.ListThreadSliceAround("t", "", 0) // <=0 → default 50
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}
	if got := len(paged.Items); got != 50 {
		t.Errorf("default item count: got %d, want 50", got)
	}
	// Tail of 50 from a thread of 80 → floor at turn 30.
	if paged.OldestTurnIndex != 30 {
		t.Errorf("oldest: got %d, want 30", paged.OldestTurnIndex)
	}
	if !paged.HasMore {
		t.Error("expected HasMore=true (turns 0..29 below floor)")
	}
}

func TestListThreadSliceAround_FiltersPlanUpdateNotifications(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Seed plain items plus a plan_update notification mid-window.
	seedItem(t, s, "t", "a", 0, 0, "")
	seedItem(t, s, "t", "b", 1, 0, "")
	if err := s.InsertItem(Item{
		ID:        "p",
		ThreadID:  "t",
		TurnIndex: 1,
		ItemIndex: 1,
		Kind:      "notification",
		ToolName:  "plan_update",
		Role:      "system",
		Summary:   "p",
		CreatedAt: 11,
	}); err != nil {
		t.Fatalf("insert plan notif: %v", err)
	}
	seedItem(t, s, "t", "c", 2, 0, "")

	paged, err := s.ListThreadSliceAround("t", "b", 4)
	if err != nil {
		t.Fatalf("slice around: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// "p" must be filtered out the same way the other paged paths do.
	wantIDs := []string{"a", "b", "c"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
}

func idForItem(turn, idx int) string {
	return fmt.Sprintf("t%d-i%d", turn, idx)
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
