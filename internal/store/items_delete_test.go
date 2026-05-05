package store

import (
	"fmt"
	"testing"
	"time"
)

func TestDeleteConversationFromTurnRemovesSelectedAndForwardTurns(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-del", now)
	insertDeleteConversationRows(t, s, "t-del", 5, now)
	if err := s.UpsertTrackedFiles("t-del", 1, []string{"keep.txt"}); err != nil {
		t.Fatalf("track keep: %v", err)
	}
	if err := s.UpsertTrackedFiles("t-del", 3, []string{"drop.txt"}); err != nil {
		t.Fatalf("track drop: %v", err)
	}

	deleted, err := s.DeleteConversationFromTurn("t-del", 2)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 6 {
		t.Errorf("deleted count: got %d, want 6", deleted)
	}

	items, err := s.ListItems("t-del")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("remaining items: got %d, want 4", len(items))
	}
	for _, it := range items {
		if it.TurnIndex >= 2 {
			t.Errorf("item %s has turn_index %d, should have been deleted", it.ID, it.TurnIndex)
		}
	}
	assertTurnsRemaining(t, s, "t-del", []int{0, 1})
	tracked, err := s.ListTrackedFiles("t-del")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	if len(tracked) != 1 || tracked[0] != "keep.txt" {
		t.Fatalf("tracked files after delete = %v, want [keep.txt]", tracked)
	}
}

func TestDeleteConversationFromTurnScopesToThread(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	for _, id := range []string{"t-a", "t-b"} {
		createDeleteConversationThread(t, s, id, now)
		insertDeleteConversationRows(t, s, id, 3, now)
	}

	if _, err := s.DeleteConversationFromTurn("t-a", 1); err != nil {
		t.Fatalf("delete: %v", err)
	}

	aItems, err := s.ListItems("t-a")
	if err != nil {
		t.Fatalf("list a: %v", err)
	}
	bItems, err := s.ListItems("t-b")
	if err != nil {
		t.Fatalf("list b: %v", err)
	}
	if len(aItems) != 2 {
		t.Errorf("t-a remaining items: got %d, want 2", len(aItems))
	}
	if len(bItems) != 6 {
		t.Errorf("t-b remaining items: got %d, want 6", len(bItems))
	}
	assertTurnsRemaining(t, s, "t-a", []int{0})
	assertTurnsRemaining(t, s, "t-b", []int{0, 1, 2})
}

func TestDeleteConversationFromTurnEmptyIsNoop(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	createDeleteConversationThread(t, s, "t-empty", now)

	deleted, err := s.DeleteConversationFromTurn("t-empty", 0)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted on empty thread: got %d, want 0", deleted)
	}
}

func TestDeleteConversationFromTurnTouchesThreadUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli() - 10_000
	createDeleteConversationThread(t, s, "t-touch", base)
	insertDeleteConversationRows(t, s, "t-touch", 1, base)

	before, err := s.GetThread("t-touch")
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if _, err := s.DeleteConversationFromTurn("t-touch", 0); err != nil {
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

func createDeleteConversationThread(t *testing.T, s *Store, id string, now int64) {
	t.Helper()
	if err := s.CreateThread(Thread{
		ProjectID:     defaultTestProjectID,
		ID:            id,
		Title:         id,
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread %s: %v", id, err)
	}
}

func insertDeleteConversationRows(t *testing.T, s *Store, threadID string, turns int, now int64) {
	t.Helper()
	for turn := 0; turn < turns; turn++ {
		if err := s.InsertTurn(Turn{
			TurnID:    fmt.Sprintf("%s-turn-%d", threadID, turn),
			ThreadID:  threadID,
			TurnIndex: turn,
			StartedAt: now + int64(turn),
		}); err != nil {
			t.Fatalf("insert turn %s %d: %v", threadID, turn, err)
		}
		for i := 0; i < 2; i++ {
			if _, err := s.AppendItem(Item{
				ID:        fmt.Sprintf("%s-item-%d-%d", threadID, turn, i),
				ThreadID:  threadID,
				TurnIndex: turn,
				Kind:      "assistant_text",
				Role:      "assistant",
				CreatedAt: now,
			}); err != nil {
				t.Fatalf("append %s t%d i%d: %v", threadID, turn, i, err)
			}
		}
	}
}

func assertTurnsRemaining(t *testing.T, s *Store, threadID string, want []int) {
	t.Helper()
	rows, err := s.db.Query(`SELECT turn_index FROM turns WHERE thread_id = ? ORDER BY turn_index`, threadID)
	if err != nil {
		t.Fatalf("query turns: %v", err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var turn int
		if err := rows.Scan(&turn); err != nil {
			t.Fatalf("scan turn: %v", err)
		}
		got = append(got, turn)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("turns remaining: got %v, want %v", got, want)
	}
}
