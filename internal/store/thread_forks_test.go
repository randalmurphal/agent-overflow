package store

import (
	"testing"
)

// TestCloneThreadItemsExcludesRunningBackgroundRows pins Phase-4's
// fork-exclusion contract at the store level: the clone skips rows
// whose `IsBackground && status='running'` combination implicates the
// source thread's subprocess. Any other row — completed backgrounded,
// non-background running, non-tool_call text — copies normally.
//
// The parent thread is untouched; the filter is applied in the clone
// path alone.
func TestCloneThreadItemsExcludesRunningBackgroundRows(t *testing.T) {
	s := newTestStore(t)
	now := int64(1)
	for _, id := range []string{"t-fork-src", "t-fork-dst"} {
		if err := s.CreateThread(Thread{
			ID: id, ProjectID: defaultTestProjectID, Title: "T",
			Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
	}

	// Mixed seed on the source thread.
	items := []Item{
		{ID: "user-0", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "hi", CreatedAt: now, UpdatedAt: now},
		{ID: "asst-1", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "hello", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "bg-run", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, Summary: "Bash: sleep 60", ToolName: "Bash", CreatedAt: now, UpdatedAt: now},
		{ID: "bg-done", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "completed", IsBackground: true, Summary: "Bash: echo done", ToolName: "Bash", CreatedAt: now, UpdatedAt: now},
		{ID: "inline-run", ThreadID: "t-fork-src", TurnIndex: 1, ItemIndex: 4, Kind: "tool_call", Role: "assistant", Status: "running", Summary: "Read: /tmp/x", ToolName: "Read", CreatedAt: now, UpdatedAt: now},
	}
	for _, it := range items {
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}

	if err := s.CloneThreadItems("t-fork-src", "t-fork-dst"); err != nil {
		t.Fatalf("CloneThreadItems: %v", err)
	}

	// Destination should carry everything EXCEPT `bg-run`.
	dst, err := s.ListItems("t-fork-dst")
	if err != nil {
		t.Fatalf("ListItems(dst): %v", err)
	}
	if len(dst) != 4 {
		t.Fatalf("dst items = %d, want 4 (bg-run excluded)", len(dst))
	}
	for _, it := range dst {
		if it.Summary == "Bash: sleep 60" {
			t.Errorf("running backgrounded row leaked into clone: %+v", it)
		}
		if it.IsBackground && it.Status == "running" {
			t.Errorf("clone carries a running backgrounded row: id=%s summary=%q", it.ID, it.Summary)
		}
	}

	// Source thread is untouched: the running bg row is still there.
	src, err := s.ListItems("t-fork-src")
	if err != nil {
		t.Fatalf("ListItems(src): %v", err)
	}
	found := false
	for _, it := range src {
		if it.ID == "bg-run" {
			found = true
			if it.Status != "running" {
				t.Errorf("source bg-run status mutated: %q", it.Status)
			}
		}
	}
	if !found {
		t.Error("source bg-run row vanished (clone must not mutate source)")
	}
}
