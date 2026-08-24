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
//   - ListItemsBeforeTurn respects the exclusive upper bound and reports
//     HasMore + OldestTurnIndex consistently.
//   - PickInitialFloorTurn produces windows that honour minItems /
//     maxItems / turnLimit invariants for the typical thread shapes.

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

func TestListRecentItems_NoChildren(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Two turns, two items each, no parent relationships.
	seedItem(t, s, "t", "a", 0, 0, "")
	seedItem(t, s, "t", "b", 0, 1, "")
	seedItem(t, s, "t", "c", 1, 0, "")
	seedItem(t, s, "t", "d", 1, 1, "")

	paged, err := s.ListRecentItems("t", 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"c", "d"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 1 {
		t.Errorf("oldest: got %d, want 1", paged.OldestTurnIndex)
	}
	if !paged.HasMore {
		t.Error("expected HasMore=true with turn 0 below the floor")
	}
}

// TestListRecentItemsFiltersPlanUpdateNotifications guards the
// timeline projection: pre-existing plan_update notification rows from
// before the live-panel feature must not surface on read. Every other
// notification kind currently in use (warning, hook, review_status,
// model_verification, deprecation_notice) MUST still render — the
// filter targets only `tool_name='plan_update'`.
func TestListRecentItemsFiltersPlanUpdateNotifications(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	mustInsert := func(id, kind, role, toolName string, turn, idx int) {
		t.Helper()
		if err := s.InsertItem(Item{
			ID:        id,
			ThreadID:  "t",
			TurnIndex: turn,
			ItemIndex: idx,
			Kind:      kind,
			Role:      role,
			ToolName:  toolName,
			Summary:   id,
			CreatedAt: int64(turn*10 + idx),
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	mustInsert("text-1", "assistant_text", "assistant", "", 0, 0)
	mustInsert("plan-1", "notification", "system", "plan_update", 0, 1)
	mustInsert("hook-1", "notification", "system", "hook", 0, 2)
	mustInsert("review-1", "notification", "system", "review_status", 0, 3)
	mustInsert("warn-1", "notification", "system", "warning", 0, 4)
	mustInsert("modver-1", "notification", "system", "model_verification", 0, 5)
	mustInsert("deprec-1", "notification", "system", "deprecation_notice", 0, 6)
	mustInsert("text-2", "assistant_text", "assistant", "", 0, 7)

	paged, err := s.ListRecentItems("t", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := collectIDs(paged.Items)
	// Every kind except plan_update must survive the filter; the order
	// matches insertion order since the projection sorts by
	// (turn_index, item_index).
	want := []string{"text-1", "hook-1", "review-1", "warn-1", "modver-1", "deprec-1", "text-2"}
	if !equalStringSlice(got, want) {
		t.Errorf("items: got %v, want %v (plan_update filtered, others kept)", got, want)
	}
}

func TestListRecentItemsDecoratesProposedPlans(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedPayloadItem(t, s, "t", "plan-1", 0, 0, "tool_call", "proposed_plan", "{}")
	if _, err := s.EnsureProposedPlanState("t", "plan-1", 100); err != nil {
		t.Fatalf("ensure plan: %v", err)
	}
	if _, err := s.CreateProposedPlanComment(ProposedPlanComment{
		ID:         "comment-1",
		ThreadID:   "t",
		PlanItemID: "plan-1",
		StartLine:  1,
		EndLine:    1,
		Body:       "tighten this",
		CreatedAt:  101,
		UpdatedAt:  101,
	}); err != nil {
		t.Fatalf("create comment: %v", err)
	}

	page, err := s.ListRecentItems("t", 0)
	if err != nil {
		t.Fatalf("list recent items: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	meta := page.Items[0].Meta
	if !strings.Contains(meta, `"planVersion":1`) {
		t.Fatalf("meta = %s, want plan version", meta)
	}
	if !strings.Contains(meta, `"draft":1`) {
		t.Fatalf("meta = %s, want draft comment count", meta)
	}
}

func TestListRecentItems_ExcludesSubagentChildren(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedAnchorItem(t, s, "t", "parent", 0, 0)
	seedItem(t, s, "t", "filler1", 1, 0, "")
	seedItem(t, s, "t", "child", 2, 0, "parent")

	paged, err := s.ListRecentItems("t", 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// child (turn 2) is inside the requested range but carries a
	// parent_id: it renders inside its anchor's SubagentGroup card and
	// loads via ListSubagentDescendants, never as a timeline row.
	wantIDs := []string{"filler1"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 1 {
		t.Errorf("oldest: got %d, want 1", paged.OldestTurnIndex)
	}
	if !paged.HasMoreOlder {
		t.Error("expected HasMoreOlder=true because top-level parent sits at turn 0")
	}
	// The child-only turn above the window must not advertise a newer
	// history gap — there is nothing the frontend could load there.
	if paged.HasMoreNewer {
		t.Error("expected HasMoreNewer=false: only subagent children exist above the window")
	}
}

func TestListRecentItems_ChildOnlyOlderTurnsDontFlagHasMore(t *testing.T) {
	// Regression guard for the original bug: "Load older messages"
	// appeared when the only rows below the floor were subagent
	// children — which never render as timeline rows, so clicking the
	// button loaded nothing.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	seedAnchorItem(t, s, "t", "anchor", 2, 0)
	seedItem(t, s, "t", "c-0", 0, 0, "anchor")
	seedItem(t, s, "t", "c-1", 1, 0, "anchor")

	paged, err := s.ListRecentItems("t", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !equalStringSlice(collectIDs(paged.Items), []string{"anchor"}) {
		t.Errorf("items: got %v, want [anchor]", collectIDs(paged.Items))
	}
	if paged.HasMore || paged.HasMoreOlder {
		t.Error("HasMoreOlder must be false: turns 0-1 hold only subagent children")
	}
}

func TestListItemsBeforeTurn_RespectsUpperBound(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Four turns, all with corresponding turns rows. The item-budget
	// walker reads from `items` directly (GROUP BY turn_index) so the
	// `turns` rows are not strictly required, but seeding both mirrors
	// the production shape.
	for i := 0; i < 4; i++ {
		if err := s.InsertTurn(Turn{
			TurnID: idForTurn(i), ThreadID: "t", TurnIndex: i, StartedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}

	// Request the two turns below turn 3 — expect turns 1 and 2.
	paged, err := s.ListItemsBeforeTurn("t", 3, 2)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{idForTurn(1), idForTurn(2)}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 1 {
		t.Errorf("oldest: got %d, want 1", paged.OldestTurnIndex)
	}
	if !paged.HasMore {
		t.Error("HasMore should be true (turn 0 still below)")
	}
	// No item from turn 3 or above may leak through the upper bound.
	for _, it := range paged.Items {
		if it.TurnIndex >= 3 {
			t.Errorf("leaked upper-bound item %s at turn %d", it.ID, it.TurnIndex)
		}
	}
}

func TestListItemsBeforeTurn_EmptyTail(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Thread has no items at all.
	paged, err := s.ListItemsBeforeTurn("t", 0, 10)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	if len(paged.Items) != 0 {
		t.Errorf("items: got %d, want 0", len(paged.Items))
	}
	if paged.OldestTurnIndex != -1 {
		t.Errorf("oldest: got %d, want -1", paged.OldestTurnIndex)
	}
	if paged.HasMore {
		t.Error("HasMore must be false for an empty tail")
	}
}

func TestListItemsBeforeTurn_ChildRowsDontCountOrReturn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Thread has turns 0..4 with a subagent child in turn 2. Paging
	// back "before turn 3, budget 2" must treat turn 2 as empty: the
	// child neither consumes budget nor appears in the page, so the
	// walk reaches turn 0 in a single call.
	for i := 0; i < 5; i++ {
		if err := s.InsertTurn(Turn{
			TurnID: idForTurn(i), ThreadID: "t", TurnIndex: i, StartedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
	}
	seedAnchorItem(t, s, "t", "old-parent", 0, 0)
	seedItem(t, s, "t", "filler1", 1, 0, "")
	seedItem(t, s, "t", "child-in-batch", 2, 0, "old-parent")
	seedItem(t, s, "t", "filler3", 3, 0, "")
	seedItem(t, s, "t", "filler4", 4, 0, "")

	paged, err := s.ListItemsBeforeTurn("t", 3, 2)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"old-parent", "filler1"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 0 {
		t.Errorf("oldest: got %d, want 0 (child-only turn 2 contributes no budget)", paged.OldestTurnIndex)
	}
	if paged.HasMore {
		t.Error("HasMore should be false: the walk reached turn 0")
	}
}

func TestListItemsBeforeTurn_DoesNotReturnAlreadyLoaded(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Caller's window: turn_index >= 3. A parent in turn 5 is already
	// in the window; a child in turn 6 has that parent. ListItemsBeforeTurn
	// for "turns below 3" must NOT return the already-loaded parent row.
	for i := 0; i < 7; i++ {
		if err := s.InsertTurn(Turn{
			TurnID: idForTurn(i), ThreadID: "t", TurnIndex: i, StartedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
	}
	seedItem(t, s, "t", "old-0", 0, 0, "")
	seedItem(t, s, "t", "old-1", 1, 0, "")
	seedItem(t, s, "t", "old-2", 2, 0, "")
	seedItem(t, s, "t", "already-loaded-parent", 5, 0, "")
	seedItem(t, s, "t", "already-loaded-child", 6, 0, "already-loaded-parent")

	paged, err := s.ListItemsBeforeTurn("t", 3, 5)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Must be just the three below-floor items; NOT already-loaded-parent.
	wantIDs := []string{"old-0", "old-1", "old-2"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
}

func TestPickInitialFloorTurn_EmptyThread(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	floor, hasMore, err := s.PickInitialFloorTurn("t", 50, 500, 2000)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if floor != -1 {
		t.Errorf("floor: got %d, want -1 (empty thread)", floor)
	}
	if hasMore {
		t.Error("hasMore must be false for empty thread")
	}
}

func TestPickInitialFloorTurn_IncludesActiveTurnWithNoItems(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Turn 0 has items; turn 1 is in-flight with no items yet.
	if err := s.InsertTurn(Turn{TurnID: "t0", ThreadID: "t", TurnIndex: 0, StartedAt: 100}); err != nil {
		t.Fatalf("insert turn 0: %v", err)
	}
	seedItem(t, s, "t", "a", 0, 0, "")
	if err := s.InsertTurn(Turn{TurnID: "t1", ThreadID: "t", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("insert turn 1 (in-flight): %v", err)
	}

	floor, hasMore, err := s.PickInitialFloorTurn("t", 1, 0, 10)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	// Must not exclude turn 1 — even though it has no items, the active-
	// turn guarantee keeps it inside the window.
	if floor > 0 {
		t.Errorf("floor: got %d, want <= 0 so active turn 1 is covered", floor)
	}
	// The window loaded every turn this thread has. Nothing older exists,
	// so `hasMore` must be false — the Load Older button would lie
	// otherwise. Assert it rather than discarding the return value.
	if hasMore {
		t.Error("hasMore: got true, want false (every turn is inside the window)")
	}
}

func TestPickInitialFloorTurn_BurstyThread(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// 100 turns, one item each. With turnLimit=10, minItems=50 the
	// window should grow past 10 turns until it has at least 50 items.
	for i := 0; i < 100; i++ {
		seedItem(t, s, "t", idForTurn(i), i, 0, "")
	}

	floor, hasMore, err := s.PickInitialFloorTurn("t", 10, 50, 200)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	// 100 turns each contributing 1 item. The window must grow to at
	// least 50 items → floor <= 50.
	if floor > 50 {
		t.Errorf("floor: got %d, want <= 50 (minItems must force growth)", floor)
	}
	if !hasMore {
		t.Error("hasMore should be true (older turns exist)")
	}
}

func TestPickInitialFloorTurn_AgentHeavyTurn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Three turns of 300 items each = 900 total. turnLimit=10, cap=500:
	// the window includes turn 2 (300), stops before adding turn 1 (600
	// would exceed 500) so we return floor=2.
	for turn := 0; turn < 3; turn++ {
		for i := 0; i < 300; i++ {
			seedItem(t, s, "t", idForTurnItem(turn, i), turn, i, "")
		}
	}

	floor, hasMore, err := s.PickInitialFloorTurn("t", 10, 200, 500)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if floor != 2 {
		t.Errorf("floor: got %d, want 2 (single large turn fits, next would exceed cap)", floor)
	}
	if !hasMore {
		t.Error("hasMore should be true with turns 0 and 1 below")
	}
}

func TestPickInitialFloorTurn_SubagentChildrenDontCount(t *testing.T) {
	// Regression guard for the original heap bug: a subagent-heavy turn
	// used to weigh hundreds of child rows against the window budget,
	// pinning the initial floor right above real history and flashing
	// "Load older messages" for rows that never render as timeline rows.
	// Top-level counting weighs the launch turn as one row (its anchor).
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for i := 0; i < 10; i++ {
		seedItem(t, s, "t", idForTurnItem(0, i), 0, i, "")
	}
	seedAnchorItem(t, s, "t", "anchor", 1, 0)
	for i := 1; i <= 300; i++ {
		seedItem(t, s, "t", fmt.Sprintf("agent-child-%d", i), 1, i, "anchor")
	}
	for i := 0; i < 5; i++ {
		seedItem(t, s, "t", idForTurnItem(2, i), 2, i, "")
	}

	floor, hasMore, err := s.PickInitialFloorTurn("t", 10, 0, 100)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	// Top-level rows: turn 2 = 5, turn 1 = 1 (anchor only), turn 0 = 10;
	// 16 total fits the 100-item cap, so the whole thread loads.
	if floor != 0 {
		t.Errorf("floor: got %d, want 0 (300 children must not count against maxItems)", floor)
	}
	if hasMore {
		t.Error("hasMore: got true, want false (every top-level row is inside the window)")
	}
}

func TestPickInitialFloorTurn_SingleHugeTurnLoadsAnyway(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// A single turn of 3000 items. Even though it exceeds maxItems
	// (2000), it must still be loaded whole — mid-turn cuts are disallowed.
	for i := 0; i < 3000; i++ {
		seedItem(t, s, "t", idForTurnItem(0, i), 0, i, "")
	}

	floor, _, err := s.PickInitialFloorTurn("t", 50, 500, 2000)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if floor != 0 {
		t.Errorf("floor: got %d, want 0 (only turn must be selected whole)", floor)
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

func TestPickInitialFloorTurn_IgnoresObsoleteInflightBelowPicked(t *testing.T) {
	// Crash recovery scenario: an older turn was left with
	// `completed_at = NULL` after a process crash, and a later
	// completed turn sits above it with items. Only the latest turn row
	// can be live, so the older NULL row is historical corruption and
	// must not lower the initial floor.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// InsertTurn always writes completed_at=NULL; use
	// UpdateTurnCompleted to close the ones we want to be settled.
	if err := s.InsertTurn(Turn{TurnID: "t0", ThreadID: "t", TurnIndex: 0, StartedAt: 0}); err != nil {
		t.Fatalf("insert turn 0: %v", err)
	}
	if err := s.UpdateTurnCompleted("t0", 100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete turn 0: %v", err)
	}
	seedItem(t, s, "t", "old-0", 0, 0, "")
	// Turn 1: obsolete in-flight row (stays NULL). No items yet.
	if err := s.InsertTurn(Turn{TurnID: "t1", ThreadID: "t", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("insert turn 1 in-flight: %v", err)
	}
	// Turn 5: completed, with items. PickInitialFloorTurn walking
	// newest to oldest with turnLimit=1 should pick turn 5 and ignore
	// the older NULL row.
	if err := s.InsertTurn(Turn{TurnID: "t5", ThreadID: "t", TurnIndex: 5, StartedAt: 400}); err != nil {
		t.Fatalf("insert turn 5: %v", err)
	}
	if err := s.UpdateTurnCompleted("t5", 500, "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete turn 5: %v", err)
	}
	seedItem(t, s, "t", "new-5", 5, 0, "")

	floor, hasMore, err := s.PickInitialFloorTurn("t", 1, 0, 10)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if floor != 5 {
		t.Errorf("floor: got %d, want 5 (obsolete in-flight turn 1 must be ignored)", floor)
	}
	// Turn 0 has items and sits below the returned floor.
	if !hasMore {
		t.Error("hasMore: got false, want true (turn 0 has items below the returned floor)")
	}
}

func TestPickInitialFloorTurn_EmptyThreadActiveOnlyReportsHasMoreForOlderRows(t *testing.T) {
	// Another crash-recovery edge: the items table is empty (fresh
	// thread after a DB-level item wipe, or a restored backup that
	// lost item rows) but the turns table has both an active turn and
	// an older orphan NULL-completed row. The earlier implementation
	// returned hasMore=false unconditionally for the "no items, one
	// active turn" branch; now it probes and reports accurately.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Older turn row, NULL completed_at. No items. Represents an
	// orphan interrupted turn.
	if err := s.InsertTurn(Turn{TurnID: "t0", ThreadID: "t", TurnIndex: 0, StartedAt: 10}); err != nil {
		t.Fatalf("insert turn 0 in-flight: %v", err)
	}
	// Newer active turn. Also no items.
	if err := s.InsertTurn(Turn{TurnID: "t3", ThreadID: "t", TurnIndex: 3, StartedAt: 300}); err != nil {
		t.Fatalf("insert turn 3: %v", err)
	}

	floor, hasMore, err := s.PickInitialFloorTurn("t", 1, 0, 10)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	// GetActiveTurn returns the latest in-flight turn (turn 3).
	if floor != 3 {
		t.Errorf("floor: got %d, want 3 (latest active turn)", floor)
	}
	// The items table is empty so `hasOlderTurns` returns false even
	// though the turns table has an older row. This is accurate: the
	// caller's "older items" promise can't be fulfilled from the
	// items table alone, and the turns-only row has no items to load.
	// Pinning the current behavior here prevents a silent regression
	// if the probe semantics change.
	if hasMore {
		t.Error("hasMore: got true, want false (no older item rows, even with an older turn row)")
	}
}

// Pins the cap on the active-turn floor adjustment: when honoring the
// active turn would blow the caller's maxItems budget by a large
// margin, the adjustment is skipped and `picked` stays at the
// walk-derived floor. The active turn's items still reach the UI via
// the streaming path, and the user can Load Older to surface it.
func TestPickInitialFloorTurn_ActiveTurnAdjustmentRespectsMaxItems(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Two heavy completed turns (5k items each) and one far-below
	// active turn. Walk picks turn 10 (cumulative=5000, fits
	// maxItems=5000 exactly). If we naively lowered to activeFloor=1
	// we'd pull in turn 5's 5000 items plus everything between →
	// 10k+ items, violating the cap.
	for i := 0; i < 5000; i++ {
		seedItem(t, s, "t", idForItem(5, i), 5, i, "")
	}
	for i := 0; i < 5000; i++ {
		seedItem(t, s, "t", idForItem(10, i), 10, i, "")
	}
	if err := s.InsertTurn(Turn{TurnID: "t1", ThreadID: "t", TurnIndex: 1, StartedAt: 100}); err != nil {
		t.Fatalf("insert active turn: %v", err)
	}
	// Don't complete turn 1 — it's the crash-interrupted active row.

	floor, _, err := s.PickInitialFloorTurn("t", 1, 0, 5000)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	// Walk picks turn 10 first (5000 items = maxItems). Lowering to
	// turn 1 would blow the budget; the cap keeps picked at 10.
	if floor != 10 {
		t.Errorf("floor: got %d, want 10 (cap preserves maxItems=5000 budget)", floor)
	}
}

// Exercises the `reachable && cumulative+extra > maxItems` branch
// directly. The wide-gap fallback at the bottom of the cap block
// covers the "scan didn't reach activeFloor" case; this test pins
// the precise-budget guard — when the scan DID reach activeFloor
// and we can count every item in the gap, the guard must skip
// adjustment if adding the gap would blow maxItems.
//
// Setup: latest persisted turn 10 is active with 6 items; malformed
// item rows exist at turn 20 without a matching turns row. turnLimit=1,
// maxItems=15. Walker picks 20 (10 items, under maxItems). gap is
// [activeFloor=10, picked=20); turn 10 sits inside with 6 items.
// scanLimit = 4, the scan of GROUP-BY item-counts returns both item
// groups (2 rows, under limit), so lastScanned=10 == activeFloor,
// reachable=true. cumulative+extra = 10+6 = 16 > maxItems=15;
// first branch must skip. picked stays at 20.
func TestPickInitialFloorTurn_ActiveTurnReachableButOverBudget(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Turn 10: latest persisted row, still active.
	if err := s.InsertTurn(Turn{TurnID: "t10", ThreadID: "t", TurnIndex: 10, StartedAt: 1000}); err != nil {
		t.Fatalf("insert turn 10: %v", err)
	}
	for i := 0; i < 6; i++ {
		seedItem(t, s, "t", idForItem(10, i), 10, i, "")
	}
	// Malformed rows above the active turn. This keeps activeFloor < picked
	// without inserting a newer turn row that would make turn 10 obsolete.
	for i := 0; i < 10; i++ {
		seedItem(t, s, "t", idForItem(20, i), 20, i, "")
	}

	floor, _, err := s.PickInitialFloorTurn("t", 1, 0, 15)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	// Lowering would pull turn 10's 6 items → 16 > maxItems=15.
	// Precise-budget guard must keep picked at 20. Removing the
	// `cumulative+extra <= maxItems` predicate from the fix would
	// let picked drop to 10 and fail this assertion.
	if floor != 20 {
		t.Errorf("floor: got %d, want 20 (cap must block adjustment when it blows maxItems)", floor)
	}
}

// Once a newer completed turn row exists, an older NULL-completed row is
// obsolete. Even when its items would fit the budget, it must not be treated
// as live provider work.
func TestPickInitialFloorTurn_ObsoleteInflightReachableAtBudgetIgnored(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertTurn(Turn{TurnID: "t10", ThreadID: "t", TurnIndex: 10, StartedAt: 1000}); err != nil {
		t.Fatalf("insert turn 10: %v", err)
	}
	for i := 0; i < 6; i++ {
		seedItem(t, s, "t", idForItem(10, i), 10, i, "")
	}
	if err := s.InsertTurn(Turn{TurnID: "t20", ThreadID: "t", TurnIndex: 20, StartedAt: 2000}); err != nil {
		t.Fatalf("insert turn 20: %v", err)
	}
	if err := s.UpdateTurnCompleted("t20", 2500, "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete turn 20: %v", err)
	}
	for i := 0; i < 10; i++ {
		seedItem(t, s, "t", idForItem(20, i), 20, i, "")
	}

	floor, _, err := s.PickInitialFloorTurn("t", 1, 0, 16)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if floor != 20 {
		t.Errorf("floor: got %d, want 20 (obsolete in-flight turn 10 must be ignored)", floor)
	}
}

// A lower turn_index row inserted after a newer completed turn is still
// obsolete by index ordering. The active-turn floor is based on the latest
// turn row, not wall-clock insertion order.
func TestPickInitialFloorTurn_ObsoleteAdjacentEmptyTurnIgnored(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertTurn(Turn{TurnID: "t5", ThreadID: "t", TurnIndex: 5, StartedAt: 500}); err != nil {
		t.Fatalf("insert turn 5: %v", err)
	}
	if err := s.UpdateTurnCompleted("t5", 550, "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete turn 5: %v", err)
	}
	seedItem(t, s, "t", "item-5", 5, 0, "")
	// Turn 4: obsolete in-flight row, no items yet. Gap of 1 turn.
	if err := s.InsertTurn(Turn{TurnID: "t4", ThreadID: "t", TurnIndex: 4, StartedAt: 600}); err != nil {
		t.Fatalf("insert active turn 4: %v", err)
	}

	floor, _, err := s.PickInitialFloorTurn("t", 1, 0, 10)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if floor != 5 {
		t.Errorf("floor: got %d, want 5 (adjacent obsolete in-flight turn must be ignored)", floor)
	}
}

// maxItems < minItems is a config accident; the coercion branch at
// the top of PickInitialFloorTurn normalizes maxItems up to minItems
// so the "window never drops below minItems when history exists"
// invariant always holds. Without the coercion, a caller passing
// maxItems=100 / minItems=500 would see windows truncated at the
// MAX cap — half of what the caller specified as the minimum.
func TestPickInitialFloorTurn_CoercesMaxBelowMin(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Seed 300 items spread across 3 turns (100 each). With
	// maxItems=100, a naive walk would stop after turn 2 (100
	// items, hits cap). The coercion should widen maxItems to
	// minItems=500 and pick the oldest turn with everything loaded.
	for turn := 0; turn < 3; turn++ {
		if err := s.InsertTurn(Turn{
			TurnID: idForTurn(turn), ThreadID: "t", TurnIndex: turn, StartedAt: int64(turn) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", turn, err)
		}
		if err := s.UpdateTurnCompleted(idForTurn(turn), int64(turn)*1000+500, "end_turn", "", "", ""); err != nil {
			t.Fatalf("complete turn %d: %v", turn, err)
		}
		for i := 0; i < 100; i++ {
			seedItem(t, s, "t", idForItem(turn, i), turn, i, "")
		}
	}

	floor, _, err := s.PickInitialFloorTurn("t", 1, 500, 100)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if floor != 0 {
		t.Errorf("floor: got %d, want 0 (minItems=500 must override maxItems=100 after coercion)", floor)
	}
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

// Paging back over child-only turns must skip straight to real
// content: the budget walker sees turns 2-3 (subagent children only)
// as empty, so a single budget-1 page from turn 4 lands on the anchor
// turn directly. Under the old child-counting behavior the first page
// stopped on turn 3 and returned rows the timeline never renders.
func TestListItemsBeforeTurn_SkipsChildOnlyTurns(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := s.InsertTurn(Turn{TurnID: idForTurn(i), ThreadID: "t", TurnIndex: i, StartedAt: int64(i) * 1000}); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
	}
	seedAnchorItem(t, s, "t", "anchor-0", 0, 0)
	seedItem(t, s, "t", "child-2", 2, 0, "anchor-0")
	seedItem(t, s, "t", "child-3", 3, 0, "anchor-0")
	seedItem(t, s, "t", "filler-4", 4, 0, "")

	paged, err := s.ListItemsBeforeTurn("t", 4, 1)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"anchor-0"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 0 {
		t.Errorf("floor: got %d, want 0", paged.OldestTurnIndex)
	}
	if paged.HasMore {
		t.Error("HasMore: got true, want false (floor at turn 0)")
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

func TestListItemsAfterTurn_ItemBudgetSemantics(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for turn := 0; turn < 6; turn++ {
		seedItem(t, s, "t", idForTurn(turn), turn, 0, "")
	}

	paged, err := s.ListItemsAfterTurn("t", 1, 2)
	if err != nil {
		t.Fatalf("items after turn: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{idForTurn(2), idForTurn(3)}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 2 {
		t.Errorf("oldest: got %d, want 2", paged.OldestTurnIndex)
	}
	if paged.NewestTurnIndex != 3 {
		t.Errorf("newest: got %d, want 3", paged.NewestTurnIndex)
	}
	if !paged.HasMore || !paged.HasMoreOlder {
		t.Error("expected older history below turn 2")
	}
	if !paged.HasMoreNewer {
		t.Error("expected newer history above turn 3")
	}
}

func TestListItemsAfterTurn_ChildOnlyTurnsAboveYieldEmptyPage(t *testing.T) {
	// The only rows above turn 5 are subagent children — they render
	// inside their anchor's group card, not as timeline rows, so the
	// forward pager reports a clean empty page instead of fabricating a
	// cursor from rows the frontend would drop.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedAnchorItem(t, s, "t", "parent", 0, 0)
	seedItem(t, s, "t", "omitted", 4, 0, "")
	seedItem(t, s, "t", "child", 10, 0, "parent")

	paged, err := s.ListItemsAfterTurn("t", 5, 1)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}

	if len(paged.Items) != 0 {
		t.Errorf("items: got %v, want empty page", collectIDs(paged.Items))
	}
	if paged.OldestTurnIndex != -1 || paged.NewestTurnIndex != -1 {
		t.Errorf("turn aliases = (%d, %d), want (-1, -1)", paged.OldestTurnIndex, paged.NewestTurnIndex)
	}
	if paged.HasMoreNewer {
		t.Error("HasMoreNewer: got true, want false (no top-level rows above)")
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

// TestListItemsBeforeTurn_ItemBudgetWalksUntilCumulative verifies that
// the item-budget semantic walks turns DESC accumulating item counts
// until cumulative ≥ budget. With turns 0..3 each holding 5 items
// (20 total below turn 4), a budget of 7 walks turn 3 (cumulative=5
// < 7) then turn 2 (cumulative=10 ≥ 7, stop). Floor=2. Returns 10
// items (turns 2 and 3). Turns 0 and 1 stay below; HasMore=true.
func TestListItemsBeforeTurn_ItemBudgetWalksUntilCumulative(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for turn := 0; turn < 4; turn++ {
		for i := 0; i < 5; i++ {
			seedItem(t, s, "t", idForItem(turn, i), turn, i, "")
		}
	}

	paged, err := s.ListItemsBeforeTurn("t", 4, 7)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	if len(paged.Items) != 10 {
		t.Errorf("items count: got %d, want 10 (turns 2 and 3)", len(paged.Items))
	}
	if paged.OldestTurnIndex != 2 {
		t.Errorf("oldest: got %d, want 2", paged.OldestTurnIndex)
	}
	if !paged.HasMore {
		t.Error("HasMore: got false, want true (turns 0 and 1 still below)")
	}
}

// TestListItemsBeforeTurn_ItemBudgetIgnoresPlanUpdateNotifications
// guards the kind filter on the item-budget walker. A `plan_update`
// notification should NOT count toward the cumulative budget — the
// loader filters them out, so counting them would systematically
// under-deliver visible content.
func TestListItemsBeforeTurn_ItemBudgetIgnoresPlanUpdateNotifications(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Turn 0: 1 real item + 10 plan_update notifications (excluded).
	// Turn 1: 1 real item. Budget=2 should reach turn 0 (cumulative=2
	// counting only the real items).
	seedItem(t, s, "t", "t1-real", 1, 0, "")
	seedItem(t, s, "t", "t0-real", 0, 0, "")
	for i := 0; i < 10; i++ {
		if err := s.InsertItem(Item{
			ID:        fmt.Sprintf("t0-plan-%d", i),
			ThreadID:  "t",
			TurnIndex: 0,
			ItemIndex: i + 1,
			Kind:      "notification",
			Role:      "system",
			ToolName:  "plan_update",
			Summary:   "plan",
			CreatedAt: int64(i),
		}); err != nil {
			t.Fatalf("insert plan_update %d: %v", i, err)
		}
	}

	paged, err := s.ListItemsBeforeTurn("t", 2, 2)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	if paged.OldestTurnIndex != 0 {
		t.Errorf("oldest: got %d, want 0 (plan_update must not count toward budget)", paged.OldestTurnIndex)
	}
	// Returned items: turn 0 real + turn 1 real (plan_updates filtered out by queryPagedItems).
	gotIDs := collectIDs(paged.Items)
	wantIDs := []string{"t0-real", "t1-real"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v (plan_update notifications filtered)", gotIDs, wantIDs)
	}
}

// TestHasOlderTurns_IgnoresPlanUpdateOnlySubFloor guards the false-
// positive Load-older button case. If the only items below the floor
// are `plan_update` notifications, `hasOlderTurns` must return false —
// the loaders filter those out, so clicking "Load older" would return
// zero rows. Before the kind-filter fix, the EXISTS probe didn't
// match the loader's WHERE clause and the button would render anyway.
func TestHasOlderTurns_IgnoresPlanUpdateOnlySubFloor(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Turn 0: only plan_update notifications. Turn 1: real items
	// (the "loaded window"). hasOlderTurns(floor=1) must be false:
	// nothing visible exists below.
	for i := 0; i < 5; i++ {
		if err := s.InsertItem(Item{
			ID:        fmt.Sprintf("t0-plan-%d", i),
			ThreadID:  "t",
			TurnIndex: 0,
			ItemIndex: i,
			Kind:      "notification",
			Role:      "system",
			ToolName:  "plan_update",
			Summary:   "plan",
			CreatedAt: int64(i),
		}); err != nil {
			t.Fatalf("insert plan_update %d: %v", i, err)
		}
	}
	seedItem(t, s, "t", "t1-real", 1, 0, "")

	exists, err := s.hasOlderTurns("t", 1)
	if err != nil {
		t.Fatalf("hasOlderTurns: %v", err)
	}
	if exists {
		t.Error("hasOlderTurns: got true, want false (only plan_update rows below floor)")
	}
}

// TestHasOlderTurns_TrueForMixedSubFloor confirms the positive case
// still works after adding the kind filter: a real item below the
// floor must report hasMore=true.
func TestHasOlderTurns_TrueForMixedSubFloor(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Turn 0: one real item + a few plan_update notifications. Turn 1:
	// real items (the loaded window). hasOlderTurns(floor=1) must be
	// true: the one real item in turn 0 counts.
	seedItem(t, s, "t", "t0-real", 0, 0, "")
	if err := s.InsertItem(Item{
		ID:        "t0-plan",
		ThreadID:  "t",
		TurnIndex: 0,
		ItemIndex: 1,
		Kind:      "notification",
		Role:      "system",
		ToolName:  "plan_update",
		Summary:   "plan",
		CreatedAt: 1,
	}); err != nil {
		t.Fatalf("insert plan_update: %v", err)
	}
	seedItem(t, s, "t", "t1-real", 1, 0, "")

	exists, err := s.hasOlderTurns("t", 1)
	if err != nil {
		t.Fatalf("hasOlderTurns: %v", err)
	}
	if !exists {
		t.Error("hasOlderTurns: got false, want true (real item below floor must count)")
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
