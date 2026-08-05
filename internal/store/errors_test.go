package store

import "testing"

func TestStoreMethodsReturnErrorsAfterClose(t *testing.T) {
	s := newTestStore(t)

	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	now := nowMillis()
	thread := Thread{
		ID:            "closed-thread",
		Title:         "Closed",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		Model:         "test-model",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	item := Item{
		ID:        "closed-item",
		ThreadID:  "closed-thread",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "hello",
		CreatedAt: now,
	}
	payload := Payload{
		ID:        "closed-payload",
		Kind:      "diff",
		Meta:      "{}",
		Data:      []byte("payload"),
		CreatedAt: now,
	}

	if err := s.UpdateThread(thread); err == nil {
		t.Fatal("expected UpdateThread to fail after store close")
	}
	if err := s.DeleteThread(thread.ID); err == nil {
		t.Fatal("expected DeleteThread to fail after store close")
	}
	if err := s.ArchiveThread(thread.ID); err == nil {
		t.Fatal("expected ArchiveThread to fail after store close")
	}
	if _, err := s.UpdateSessionRef(thread.ID, "session-ref"); err == nil {
		t.Fatal("expected UpdateSessionRef to fail after store close")
	}
	if err := s.InsertPayload(payload); err == nil {
		t.Fatal("expected InsertPayload to fail after store close")
	}
	if _, err := s.GetPayloadMeta(payload.ID); err == nil {
		t.Fatal("expected GetPayloadMeta to fail after store close")
	}
	if _, err := s.GetPayloadData(payload.ID); err == nil {
		t.Fatal("expected GetPayloadData to fail after store close")
	}
	if err := s.InsertItem(item); err == nil {
		t.Fatal("expected InsertItem to fail after store close")
	}
	if _, err := s.ListItems(thread.ID); err == nil {
		t.Fatal("expected ListItems to fail after store close")
	}
	if _, err := s.LastTurnIndex(thread.ID); err == nil {
		t.Fatal("expected LastTurnIndex to fail after store close")
	}
	if _, err := s.ListThreads(); err == nil {
		t.Fatal("expected ListThreads to fail after store close")
	}
}
