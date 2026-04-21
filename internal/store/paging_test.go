package store

import (
	"testing"
)

// Paging tests cover:
//   - ListRecentItemsWithAncestors pulls subagent parents that live below
//     the floor so groupItemsBySubagent can render them correctly.
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
	payloadID := "p-" + id
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

func TestListRecentItemsWithAncestors_NoAncestors(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Two turns, two items each, no parent relationships.
	seedItem(t, s, "t", "a", 0, 0, "")
	seedItem(t, s, "t", "b", 0, 1, "")
	seedItem(t, s, "t", "c", 1, 0, "")
	seedItem(t, s, "t", "d", 1, 1, "")

	paged, err := s.ListRecentItemsWithAncestors("t", 1)
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

func TestListRecentItemsWithAncestors_OneLevel(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Parent in turn 0 (outside window), subagent child in turn 2.
	seedItem(t, s, "t", "parent", 0, 0, "")
	seedItem(t, s, "t", "filler1", 1, 0, "")
	seedItem(t, s, "t", "child", 2, 0, "parent")

	paged, err := s.ListRecentItemsWithAncestors("t", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Parent MUST be present even though its turn_index (0) is below the
	// floor (2) — the recursive CTE is what keeps SubagentGroup intact
	// across paging boundaries.
	wantIDs := []string{"parent", "child"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	// The ancestor-included item reports the actual smallest turn_index
	// in the set — not the requested floor.
	if paged.OldestTurnIndex != 0 {
		t.Errorf("oldest: got %d, want 0", paged.OldestTurnIndex)
	}
	// HasMore probes for turn_index < OldestTurnIndex; nothing below 0 so
	// HasMore should be false.
	if paged.HasMore {
		t.Error("expected HasMore=false when parent turn already at 0")
	}
}

func TestListRecentItemsWithAncestors_MultiLevel(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Grandparent in turn 0, parent in turn 1, child in turn 5. Both
	// ancestors live below the requested floor of 4.
	seedItem(t, s, "t", "grand", 0, 0, "")
	seedItem(t, s, "t", "parent", 1, 0, "grand")
	seedItem(t, s, "t", "noise", 2, 0, "")
	seedItem(t, s, "t", "noise2", 3, 0, "")
	seedItem(t, s, "t", "child", 5, 0, "parent")

	paged, err := s.ListRecentItemsWithAncestors("t", 4)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Recursive CTE must pull both grand and parent. "noise" / "noise2"
	// live below the floor and have no ancestor relation, so they stay
	// excluded.
	wantIDs := []string{"grand", "parent", "child"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
}

func TestListItemsBeforeTurn_RespectsUpperBound(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Four turns, all with corresponding turns rows so
	// floorTurnIndexBefore uses the turns table path.
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

func TestListItemsBeforeTurn_IncludesAncestorsBelowNewFloor(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Thread has turns 0..4. The caller previously loaded turns 3-4
	// with a child whose parent is in turn 0. Asking for the next
	// batch "before turn 3 turnLimit=2" should return turns 1 and 2;
	// a new child in turn 2 pointing at the parent in turn 0 must
	// also pull the parent.
	for i := 0; i < 5; i++ {
		if err := s.InsertTurn(Turn{
			TurnID: idForTurn(i), ThreadID: "t", TurnIndex: i, StartedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
	}
	seedItem(t, s, "t", "old-parent", 0, 0, "")
	seedItem(t, s, "t", "filler1", 1, 0, "")
	seedItem(t, s, "t", "child-in-batch", 2, 0, "old-parent")
	seedItem(t, s, "t", "filler3", 3, 0, "")
	seedItem(t, s, "t", "filler4", 4, 0, "")

	paged, err := s.ListItemsBeforeTurn("t", 3, 2)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	gotIDs := collectIDs(paged.Items)
	// Must include old-parent (ancestor below new floor of 1), filler1,
	// child-in-batch — all ordered by (turn, item).
	wantIDs := []string{"old-parent", "filler1", "child-in-batch"}
	if !equalStringSlice(gotIDs, wantIDs) {
		t.Errorf("items: got %v, want %v", gotIDs, wantIDs)
	}
	if paged.OldestTurnIndex != 1 {
		t.Errorf("oldest: got %d, want 1", paged.OldestTurnIndex)
	}
	// The pager's oldest is the new floor (1), but HasMore probes for
	// items below that floor. Turn 0's parent is below, so HasMore=true.
	if !paged.HasMore {
		t.Error("HasMore should be true with old-parent turn 0 below new floor 1")
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
// test fails. Reuses the package-level `itoa` from items_delete_test.go.
func idForTurn(turn int) string {
	return "turn-" + itoa(turn)
}

func idForTurnItem(turn, item int) string {
	return "t" + itoa(turn) + "-i" + itoa(item)
}

func collectIDs(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestListRecentItemsWithAncestors_CrossThreadIsolation(t *testing.T) {
	// The items PK is `(thread_id, id)` not `id` alone, so the same
	// item id can exist on two threads. The recursive CTE in
	// `ancestorCTE` re-filters by thread_id at every step so an
	// ancestor walk on thread A never pulls rows from thread B even
	// when ids collide. Regression guard for that invariant.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("ta", "claude")); err != nil {
		t.Fatalf("create ta: %v", err)
	}
	if err := s.CreateThread(makeThread("tb", "claude")); err != nil {
		t.Fatalf("create tb: %v", err)
	}
	// Thread A: parent in turn 0, child in turn 5 pointing at parent.
	seedItem(t, s, "ta", "parent", 0, 0, "")
	seedItem(t, s, "ta", "child", 5, 0, "parent")
	// Thread B has an item with the SAME id as A's parent (id collision).
	// If the CTE's recursive step didn't filter by thread_id, the
	// ancestor walk for thread A would pick up B's "parent" row too and
	// render it as an orphan under A's timeline.
	seedItem(t, s, "tb", "parent", 10, 0, "")
	// Thread B also has a child pointing at a parent-id that only exists
	// on thread A — another cross-thread trap. The ancestor walk on B
	// should find NOTHING.
	seedItem(t, s, "tb", "b-child", 10, 1, "parent")

	pagedA, err := s.ListRecentItemsWithAncestors("ta", 5)
	if err != nil {
		t.Fatalf("list ta: %v", err)
	}
	if !equalStringSlice(collectIDs(pagedA.Items), []string{"parent", "child"}) {
		t.Errorf("ta: got %v, want [parent child]", collectIDs(pagedA.Items))
	}
	// Both returned rows must be from thread A — check thread_id
	// explicitly since the ids alone can't distinguish.
	for _, it := range pagedA.Items {
		if it.ThreadID != "ta" {
			t.Errorf("cross-thread leak: item %s came from %s, want ta", it.ID, it.ThreadID)
		}
	}

	// Thread B's ancestor walk hits a parent id that only exists on A.
	// Filtering must keep A's row out of B's result entirely.
	pagedB, err := s.ListRecentItemsWithAncestors("tb", 0)
	if err != nil {
		t.Fatalf("list tb: %v", err)
	}
	if !equalStringSlice(collectIDs(pagedB.Items), []string{"parent", "b-child"}) {
		t.Errorf("tb: got %v, want [parent b-child]", collectIDs(pagedB.Items))
	}
	for _, it := range pagedB.Items {
		if it.ThreadID != "tb" {
			t.Errorf("cross-thread leak: item %s came from %s, want tb", it.ID, it.ThreadID)
		}
	}
}

func TestListRecentItemsWithAncestors_TerminatesOnParentCycle(t *testing.T) {
	// A malformed thread where parent_id forms a cycle (item A ->
	// parent B; item B -> parent A). The recursive CTE uses UNION (not
	// UNION ALL) so duplicate ancestor ids are dedup'd per-iteration
	// and the recursion converges instead of spinning. Guard: if a
	// future refactor changes UNION → UNION ALL this test deadlocks,
	// surfacing the regression immediately.
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	seedItem(t, s, "t", "a", 0, 0, "b")
	seedItem(t, s, "t", "b", 1, 0, "a")
	seedItem(t, s, "t", "child", 5, 0, "a")

	paged, err := s.ListRecentItemsWithAncestors("t", 5)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := collectIDs(paged.Items)
	// Both a and b are ancestors (reachable via the cycle) so both load.
	// Order: grand/parent by turn_index then child.
	want := []string{"a", "b", "child"}
	if !equalStringSlice(got, want) {
		t.Errorf("items: got %v, want %v", got, want)
	}
}

func TestPickInitialFloorTurn_ActiveTurnBelowPicked(t *testing.T) {
	// Crash recovery scenario: an older turn was left with
	// `completed_at = NULL` after a process crash, and a later
	// (completed) turn sits above it with items. `GetActiveTurn`
	// returns the older, still-in-flight row; the floor must be
	// lowered to cover it AND `hasMore` must reflect the lowered
	// floor (items from an even-older completed turn below).
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// InsertTurn always writes completed_at=NULL; use
	// UpdateTurnCompleted to close the ones we want to be settled. The
	// one left NULL is the "crash-interrupted" row that activeTurnFloor
	// must surface.
	if err := s.InsertTurn(Turn{TurnID: "t0", ThreadID: "t", TurnIndex: 0, StartedAt: 0}); err != nil {
		t.Fatalf("insert turn 0: %v", err)
	}
	if err := s.UpdateTurnCompleted("t0", 100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("complete turn 0: %v", err)
	}
	seedItem(t, s, "t", "old-0", 0, 0, "")
	// Turn 1: in-flight (stays NULL). No items yet.
	if err := s.InsertTurn(Turn{TurnID: "t1", ThreadID: "t", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("insert turn 1 in-flight: %v", err)
	}
	// Turn 5: completed, with items. PickInitialFloorTurn walking
	// newest→oldest with turnLimit=1 would pick turn 5, but the
	// active-turn adjustment must lower picked to turn 1 so the
	// pre-fixed ordering bug is reproduced/caught.
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
	if floor != 1 {
		t.Errorf("floor: got %d, want 1 (active turn 1 must be covered)", floor)
	}
	// Turn 0 has items and sits below the lowered floor (1). hasMore
	// must be probed AFTER the active-turn adjustment so this is true.
	// Before the fix this was false (probe ran against the higher
	// picked, missing the items between the active-turn index and
	// the newer picked).
	if !hasMore {
		t.Error("hasMore: got false, want true (turn 0 has items below the lowered floor)")
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
