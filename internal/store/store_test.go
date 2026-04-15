package store

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func makeThread(id, provider string) Thread {
	now := time.Now().UnixMilli()
	return Thread{
		ID:            id,
		Title:         "Thread " + id,
		Provider:      provider,
		WorkspacePath: "/tmp/test",
		Model:         "test-model",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestNewCreatesTablesSuccessfully(t *testing.T) {
	s := newTestStore(t)

	// Verify tables exist by inserting and querying.
	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.ID != "t1" {
		t.Errorf("thread ID: got %q, want %q", got.ID, "t1")
	}
}

func TestCreateAndGetThreadRoundTrip(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:            "thread-abc",
		Title:         "My Thread",
		Provider:      "codex",
		SessionRef:    "session-xyz",
		WorkspacePath: "/home/user/project",
		Model:         "gpt-4.1",
		CreatedAt:     now,
		UpdatedAt:     now,
		Archived:      false,
	}

	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetThread("thread-abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.ID != thr.ID {
		t.Errorf("ID: got %q, want %q", got.ID, thr.ID)
	}
	if got.Title != thr.Title {
		t.Errorf("Title: got %q, want %q", got.Title, thr.Title)
	}
	if got.Provider != thr.Provider {
		t.Errorf("Provider: got %q, want %q", got.Provider, thr.Provider)
	}
	if got.SessionRef != thr.SessionRef {
		t.Errorf("SessionRef: got %q, want %q", got.SessionRef, thr.SessionRef)
	}
	if got.WorkspacePath != thr.WorkspacePath {
		t.Errorf("WorkspacePath: got %q, want %q", got.WorkspacePath, thr.WorkspacePath)
	}
	if got.Model != thr.Model {
		t.Errorf("Model: got %q, want %q", got.Model, thr.Model)
	}
	if got.CreatedAt != thr.CreatedAt {
		t.Errorf("CreatedAt: got %d, want %d", got.CreatedAt, thr.CreatedAt)
	}
	if got.UpdatedAt != thr.UpdatedAt {
		t.Errorf("UpdatedAt: got %d, want %d", got.UpdatedAt, thr.UpdatedAt)
	}
	if got.Archived != thr.Archived {
		t.Errorf("Archived: got %v, want %v", got.Archived, thr.Archived)
	}
}

func TestListThreadsOrderedByUpdatedAtDesc(t *testing.T) {
	s := newTestStore(t)

	base := time.Now().UnixMilli()
	// Create threads with different updated_at values.
	for i, id := range []string{"old", "mid", "new"} {
		thr := makeThread(id, "claude")
		thr.UpdatedAt = base + int64(i)*1000
		if err := s.CreateThread(thr); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	threads, err := s.ListThreads()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(threads) != 3 {
		t.Fatalf("expected 3 threads, got %d", len(threads))
	}

	// Should be ordered newest first.
	if threads[0].ID != "new" {
		t.Errorf("first thread: got %q, want %q", threads[0].ID, "new")
	}
	if threads[1].ID != "mid" {
		t.Errorf("second thread: got %q, want %q", threads[1].ID, "mid")
	}
	if threads[2].ID != "old" {
		t.Errorf("third thread: got %q, want %q", threads[2].ID, "old")
	}
}

func TestListThreadsExcludesArchived(t *testing.T) {
	s := newTestStore(t)

	active := makeThread("active", "claude")
	if err := s.CreateThread(active); err != nil {
		t.Fatalf("create active: %v", err)
	}

	archived := makeThread("archived", "codex")
	archived.Archived = true
	if err := s.CreateThread(archived); err != nil {
		t.Fatalf("create archived: %v", err)
	}

	threads, err := s.ListThreads()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(threads))
	}
	if threads[0].ID != "active" {
		t.Errorf("expected active thread, got %q", threads[0].ID)
	}
}

func TestDeleteThread(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := s.GetThread("t1")
	if err == nil {
		t.Fatal("expected error getting deleted thread, got nil")
	}
}

func TestDeleteThreadCascadesToItems(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	item := Item{
		ID:        "item-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "user",
		Summary:   "hello",
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	items, err := s.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items after cascade delete, got %d", len(items))
	}
}

func TestArchiveThread(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	thr.UpdatedAt = 1000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.ArchiveThread("t1"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if !got.Archived {
		t.Error("expected thread to be archived")
	}
	if got.UpdatedAt <= 1000 {
		t.Errorf("expected updated_at to be bumped from 1000, got %d", got.UpdatedAt)
	}
}

func TestUpdateSessionRef(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	thr.UpdatedAt = 1000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.UpdateSessionRef("t1", "session-new"); err != nil {
		t.Fatalf("update session ref: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.SessionRef != "session-new" {
		t.Errorf("SessionRef: got %q, want %q", got.SessionRef, "session-new")
	}
	if got.UpdatedAt <= 1000 {
		t.Errorf("expected updated_at to be bumped from 1000, got %d", got.UpdatedAt)
	}
}

func TestGetThreadNonexistent(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetThread("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent thread, got nil")
	}
}

func TestCreateThreadDuplicateID(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("first create: %v", err)
	}

	err := s.CreateThread(thr)
	if err == nil {
		t.Fatal("expected error for duplicate ID, got nil")
	}
}

func TestProviderConstraint(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "invalid_provider")
	err := s.CreateThread(thr)
	if err == nil {
		t.Fatal("expected error for invalid provider, got nil")
	}
}

func TestSessionRefEmptyIsNull(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	thr.SessionRef = "" // should store as NULL
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// COALESCE in query converts NULL back to ""
	if got.SessionRef != "" {
		t.Errorf("SessionRef: got %q, want empty string", got.SessionRef)
	}
}

func TestUpdateThread(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	thr.Title = "Updated Title"
	thr.Model = "new-model"
	thr.UpdatedAt = time.Now().UnixMilli() + 5000
	if err := s.UpdateThread(thr); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Title != "Updated Title" {
		t.Errorf("Title: got %q, want %q", got.Title, "Updated Title")
	}
	if got.Model != "new-model" {
		t.Errorf("Model: got %q, want %q", got.Model, "new-model")
	}
}
