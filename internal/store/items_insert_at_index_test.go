package store

import (
	"strings"
	"testing"
	"time"
)

func createInsertAtIndexThread(t *testing.T, s *Store, threadID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := s.CreateThread(Thread{
		ProjectID:     defaultTestProjectID,
		ID:            threadID,
		Title:         "thread",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
}

func appendInsertAtIndexItem(t *testing.T, s *Store, threadID string, id string, summary string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := s.AppendItem(Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: 0,
		Kind:      "assistant_text",
		Role:      "assistant",
		Status:    "completed",
		Summary:   summary,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

func TestInsertItemAtIndexShiftsRowsAndReturnsAffectedRows(t *testing.T) {
	s := newTestStore(t)
	createInsertAtIndexThread(t, s, "t-insert-at-index")
	appendInsertAtIndexItem(t, s, "t-insert-at-index", "a", "first")
	appendInsertAtIndexItem(t, s, "t-insert-at-index", "b", "second")
	appendInsertAtIndexItem(t, s, "t-insert-at-index", "c", "third")

	now := time.Now().UnixMilli()
	affected, err := s.InsertItemAtIndex(Item{
		ID:        "user:0:flush:1",
		ThreadID:  "t-insert-at-index",
		TurnIndex: 0,
		ItemIndex: 1,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "inserted",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("InsertItemAtIndex: %v", err)
	}

	gotIDs := make([]string, 0, len(affected))
	gotIndexes := make([]int, 0, len(affected))
	for _, item := range affected {
		gotIDs = append(gotIDs, item.ID)
		gotIndexes = append(gotIndexes, item.ItemIndex)
	}
	wantIDs := []string{"user:0:flush:1", "b", "c"}
	wantIndexes := []int{1, 2, 3}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] || gotIndexes[i] != wantIndexes[i] {
			t.Fatalf("affected[%d] = (%s,%d), want (%s,%d); all affected=%+v", i, gotIDs[i], gotIndexes[i], wantIDs[i], wantIndexes[i], affected)
		}
	}

	items, err := s.ListItemsForTurn("t-insert-at-index", 0)
	if err != nil {
		t.Fatalf("ListItemsForTurn: %v", err)
	}
	allIDs := make([]string, 0, len(items))
	allIndexes := make([]int, 0, len(items))
	for _, item := range items {
		allIDs = append(allIDs, item.ID)
		allIndexes = append(allIndexes, item.ItemIndex)
	}
	wantAllIDs := []string{"a", "user:0:flush:1", "b", "c"}
	wantAllIndexes := []int{0, 1, 2, 3}
	for i := range wantAllIDs {
		if allIDs[i] != wantAllIDs[i] || allIndexes[i] != wantAllIndexes[i] {
			t.Fatalf("items[%d] = (%s,%d), want (%s,%d); all items=%+v", i, allIDs[i], allIndexes[i], wantAllIDs[i], wantAllIndexes[i], items)
		}
	}
}

func TestInsertItemAtIndexRejectsNegativeIndex(t *testing.T) {
	s := newTestStore(t)
	createInsertAtIndexThread(t, s, "t-negative-index")

	_, err := s.InsertItemAtIndex(Item{
		ID:        "bad",
		ThreadID:  "t-negative-index",
		TurnIndex: 0,
		ItemIndex: -1,
		Kind:      "user_text",
		Role:      "user",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "negative item_index") {
		t.Fatalf("InsertItemAtIndex negative index err = %v, want negative item_index", err)
	}
}

func TestInsertItemAtIndexPersistsPayload(t *testing.T) {
	s := newTestStore(t)
	createInsertAtIndexThread(t, s, "t-insert-payload")
	appendInsertAtIndexItem(t, s, "t-insert-payload", "a", "first")

	now := time.Now().UnixMilli()
	affected, err := s.InsertItemAtIndex(Item{
		ID:        "with-payload",
		ThreadID:  "t-insert-payload",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "inserted",
		CreatedAt: now,
		UpdatedAt: now,
	}, &Payload{
		ID:        "payload-insert",
		Kind:      "tool_call_result",
		Meta:      `{"ok":true}`,
		Data:      []byte("payload"),
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("InsertItemAtIndex payload: %v", err)
	}
	if len(affected) != 2 {
		t.Fatalf("affected rows = %d, want inserted plus shifted row: %+v", len(affected), affected)
	}
	if affected[0].ID != "with-payload" || affected[0].PayloadID != "payload-insert" || affected[0].PayloadKind != "tool_call_result" || affected[0].PayloadMeta != `{"ok":true}` {
		t.Fatalf("inserted payload fields = %+v", affected[0])
	}
}
