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
