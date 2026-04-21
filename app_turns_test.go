package main

import (
	"testing"

	"agent-overflow/internal/store"
)

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
