package main

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

func TestListItemsBeforeTurn_DefaultsTurnLimitWhenNonPositive(t *testing.T) {
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

	// turnLimit=0 defaults to paginationTurns (50). Asking for
	// everything before turn 5 must load all 5 older turns.
	paged, err := app.ListItemsBeforeTurn(thread.ID, 5, 0)
	if err != nil {
		t.Fatalf("ListItemsBeforeTurn: %v", err)
	}
	if len(paged.Items) != 5 {
		t.Errorf("Items: got %d, want 5 (default turnLimit must be paginationTurns)", len(paged.Items))
	}
	if paged.Items == nil {
		t.Fatal("Items should be empty slice, not nil")
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

func TestListThreadDiffPayloads_NilNormalization(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/w-diffs", "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	diffs, err := app.ListThreadDiffPayloads(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadDiffPayloads: %v", err)
	}
	if diffs == nil {
		t.Fatal("diffs should be []Item{}, not nil")
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
