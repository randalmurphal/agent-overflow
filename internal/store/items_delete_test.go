package store

import (
	"testing"
	"time"
)

func TestDeleteItemsAfterTurnRemovesOnlyForwardTurns(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t-del", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Five turns, two items each — total 10 items.
	for turn := 0; turn < 5; turn++ {
		for i := 0; i < 2; i++ {
			if _, err := s.AppendItem(Item{
				ID:        idFor(turn, i),
				ThreadID:  "t-del",
				TurnIndex: turn,
				Kind:      "text",
				Role:      "assistant",
				CreatedAt: now,
			}); err != nil {
				t.Fatalf("append t%d i%d: %v", turn, i, err)
			}
		}
	}

	// Keep through turn 2 → drop turns 3 and 4 (4 items).
	deleted, err := s.DeleteItemsAfterTurn("t-del", 2)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 4 {
		t.Errorf("deleted count: got %d, want 4", deleted)
	}

	items, err := s.ListItems("t-del")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 6 {
		t.Fatalf("remaining items: got %d, want 6", len(items))
	}
	for _, it := range items {
		if it.TurnIndex > 2 {
			t.Errorf("item %s has turn_index %d, should have been deleted", it.ID, it.TurnIndex)
		}
	}
}

func TestDeleteItemsAfterTurnScopesToThread(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	for _, id := range []string{"t-a", "t-b"} {
		if err := s.CreateThread(Thread{
			ID: id, Title: id, Provider: "claude", WorkspacePath: "/tmp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create thread %s: %v", id, err)
		}
		for turn := 0; turn < 3; turn++ {
			if _, err := s.AppendItem(Item{
				ID: id + "-" + idFor(turn, 0), ThreadID: id,
				TurnIndex: turn, Kind: "text", Role: "assistant", CreatedAt: now,
			}); err != nil {
				t.Fatalf("append %s t%d: %v", id, turn, err)
			}
		}
	}

	if _, err := s.DeleteItemsAfterTurn("t-a", 0); err != nil {
		t.Fatalf("delete: %v", err)
	}

	aItems, _ := s.ListItems("t-a")
	bItems, _ := s.ListItems("t-b")
	if len(aItems) != 1 {
		t.Errorf("t-a remaining: got %d, want 1", len(aItems))
	}
	if len(bItems) != 3 {
		t.Errorf("t-b remaining: got %d, want 3 (unaffected)", len(bItems))
	}
}

func TestDeleteItemsAfterTurnEmptyIsNoop(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ID: "t-empty", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	deleted, err := s.DeleteItemsAfterTurn("t-empty", 0)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted on empty thread: got %d, want 0", deleted)
	}
}

func TestDeleteItemsAfterTurnTouchesThreadUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli() - 10_000
	if err := s.CreateThread(Thread{
		ID: "t-touch", Title: "t", Provider: "claude", WorkspacePath: "/tmp",
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := s.AppendItem(Item{
		ID: "only", ThreadID: "t-touch", TurnIndex: 0, Kind: "text",
		Role: "assistant", CreatedAt: base,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	before, err := s.GetThread("t-touch")
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if _, err := s.DeleteItemsAfterTurn("t-touch", -1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := s.GetThread("t-touch")
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.UpdatedAt <= before.UpdatedAt {
		t.Errorf("updated_at not bumped: before=%d after=%d", before.UpdatedAt, after.UpdatedAt)
	}
}

func idFor(turn, item int) string {
	return "id-" + itoa(turn) + "-" + itoa(item)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}
