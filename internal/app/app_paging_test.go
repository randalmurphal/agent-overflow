package app

import (
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// Pin the binding-level contracts for the windowed-history wrappers.
// The underlying store methods have their own unit tests; these tests
// exercise the thin app-layer translation — turnLimit defaulting, nil
// normalization, constant wiring — so a typo in any of those would
// fail here instead of shipping silently.

func TestListRecentThreadItems_EmptyThreadReturnsStableShape(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-empty", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	paged, err := app.ListRecentThreadItems(thread.ID, 0)
	if err != nil {
		t.Fatalf("ListRecentThreadItems: %v", err)
	}
	// Never return nil Items — the frontend type is `Item[]`.
	if paged.Items == nil {
		t.Fatal("Items should be an empty slice, not nil")
	}
	if len(paged.Items) != 0 {
		t.Errorf("Items: got %d, want 0", len(paged.Items))
	}
	if paged.OldestTurnIndex != -1 {
		t.Errorf("OldestTurnIndex: got %d, want -1", paged.OldestTurnIndex)
	}
	if paged.HasMore {
		t.Error("HasMore: got true, want false for empty thread")
	}
}

func TestListRecentThreadItems_DefaultsTurnLimitWhenNonPositive(t *testing.T) {
	// turnLimit <= 0 must coerce to initialTurnWindow (50). A naive
	// change like `turnLimit < 0` would make turnLimit=0 pass through
	// to the store, where PickInitialFloorTurn would bump it to 1 —
	// silently shrinking the user's default window.
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-default", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	// Seed 60 turns so the difference between turnLimit=50 and
	// turnLimit=1 is visible: turnLimit=50 loads every turn (60
	// turns is within minWindowItems=500 so the whole thread is
	// loaded); turnLimit=1 would load just the tail turn.
	for i := 0; i < 60; i++ {
		if err := app.store.InsertTurn(store.Turn{
			TurnID:    "turn-" + itoa(i),
			ThreadID:  thread.ID,
			TurnIndex: i,
			StartedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
		if err := app.store.UpdateTurnCompleted("turn-"+itoa(i), int64(i)*1000+500, "end_turn", "", "", ""); err != nil {
			t.Fatalf("complete turn %d: %v", i, err)
		}
		if err := app.store.InsertItem(store.Item{
			ID:        "turn-" + itoa(i) + "-item",
			ThreadID:  thread.ID,
			TurnIndex: i,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Status:    "completed",
			Summary:   "x",
			CreatedAt: int64(i) * 1000,
			UpdatedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("upsert turn %d item: %v", i, err)
		}
	}

	paged, err := app.ListRecentThreadItems(thread.ID, 0)
	if err != nil {
		t.Fatalf("ListRecentThreadItems: %v", err)
	}
	if len(paged.Items) != 60 {
		t.Errorf("Items: got %d, want 60 (default turnLimit must be initialTurnWindow=50, "+
			"and minWindowItems=500 extends the window to cover all 60 single-item turns)",
			len(paged.Items))
	}
	if paged.HasMore {
		t.Error("HasMore: got true, want false (all turns loaded)")
	}
}

func TestListRecentThreadItems_RespectsExplicitTurnLimit(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-limit", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	// Two heavy turns, 300 items each. minWindowItems=500,
	// maxWindowItems=2000. With turnLimit=1, the walker picks turn
	// 1 first (300 items — under minItems), then turn 0 (total
	// 600 — still under max). Both get loaded.
	for turn := 0; turn < 2; turn++ {
		if err := app.store.InsertTurn(store.Turn{
			TurnID:    "t" + itoa(turn),
			ThreadID:  thread.ID,
			TurnIndex: turn,
			StartedAt: int64(turn) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", turn, err)
		}
		if err := app.store.UpdateTurnCompleted("t"+itoa(turn), int64(turn)*1000+500, "end_turn", "", "", ""); err != nil {
			t.Fatalf("complete turn %d: %v", turn, err)
		}
		for i := 0; i < 300; i++ {
			if err := app.store.InsertItem(store.Item{
				ID:        "t" + itoa(turn) + "-i" + itoa(i),
				ThreadID:  thread.ID,
				TurnIndex: turn,
				ItemIndex: i,
				Kind:      "user_text",
				Role:      "user",
				Status:    "completed",
				Summary:   "x",
				CreatedAt: int64(turn*1000 + i),
				UpdatedAt: int64(turn*1000 + i),
			}); err != nil {
				t.Fatalf("upsert t%d item %d: %v", turn, i, err)
			}
		}
	}

	paged, err := app.ListRecentThreadItems(thread.ID, 1)
	if err != nil {
		t.Fatalf("ListRecentThreadItems: %v", err)
	}
	// Walker picks turn 1 first (300 items). i+1=1 >= turnLimit=1
	// but cumulative=300 < minItems=500, so it keeps walking to
	// turn 0 (total 600, >= minItems, break). Both turns loaded.
	if len(paged.Items) != 600 {
		t.Errorf("Items: got %d, want 600 (turnLimit=1 but minWindowItems=500 extends window)",
			len(paged.Items))
	}
}

func TestListItemsBeforeTurn_DefaultsItemBudgetWhenNonPositive(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-before", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := app.store.InsertTurn(store.Turn{
			TurnID:    "t" + itoa(i),
			ThreadID:  thread.ID,
			TurnIndex: i,
			StartedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("insert turn %d: %v", i, err)
		}
		if err := app.store.UpdateTurnCompleted("t"+itoa(i), int64(i)*1000+500, "end_turn", "", "", ""); err != nil {
			t.Fatalf("complete turn %d: %v", i, err)
		}
		if err := app.store.InsertItem(store.Item{
			ID:        "t" + itoa(i) + "-item",
			ThreadID:  thread.ID,
			TurnIndex: i,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Status:    "completed",
			Summary:   "x",
			CreatedAt: int64(i) * 1000,
			UpdatedAt: int64(i) * 1000,
		}); err != nil {
			t.Fatalf("upsert turn %d: %v", i, err)
		}
	}

	// itemBudget=0 defaults to paginationItems (200). Asking for
	// everything before turn 5 must load all 5 older turns — the 5
	// items easily fit under the default budget.
	paged, err := app.ListItemsBeforeTurn(thread.ID, 5, 0)
	if err != nil {
		t.Fatalf("ListItemsBeforeTurn: %v", err)
	}
	if len(paged.Items) != 5 {
		t.Errorf("Items: got %d, want 5 (default itemBudget must accommodate 5 items)", len(paged.Items))
	}
	if paged.Items == nil {
		t.Fatal("Items should be empty slice, not nil")
	}
}

// TestListItemsBeforeTurn_ItemBudgetSemantics verifies the app-level legacy
// turn pager is now item-coordinate bounded. With 3 turns of 4 items each
// below the floor, a budget of 5 returns exactly the five newest primary rows
// below turn 3, even though that splits turn 1. HasMore=true because older
// rows still exist below the returned cursor.
func TestListItemsBeforeTurn_ItemBudgetSemantics(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-budget", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	for turn := 0; turn < 3; turn++ {
		for i := 0; i < 4; i++ {
			if err := app.store.InsertItem(store.Item{
				ID:        "t" + itoa(turn) + "-i" + itoa(i),
				ThreadID:  thread.ID,
				TurnIndex: turn,
				ItemIndex: i,
				Kind:      "assistant_text",
				Role:      "assistant",
				Status:    "completed",
				Summary:   "x",
				CreatedAt: int64(turn*100 + i),
				UpdatedAt: int64(turn*100 + i),
			}); err != nil {
				t.Fatalf("insert t%d-i%d: %v", turn, i, err)
			}
		}
	}

	paged, err := app.ListItemsBeforeTurn(thread.ID, 3, 5)
	if err != nil {
		t.Fatalf("ListItemsBeforeTurn: %v", err)
	}
	if len(paged.Items) != 5 {
		t.Errorf("Items: got %d, want 5 item-budgeted rows", len(paged.Items))
	}
	if paged.OldestTurnIndex != 1 {
		t.Errorf("OldestTurnIndex: got %d, want 1", paged.OldestTurnIndex)
	}
	if paged.OldestCursor.ItemID != "t1-i3" || paged.NewestCursor.ItemID != "t2-i3" {
		t.Errorf("cursor items = (%q, %q), want t1-i3/t2-i3", paged.OldestCursor.ItemID, paged.NewestCursor.ItemID)
	}
	if !paged.HasMore {
		t.Error("HasMore: got false, want true (older rows still below cursor)")
	}
}

// TestListItemsBeforeTurn_ExcludesHeadHealedRowsOfFloorTurn pins the
// strictly-earlier-turn contract against head-healed prompts (round-11):
// self-heal inserts a turn's prompt at MIN(item_index)-1, so index 0 is
// not the start of a turn — the synthetic floor cursor must sit below
// every possible index or a negative-index row from beforeTurnIndex
// leaks into the "older turns" page and duplicates a row the caller
// already has.
func TestListItemsBeforeTurn_ExcludesHeadHealedRowsOfFloorTurn(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-headheal", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	rows := []store.Item{
		{ID: "t0-i0", TurnIndex: 0, ItemIndex: 0, Kind: "assistant_text", Role: "assistant"},
		// Head-healed prompt in the floor turn at a NEGATIVE index.
		{ID: "t1-healed", TurnIndex: 1, ItemIndex: -1, Kind: "user_text", Role: "user"},
		{ID: "t1-i0", TurnIndex: 1, ItemIndex: 0, Kind: "assistant_text", Role: "assistant"},
	}
	for i, row := range rows {
		row.ThreadID = thread.ID
		row.Status = "completed"
		row.Summary = "x"
		row.CreatedAt = int64(i)
		row.UpdatedAt = int64(i)
		if err := app.store.InsertItem(row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}

	paged, err := app.ListItemsBeforeTurn(thread.ID, 1, 10)
	if err != nil {
		t.Fatalf("ListItemsBeforeTurn: %v", err)
	}
	if len(paged.Items) != 1 || paged.Items[0].ID != "t0-i0" {
		ids := make([]string, 0, len(paged.Items))
		for _, item := range paged.Items {
			ids = append(ids, item.ID)
		}
		t.Errorf("Items = %v, want exactly [t0-i0] — floor-turn rows must not leak into the older page", ids)
	}
}

func TestListThreadProposedPlans_NilNormalization(t *testing.T) {
	// Empty thread must return []Item{}, not nil. Otherwise the
	// frontend JSON deserializer sees `null` and the type-safe
	// wrapper errors on `.map(...)`.
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-plans", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	plans, err := app.ListThreadProposedPlans(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadProposedPlans: %v", err)
	}
	if plans == nil {
		t.Fatal("plans should be []Item{}, not nil")
	}
	if len(plans) != 0 {
		t.Errorf("len: got %d, want 0", len(plans))
	}
}

func TestListLiveBackgroundTasks_RetentionCutoffUsesWallClock(t *testing.T) {
	// The binding computes `cutoff = now - backgroundTaskRetentionMillis`
	// on each call. A completion whose created_at is within the
	// window surfaces; one outside doesn't. This test exercises the
	// cutoff by seeding two completion rows with different ages.
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-bg", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	// A background launch.
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "t0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 0,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID:           "launch",
		ThreadID:     thread.ID,
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		IsBackground: true,
		Summary:      "Bash",
		CreatedAt:    0,
		UpdatedAt:    0,
	}); err != nil {
		t.Fatalf("seed launch: %v", err)
	}
	now := time.Now().UnixMilli()
	// Recent completion — inside retention window (now - 500 ms).
	if err := app.store.InsertItem(store.Item{
		ID:           "completion-recent",
		ThreadID:     thread.ID,
		TurnIndex:    0,
		ItemIndex:    1,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		IsBackground: true,
		CompletionOf: "launch",
		Summary:      "Bash done",
		CreatedAt:    now - 500,
		UpdatedAt:    now - 500,
	}); err != nil {
		t.Fatalf("seed recent completion: %v", err)
	}
	// Old completion — outside retention window (now - 10s, cutoff is 2s).
	if err := app.store.InsertItem(store.Item{
		ID:           "completion-old",
		ThreadID:     thread.ID,
		TurnIndex:    0,
		ItemIndex:    2,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		IsBackground: true,
		CompletionOf: "launch",
		Summary:      "Bash done",
		CreatedAt:    now - 10_000,
		UpdatedAt:    now - 10_000,
	}); err != nil {
		t.Fatalf("seed old completion: %v", err)
	}

	items, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks: %v", err)
	}

	// Must include launch + recent completion, not old completion.
	byID := map[string]bool{}
	for _, it := range items {
		byID[it.ID] = true
	}
	if !byID["completion-recent"] {
		t.Error("completion-recent missing (should be inside retention window)")
	}
	if byID["completion-old"] {
		t.Error("completion-old leaked through (should be outside retention window)")
	}
}

func TestListLiveBackgroundTasks_ProjectsActiveCodexSubagentAsRunning(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/w-codex-subagent", "gpt-5.3-codex", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "t0",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: 0,
	}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID:           "spawn-active",
		ThreadID:     thread.ID,
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		Summary:      "spawn_agent",
		IsBackground: true,
		ToolName:     "collab_agent",
		Meta:         `{"input":{"tool":"spawn_agent","receiverThreadIds":["child-1"],"agentsStates":{"child-1":{"status":"running"}}}}`,
		CreatedAt:    1000,
		UpdatedAt:    1000,
	}); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}

	tasks, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks: %v", err)
	}
	var projected *store.Item
	for i := range tasks {
		if tasks[i].ID == "spawn-active" {
			projected = &tasks[i]
			break
		}
	}
	if projected == nil {
		t.Fatalf("projected Codex subagent missing from live tasks: %+v", tasks)
	}
	if projected.Status != "running" {
		t.Fatalf("projected status = %q, want running", projected.Status)
	}

	stored, found, err := app.store.GetThreadItem(thread.ID, "spawn-active")
	if err != nil || !found {
		t.Fatalf("GetThreadItem: found=%v err=%v", found, err)
	}
	if stored.Status != "completed" {
		t.Fatalf("stored status = %q, want completed", stored.Status)
	}
}

func TestGetThreadItem_ReturnsZeroValueForMissingItem(t *testing.T) {
	// Binding contract: missing item returns `Item{}` (empty id),
	// not an error. Frontend distinguishes via `item.id !== ''`.
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-getitem", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	item, err := app.GetThreadItem(thread.ID, "does-not-exist")
	if err != nil {
		t.Fatalf("GetThreadItem: %v (should be nil for missing rows)", err)
	}
	if item.ID != "" {
		t.Errorf("ID: got %q, want \"\" (zero value for missing row)", item.ID)
	}
}

func itoa(i int) string {
	// Avoids pulling strconv into every test file's import list.
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = digits[i%10]
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

// TestListRecentTurnsBindingDelegatesToStore confirms the App binding
// returns the same rows the store layer does, in newest-first order.
// The binding is a one-liner pass-through, but the test protects against
// accidental filtering / transformation being introduced later.
func TestListRecentTurnsBindingDelegatesToStore(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-1")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Two settled turns + one in-flight row. The binding must surface
	// all three in turn_index DESC order.
	for _, turn := range []store.Turn{
		{TurnID: "t-0", ThreadID: "thread-1", TurnIndex: 0, StartedAt: 1},
		{TurnID: "t-1", ThreadID: "thread-1", TurnIndex: 1, StartedAt: 2},
		{TurnID: "t-2", ThreadID: "thread-1", TurnIndex: 2, StartedAt: 3},
	} {
		if err := app.store.InsertTurn(turn); err != nil {
			t.Fatalf("InsertTurn %s: %v", turn.TurnID, err)
		}
	}
	// Settle turn 0 and 1; leave 2 in-flight to mirror the crash scenario.
	if err := app.store.UpdateTurnCompleted("t-0", 10, "end_turn", "msg-0", `{"inputTokens":5}`, ""); err != nil {
		t.Fatalf("UpdateTurnCompleted t-0: %v", err)
	}
	if err := app.store.UpdateTurnCompleted("t-1", 20, "end_turn", "msg-1", `{"inputTokens":7}`, ""); err != nil {
		t.Fatalf("UpdateTurnCompleted t-1: %v", err)
	}

	got, err := app.ListRecentTurns("thread-1", 5)
	if err != nil {
		t.Fatalf("ListRecentTurns: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	wantOrder := []string{"t-2", "t-1", "t-0"}
	for i, want := range wantOrder {
		if got[i].TurnID != want {
			t.Errorf("got[%d].TurnID = %q, want %q", i, got[i].TurnID, want)
		}
	}
	// The in-flight row must surface with CompletedAt == nil so the
	// frontend can distinguish it from a settled row.
	if got[0].CompletedAt != nil {
		t.Errorf("got[0].CompletedAt = %v, want nil (in-flight)", *got[0].CompletedAt)
	}
	// Settled rows carry their payload unchanged.
	if got[1].TokenUsageJSON != `{"inputTokens":7}` {
		t.Errorf("got[1].TokenUsageJSON = %q, want %q", got[1].TokenUsageJSON, `{"inputTokens":7}`)
	}
	if got[1].AssistantMessageID != "msg-1" {
		t.Errorf("got[1].AssistantMessageID = %q, want msg-1", got[1].AssistantMessageID)
	}
}

// TestListRecentTurnsBindingRespectsLimit exercises the happy path the
// frontend calls on thread-switch (`ListRecentTurns(threadId, 2)`).
func TestListRecentTurnsBindingRespectsLimit(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.CreateThread(testThread("thread-1")); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	for i := 0; i < 5; i++ {
		turn := store.Turn{
			TurnID:    "t-" + string(rune('0'+i)),
			ThreadID:  "thread-1",
			TurnIndex: i,
			StartedAt: int64(i + 1),
		}
		if err := app.store.InsertTurn(turn); err != nil {
			t.Fatalf("InsertTurn %d: %v", i, err)
		}
	}

	got, err := app.ListRecentTurns("thread-1", 2)
	if err != nil {
		t.Fatalf("ListRecentTurns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows with limit=2, got %d", len(got))
	}
	// Newest first — turn_index = 4 then 3.
	if got[0].TurnIndex != 4 {
		t.Errorf("got[0].TurnIndex = %d, want 4", got[0].TurnIndex)
	}
	if got[1].TurnIndex != 3 {
		t.Errorf("got[1].TurnIndex = %d, want 3", got[1].TurnIndex)
	}
}

// TestListRecentTurnsBindingEmptyThread covers the zero-state path the
// frontend hits on a freshly-created thread that's never sent a turn.
func TestListRecentTurnsBindingEmptyThread(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.CreateThread(testThread("thread-1")); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	got, err := app.ListRecentTurns("thread-1", 2)
	if err != nil {
		t.Fatalf("ListRecentTurns: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 rows for empty thread, got %d", len(got))
	}
}

// TestListRecentTurnsBindingNonPositiveLimit mirrors the store's
// short-circuit: a non-positive limit returns an empty slice without
// hitting the DB. The frontend doesn't call this path, but the contract
// is part of the store API the binding exposes.
func TestListRecentTurnsBindingNonPositiveLimit(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.CreateThread(testThread("thread-1")); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	turn := store.Turn{TurnID: "t-0", ThreadID: "thread-1", TurnIndex: 0, StartedAt: 1}
	if err := app.store.InsertTurn(turn); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	got, err := app.ListRecentTurns("thread-1", 0)
	if err != nil {
		t.Fatalf("ListRecentTurns(limit=0): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 rows for limit=0, got %d", len(got))
	}
}
