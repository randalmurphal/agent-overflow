package store

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// items.parent_id stores the parent tool_call id for nested child items;
// an empty string round-trips as empty.

func TestItemsParentIDColumnExists(t *testing.T) {
	s := newTestStore(t)

	cols, err := tableColumns(s.db, "items")
	if err != nil {
		t.Fatalf("tableColumns(items): %v", err)
	}
	if !cols["parent_id"] {
		t.Fatalf("items.parent_id column missing (columns=%v)", cols)
	}
}

func TestInsertItemPersistsParentID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()

	threadID := "thread-sub"
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID:            threadID,
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
		Kind:      "assistant_text",
		Role:            "assistant",
		Summary:         "subagent body",
		ParentID: "task_tool_42",
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
	if got.ParentID != "task_tool_42" {
		t.Errorf("ParentID: got %q, want %q", got.ParentID, "task_tool_42")
	}
}

func TestInsertItemEmptyParentIDRoundTrips(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()

	threadID := "thread-top"
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID:            threadID,
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
		Kind:      "assistant_text",
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
	if got.ParentID != "" {
		t.Errorf("ParentID: got %q, want empty", got.ParentID)
	}
}

func TestListItemsPreservesParentID(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()

	threadID := "thread-list"
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID:            threadID,
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
		Kind:      "assistant_text", Role: "assistant", Summary: "child result",
		ParentID: "parent", CreatedAt: now,
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
	if parent.ParentID != "" {
		t.Errorf("parent ParentID: got %q, want empty", parent.ParentID)
	}
	if child.ParentID != "parent" {
		t.Errorf("child ParentID: got %q, want %q", child.ParentID, "parent")
	}
}

// TestItemIndexUniqueConstraintBlocksDuplicate verifies v10's UNIQUE
// (thread_id, turn_index, item_index). A direct raw INSERT that would
// violate the invariant must fail rather than silently corrupt ordering.
func TestItemIndexUniqueConstraintBlocksDuplicate(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID: "t-dup", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// First insert is fine.
	if err := s.InsertItem(Item{
		ID: "i-a", ThreadID: "t-dup", TurnIndex: 0, ItemIndex: 0,
		Kind:      "assistant_text", Role: "assistant", CreatedAt: now,
	}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// A raw INSERT that tries to reuse (thread_id, turn_index, item_index)
	// must violate the unique index. We bypass InsertItem so we don't have
	// to race it; the index is the gate we care about. Column list matches
	// the v15 items schema (parent_id, completion_of, status, updated_at)
	// so the row is rejected by the UNIQUE index and NOT by a "no such
	// column" or CHECK-kind error unrelated to the invariant under test.
	_, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at, parent_id, completion_of)
		VALUES ('i-b', 't-dup', 0, 0, 'assistant_text', 'assistant', 'completed', '', ?, ?, '', '')`, now, now)
	if err == nil {
		t.Error("expected UNIQUE constraint violation for duplicate (thread_id, turn_index, item_index)")
	} else if !strings.Contains(err.Error(), "UNIQUE") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("expected UNIQUE constraint violation, got: %v", err)
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
		ProjectID: defaultTestProjectID, ID: "t-race", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
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
				Kind:      "assistant_text",
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
		ProjectID: defaultTestProjectID, ID: "t-ra", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	idxA, err := s.AppendItem(Item{
		ID: "a", ThreadID: "t-ra", TurnIndex: 0, Kind:      "assistant_text",
		Role: "assistant", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("append a: %v", err)
	}
	if idxA != 0 {
		t.Errorf("first append should return 0, got %d", idxA)
	}
	idxB, err := s.AppendItem(Item{
		ID: "b", ThreadID: "t-ra", TurnIndex: 0, Kind:      "assistant_text",
		Role: "assistant", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("append b: %v", err)
	}
	if idxB != 1 {
		t.Errorf("second append should return 1, got %d", idxB)
	}
}

// TestConcurrentAppendItemWithPayloadAssignsUniqueIndex mirrors the
// AppendItem test for the combined item+payload writer. The returned
// indices must be unique and every row must end up persisted.
func TestConcurrentAppendItemWithPayloadAssignsUniqueIndex(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID: "t-race-pl", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
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
			itemID := fmt.Sprintf("item-%d", n)
			payloadID := fmt.Sprintf("pay-%d", n)
			payload := Payload{
				ID: payloadID, Kind: "diff", Meta: "{}",
				Data: []byte("delta"), CreatedAt: now,
			}
			_, err := s.AppendItemWithPayload(Item{
				ID:        itemID,
				ThreadID:  "t-race-pl",
				TurnIndex: 0,
				Kind:      "tool_call",
				Role:      "assistant",
				Summary:   fmt.Sprintf("goroutine %d", n),
				PayloadID: payloadID,
				CreatedAt: now,
			}, payload)
			if err != nil {
				errs <- fmt.Errorf("append+payload: %w", err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("goroutine error: %v", err)
	}

	items, err := s.ListTurnItems("t-race-pl", 0)
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
	// Every item must have its paired payload persisted too.
	for _, it := range items {
		if it.PayloadID == "" {
			t.Errorf("item %s missing PayloadID", it.ID)
			continue
		}
		meta, err := s.GetPayloadMeta(it.PayloadID)
		if err != nil {
			t.Errorf("payload %s missing: %v", it.PayloadID, err)
		}
		if meta.Kind != "diff" {
			t.Errorf("payload %s kind = %q, want diff", it.PayloadID, meta.Kind)
		}
	}
}

// TestAppendItemWithPayloadReturnsAssignedIndex sanity-checks the
// returned index: first insert lands at 0, second at 1, etc.
func TestAppendItemWithPayloadReturnsAssignedIndex(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ProjectID: defaultTestProjectID, ID: "t-rap", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	idx, err := s.AppendItemWithPayload(Item{
		ID: "a", ThreadID: "t-rap", TurnIndex: 0, Kind:      "tool_call",
		Role: "assistant", PayloadID: "pa", CreatedAt: now,
	}, Payload{ID: "pa", Kind: "diff", Meta: "{}", Data: []byte("pa"), CreatedAt: now})
	if err != nil {
		t.Fatalf("append a: %v", err)
	}
	if idx != 0 {
		t.Errorf("first append index = %d, want 0", idx)
	}
	idx, err = s.AppendItemWithPayload(Item{
		ID: "b", ThreadID: "t-rap", TurnIndex: 0, Kind:      "tool_call",
		Role: "assistant", PayloadID: "pb", CreatedAt: now,
	}, Payload{ID: "pb", Kind: "diff", Meta: "{}", Data: []byte("pb"), CreatedAt: now})
	if err != nil {
		t.Fatalf("append b: %v", err)
	}
	if idx != 1 {
		t.Errorf("second append index = %d, want 1", idx)
	}
}
