package store

import (
	"testing"
	"time"
)

// TestFindToolCallItemByTaskIDResolvesIndexedMeta pins the v17 indexed
// lookup. Seeds a thread with several items — one carrying a matching
// task_id in meta, one carrying a different task_id, and a plain
// text item — then asserts the lookup returns exactly the right row
// without scanning the others.
func TestFindToolCallItemByTaskIDResolvesIndexedMeta(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID:            "t-ft",
		ProjectID:     defaultTestProjectID,
		Title:         "T",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Plain text item with no task_id — must not match.
	if err := s.InsertItem(Item{
		ID: "text-1", ThreadID: "t-ft", TurnIndex: 0, ItemIndex: 0,
		Kind: "assistant_text", Role: "assistant",
		Summary: "hello", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert text: %v", err)
	}

	// Tool call with a different task_id — must not match.
	if err := s.InsertItem(Item{
		ID: "tool-a", ThreadID: "t-ft", TurnIndex: 0, ItemIndex: 1,
		Kind: "tool_call", Role: "assistant", Summary: "other",
		Meta:      `{"task_id":"task-other"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert tool-a: %v", err)
	}

	// Tool call with the matching task_id — must match.
	if err := s.InsertItem(Item{
		ID: "tool-b", ThreadID: "t-ft", TurnIndex: 0, ItemIndex: 2,
		Kind: "tool_call", Role: "assistant", Summary: "match",
		Meta:      `{"task_id":"task-target","other":"value"}`,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert tool-b: %v", err)
	}

	item, ok, err := s.FindToolCallItemByTaskID("t-ft", "task-target")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !ok {
		t.Fatal("expected match, got none")
	}
	if item.ID != "tool-b" {
		t.Errorf("match id = %q, want tool-b", item.ID)
	}

	// Unknown task id must return (_, false, nil).
	_, ok, err = s.FindToolCallItemByTaskID("t-ft", "task-missing")
	if err != nil {
		t.Fatalf("find missing: %v", err)
	}
	if ok {
		t.Error("expected no match for unknown task id")
	}

	// Empty task id short-circuits without a DB call.
	_, ok, err = s.FindToolCallItemByTaskID("t-ft", "")
	if err != nil {
		t.Fatalf("find empty: %v", err)
	}
	if ok {
		t.Error("expected no match for empty task id")
	}

	// Different thread must not match even with the right task id —
	// the index is thread-scoped.
	if err := s.CreateThread(Thread{
		ID: "t-other", ProjectID: defaultTestProjectID, Title: "O",
		Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread other: %v", err)
	}
	_, ok, err = s.FindToolCallItemByTaskID("t-other", "task-target")
	if err != nil {
		t.Fatalf("find other thread: %v", err)
	}
	if ok {
		t.Error("expected no match on different thread")
	}
}
