package store

import (
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
