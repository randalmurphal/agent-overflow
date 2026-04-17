package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// --- Gap 3: items.parent_tool_use_id column ---
//
// Migration v4 adds a `parent_tool_use_id` column to items.  The column
// stores the Claude CLI `parent_tool_use_id` value when a message is a
// subagent (Task-tool) child.  An empty string round-trips as empty.

func TestMigrationV4AddsParentToolUseIDColumn(t *testing.T) {
	s := newTestStore(t)

	cols, err := tableColumns(s.db, "items")
	if err != nil {
		t.Fatalf("tableColumns(items): %v", err)
	}
	if !cols["parent_tool_use_id"] {
		t.Fatalf("items.parent_tool_use_id column missing (columns=%v)", cols)
	}

	rows, err := s.db.Query("SELECT version, name FROM migration_versions ORDER BY version")
	if err != nil {
		t.Fatalf("query migrations: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var v int
		var name string
		if err := rows.Scan(&v, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v == 4 && name == "subagent_correlation" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if !found {
		t.Error("migration_versions missing v4 subagent_correlation row")
	}
}

func TestInsertItemPersistsParentToolUseID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()

	threadID := "thread-sub"
	if err := s.CreateThread(Thread{
		ID:            threadID,
		Title:         "t",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	item := Item{
		ID:              "item-child",
		ThreadID:        threadID,
		TurnIndex:       0,
		ItemIndex:       0,
		Kind:            "text",
		Role:            "assistant",
		Summary:         "subagent body",
		ParentToolUseID: "task_tool_42",
		CreatedAt:       now,
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, found, err := s.GetItem("item-child")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatal("item not found after insert")
	}
	if got.ParentToolUseID != "task_tool_42" {
		t.Errorf("ParentToolUseID: got %q, want %q", got.ParentToolUseID, "task_tool_42")
	}
}

func TestInsertItemEmptyParentToolUseIDRoundTrips(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()

	threadID := "thread-top"
	if err := s.CreateThread(Thread{
		ID:            threadID,
		Title:         "t",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if err := s.InsertItem(Item{
		ID:        "item-top",
		ThreadID:  threadID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "assistant",
		Summary:   "top-level body",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, found, err := s.GetItem("item-top")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatal("not found")
	}
	if got.ParentToolUseID != "" {
		t.Errorf("ParentToolUseID: got %q, want empty", got.ParentToolUseID)
	}
}

func TestListItemsPreservesParentToolUseID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()

	threadID := "thread-list"
	if err := s.CreateThread(Thread{
		ID:            threadID,
		Title:         "t",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "parent", ThreadID: threadID, TurnIndex: 0, ItemIndex: 0,
		Kind: "tool_call", Role: "assistant", Summary: "Task",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "child-1", ThreadID: threadID, TurnIndex: 0, ItemIndex: 1,
		Kind: "text", Role: "assistant", Summary: "child result",
		ParentToolUseID: "parent", CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert child: %v", err)
	}

	items, err := s.ListItems(threadID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items: got %d, want 2", len(items))
	}
	var parent, child Item
	for _, it := range items {
		switch it.ID {
		case "parent":
			parent = it
		case "child-1":
			child = it
		}
	}
	if parent.ParentToolUseID != "" {
		t.Errorf("parent ParentToolUseID: got %q, want empty", parent.ParentToolUseID)
	}
	if child.ParentToolUseID != "parent" {
		t.Errorf("child ParentToolUseID: got %q, want %q", child.ParentToolUseID, "parent")
	}
}

// TestItemIndexUniqueConstraintBlocksDuplicate verifies v10's UNIQUE
// (thread_id, turn_index, item_index). A direct raw INSERT that would
// violate the invariant must fail rather than silently corrupt ordering.
func TestItemIndexUniqueConstraintBlocksDuplicate(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t-dup", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// First insert is fine.
	if err := s.InsertItem(Item{
		ID: "i-a", ThreadID: "t-dup", TurnIndex: 0, ItemIndex: 0,
		Kind: "text", Role: "assistant", CreatedAt: now,
	}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// A raw INSERT that tries to reuse (thread_id, turn_index, item_index)
	// must violate the unique index. We bypass InsertItem so we don't have
	// to race it; the index is the gate we care about.
	_, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, created_at, parent_tool_use_id)
		VALUES ('i-b', 't-dup', 0, 0, 'text', 'assistant', '', ?, '')`, now)
	if err == nil {
		t.Error("expected UNIQUE constraint violation for duplicate (thread_id, turn_index, item_index)")
	}
}

// TestConcurrentAppendItemAssignsUniqueIndex spawns many goroutines all
// calling AppendItem for the same (thread, turn). AppendItem computes the
// next item_index inside its transaction, so every item must land with a
// unique item_index — no crashes, no partial state, no duplicates.
func TestConcurrentAppendItemAssignsUniqueIndex(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t-race", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	const writers = 50
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := s.AppendItem(Item{
				ID:        fmt.Sprintf("item-%d", n),
				ThreadID:  "t-race",
				TurnIndex: 0,
				Kind:      "text",
				Role:      "assistant",
				Summary:   fmt.Sprintf("goroutine %d", n),
				CreatedAt: now,
			}); err != nil {
				errs <- fmt.Errorf("append: %w", err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}

	// Every item must be present with a unique item_index.
	items, err := s.ListTurnItems("t-race", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != writers {
		t.Fatalf("expected %d items, got %d", writers, len(items))
	}
	seen := make(map[int]bool, writers)
	for _, it := range items {
		if seen[it.ItemIndex] {
			t.Errorf("duplicate item_index %d", it.ItemIndex)
		}
		seen[it.ItemIndex] = true
	}
	if len(seen) != writers {
		t.Errorf("expected %d unique item_index values, got %d", writers, len(seen))
	}
}

// TestAppendItemReturnsAssignedIndex confirms the returned index matches
// what landed in the row so callers can forward it to downstream code
// (e.g. emitting an event with the persisted index).
func TestAppendItemReturnsAssignedIndex(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t-ra", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	idxA, err := s.AppendItem(Item{
		ID: "a", ThreadID: "t-ra", TurnIndex: 0, Kind: "text",
		Role: "assistant", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("append a: %v", err)
	}
	if idxA != 0 {
		t.Errorf("first append should return 0, got %d", idxA)
	}
	idxB, err := s.AppendItem(Item{
		ID: "b", ThreadID: "t-ra", TurnIndex: 0, Kind: "text",
		Role: "assistant", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("append b: %v", err)
	}
	if idxB != 1 {
		t.Errorf("second append should return 1, got %d", idxB)
	}
}
