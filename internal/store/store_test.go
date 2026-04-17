package store

import (
	"fmt"
	"sync"
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

func TestUnarchiveThreadRestoresRow(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t-un", "claude")
	thr.UpdatedAt = 1000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.ArchiveThread("t-un"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if err := s.UnarchiveThread("t-un"); err != nil {
		t.Fatalf("unarchive: %v", err)
	}

	got, err := s.GetThread("t-un")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Archived {
		t.Fatal("expected thread to be unarchived")
	}
	if got.UpdatedAt <= 1000 {
		t.Errorf("expected updated_at to be bumped from 1000, got %d", got.UpdatedAt)
	}
}

func TestUnarchiveThreadRestoresSidebarVisibility(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t-vis", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.ArchiveThread("t-vis"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Confirm ListThreads (sidebar's default view) hides the archived row.
	threads, err := s.ListThreads()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if containsThread(threads, "t-vis") {
		t.Fatal("expected archived thread to be hidden from ListThreads")
	}

	if err := s.UnarchiveThread("t-vis"); err != nil {
		t.Fatalf("unarchive: %v", err)
	}

	threads, err = s.ListThreads()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !containsThread(threads, "t-vis") {
		t.Fatal("expected unarchived thread to reappear in ListThreads")
	}
}

func TestUnarchiveUnknownThreadErrors(t *testing.T) {
	s := newTestStore(t)

	if err := s.UnarchiveThread("missing"); err == nil {
		t.Fatal("expected error for unknown thread id, got nil")
	}
}

func containsThread(threads []Thread, id string) bool {
	for _, t := range threads {
		if t.ID == id {
			return true
		}
	}
	return false
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

func TestUpdateTitle(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	thr.UpdatedAt = 1000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.UpdateTitle("t1", "Renamed Thread"); err != nil {
		t.Fatalf("update title: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Renamed Thread" {
		t.Fatalf("expected updated title, got %q", got.Title)
	}
	if got.UpdatedAt <= 1000 {
		t.Fatalf("expected updated_at to be bumped from 1000, got %d", got.UpdatedAt)
	}
}

func TestUpdateTitleIfCurrent(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	thr.Title = "New Thread"
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := s.UpdateTitleIfCurrent("t1", "New Thread", "Generated title")
	if err != nil {
		t.Fatalf("compare-and-swap title: %v", err)
	}
	if !updated {
		t.Fatal("expected title compare-and-swap to update")
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Generated title" {
		t.Fatalf("expected updated title, got %q", got.Title)
	}

	updated, err = s.UpdateTitleIfCurrent("t1", "New Thread", "Should not apply")
	if err != nil {
		t.Fatalf("compare-and-swap stale title: %v", err)
	}
	if updated {
		t.Fatal("expected stale compare-and-swap to report no update")
	}
}

func TestUpdateModel(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "codex")
	thr.UpdatedAt = 1000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.UpdateModel("t1", "gpt-5.4"); err != nil {
		t.Fatalf("update model: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "gpt-5.4" {
		t.Fatalf("expected updated model, got %q", got.Model)
	}
	if got.UpdatedAt <= 1000 {
		t.Fatalf("expected updated_at to be bumped from 1000, got %d", got.UpdatedAt)
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

// -- Item tests --

func TestInsertAndListItems(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	now := time.Now().UnixMilli()
	items := []Item{
		{ID: "i1", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0, Kind: "text", Role: "user", Summary: "hello", CreatedAt: now},
		{ID: "i2", ThreadID: "t1", TurnIndex: 0, ItemIndex: 1, Kind: "text", Role: "assistant", Summary: "hi", CreatedAt: now + 1},
		{ID: "i3", ThreadID: "t1", TurnIndex: 1, ItemIndex: 0, Kind: "tool_call", Role: "assistant", Summary: "bash", CreatedAt: now + 2},
	}

	for _, it := range items {
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}

	got, err := s.ListItems("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 items, got %d", len(got))
	}

	// Verify ordering by turn_index, item_index.
	for i, want := range items {
		if got[i].ID != want.ID {
			t.Errorf("item[%d].ID: got %q, want %q", i, got[i].ID, want.ID)
		}
		if got[i].TurnIndex != want.TurnIndex {
			t.Errorf("item[%d].TurnIndex: got %d, want %d", i, got[i].TurnIndex, want.TurnIndex)
		}
		if got[i].ItemIndex != want.ItemIndex {
			t.Errorf("item[%d].ItemIndex: got %d, want %d", i, got[i].ItemIndex, want.ItemIndex)
		}
	}
}

func TestInsertItemWithPayloadFK(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Inserting item with a non-existent payload_id should fail (FK constraint).
	item := Item{
		ID:        "i1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		PayloadID: "nonexistent-payload",
		CreatedAt: time.Now().UnixMilli(),
	}
	err := s.InsertItem(item)
	if err == nil {
		t.Fatal("expected FK constraint error, got nil")
	}
}

func TestInsertItemWithValidPayloadFK(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	now := time.Now().UnixMilli()
	payload := Payload{
		ID:        "p1",
		Kind:      "diff",
		Meta:      `{"file":"main.go"}`,
		Data:      []byte("diff content"),
		CreatedAt: now,
	}
	if err := s.InsertPayload(payload); err != nil {
		t.Fatalf("insert payload: %v", err)
	}

	item := Item{
		ID:        "i1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		PayloadID: "p1",
		CreatedAt: now,
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("insert item with valid payload FK: %v", err)
	}
}

// TestInsertItemWithPayloadAtomicHappyPath verifies the combined
// InsertPayload + InsertItem path writes both rows successfully.
func TestInsertItemWithPayloadAtomicHappyPath(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	now := time.Now().UnixMilli()
	payload := Payload{
		ID:        "p1",
		Kind:      "diff",
		Meta:      `{"file":"main.go"}`,
		Data:      []byte("diff content"),
		CreatedAt: now,
	}
	item := Item{
		ID:        "i1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		PayloadID: "p1",
		CreatedAt: now,
	}
	if err := s.InsertItemWithPayload(item, payload); err != nil {
		t.Fatalf("InsertItemWithPayload: %v", err)
	}

	// Both rows must be reachable afterward.
	data, err := s.GetPayloadData("p1")
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != "diff content" {
		t.Fatalf("payload data = %q, want %q", data, "diff content")
	}
	items, err := s.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].PayloadID != "p1" {
		t.Fatalf("item payload_id = %q, want p1", items[0].PayloadID)
	}
}

// TestInsertItemWithPayloadAtomicRollbackOnItemFailure exercises Bug B10:
// when the item half of the transaction fails (duplicate ID, FK error,
// whatever), the payload half must roll back too so no orphan payload
// row remains.
func TestInsertItemWithPayloadAtomicRollbackOnItemFailure(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	now := time.Now().UnixMilli()
	// Pre-insert an item with ID "i-dup" so the combined call below
	// tries to INSERT a duplicate primary key and fails.
	if err := s.InsertItem(Item{
		ID:        "i-dup",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "user",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	payload := Payload{
		ID:        "p-orphan",
		Kind:      "diff",
		Meta:      "{}",
		Data:      []byte("data"),
		CreatedAt: now,
	}
	dupItem := Item{
		ID:        "i-dup",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 1,
		Kind:      "diff",
		Role:      "assistant",
		PayloadID: "p-orphan",
		CreatedAt: now,
	}
	err := s.InsertItemWithPayload(dupItem, payload)
	if err == nil {
		t.Fatal("expected duplicate-key error, got nil")
	}

	// Payload must NOT be present — the rollback undoes the pre-item
	// insert of p-orphan.
	if _, getErr := s.GetPayloadData("p-orphan"); getErr == nil {
		t.Fatal("payload p-orphan persisted despite item failure (Bug B10 regression)")
	}
}

// TestInsertItemWithPayloadAtomicRollbackOnPayloadFailure exercises the
// symmetric case: the payload half fails (duplicate ID), so the item
// must never land either.
func TestInsertItemWithPayloadAtomicRollbackOnPayloadFailure(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	now := time.Now().UnixMilli()
	// Pre-insert a payload with ID "p-dup" so the call below fails on
	// the payload half.
	if err := s.InsertPayload(Payload{
		ID:        "p-dup",
		Kind:      "diff",
		Meta:      "{}",
		Data:      []byte("seed"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed payload: %v", err)
	}

	dupPayload := Payload{
		ID:        "p-dup",
		Kind:      "diff",
		Meta:      "{}",
		Data:      []byte("new"),
		CreatedAt: now,
	}
	item := Item{
		ID:        "i-orphan",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		PayloadID: "p-dup",
		CreatedAt: now,
	}
	err := s.InsertItemWithPayload(item, dupPayload)
	if err == nil {
		t.Fatal("expected duplicate-key error, got nil")
	}

	items, err := s.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.ID == "i-orphan" {
			t.Fatalf("item i-orphan persisted despite payload failure: %+v", it)
		}
	}
}

func TestNextItemIndex(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Empty turn -> next index is 0.
	idx, err := s.NextItemIndex("t1", 0)
	if err != nil {
		t.Fatalf("next index (empty): %v", err)
	}
	if idx != 0 {
		t.Errorf("expected 0 for empty turn, got %d", idx)
	}

	// Insert items and verify increment.
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		item := Item{
			ID:        "i" + string(rune('0'+i)),
			ThreadID:  "t1",
			TurnIndex: 0,
			ItemIndex: i,
			Kind:      "text",
			Role:      "user",
			CreatedAt: now,
		}
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert item %d: %v", i, err)
		}
	}

	idx, err = s.NextItemIndex("t1", 0)
	if err != nil {
		t.Fatalf("next index (3 items): %v", err)
	}
	if idx != 3 {
		t.Errorf("expected 3 after 3 items, got %d", idx)
	}
}

func TestLastTurnIndex(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Empty thread -> 0.
	idx, err := s.LastTurnIndex("t1")
	if err != nil {
		t.Fatalf("last turn (empty): %v", err)
	}
	if idx != 0 {
		t.Errorf("expected 0 for empty thread, got %d", idx)
	}

	now := time.Now().UnixMilli()
	for _, turnIdx := range []int{0, 1, 5} {
		item := Item{
			ID:        "i-turn-" + string(rune('0'+turnIdx)),
			ThreadID:  "t1",
			TurnIndex: turnIdx,
			ItemIndex: 0,
			Kind:      "text",
			Role:      "user",
			CreatedAt: now,
		}
		if err := s.InsertItem(item); err != nil {
			t.Fatalf("insert turn %d: %v", turnIdx, err)
		}
	}

	idx, err = s.LastTurnIndex("t1")
	if err != nil {
		t.Fatalf("last turn: %v", err)
	}
	if idx != 5 {
		t.Errorf("expected 5, got %d", idx)
	}
}

func TestHasItems(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	hasItems, err := s.HasItems("t1")
	if err != nil {
		t.Fatalf("has items (empty): %v", err)
	}
	if hasItems {
		t.Fatal("expected empty thread to report no items")
	}

	if err := s.InsertItem(Item{
		ID:        "i1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "user",
		CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	hasItems, err = s.HasItems("t1")
	if err != nil {
		t.Fatalf("has items (non-empty): %v", err)
	}
	if !hasItems {
		t.Fatal("expected non-empty thread to report items")
	}
}

func TestInsertItemBumpsThreadUpdatedAt(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "claude")
	thr.UpdatedAt = 1000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	itemTime := int64(5000)
	item := Item{
		ID:        "i1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "text",
		Role:      "user",
		CreatedAt: itemTime,
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	got, err := s.GetThread("t1")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	if got.UpdatedAt != itemTime {
		t.Errorf("thread updated_at: got %d, want %d", got.UpdatedAt, itemTime)
	}
}

// -- Payload tests --

func TestInsertAndGetPayloadMeta(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UnixMilli()
	payload := Payload{
		ID:        "p1",
		Kind:      "diff",
		Meta:      `{"file":"main.go","insertions":5,"deletions":3}`,
		Data:      []byte("--- a/main.go\n+++ b/main.go\n"),
		CreatedAt: now,
	}
	if err := s.InsertPayload(payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	meta, err := s.GetPayloadMeta("p1")
	if err != nil {
		t.Fatalf("get meta: %v", err)
	}

	if meta.ID != "p1" {
		t.Errorf("ID: got %q, want %q", meta.ID, "p1")
	}
	if meta.Kind != "diff" {
		t.Errorf("Kind: got %q, want %q", meta.Kind, "diff")
	}
	if meta.Meta != payload.Meta {
		t.Errorf("Meta: got %q, want %q", meta.Meta, payload.Meta)
	}
	if meta.CreatedAt != now {
		t.Errorf("CreatedAt: got %d, want %d", meta.CreatedAt, now)
	}
}

func TestGetPayloadData(t *testing.T) {
	s := newTestStore(t)

	content := []byte("full diff content here\nline 2\nline 3")
	payload := Payload{
		ID:        "p1",
		Kind:      "command_output",
		Meta:      `{"lineCount":3}`,
		Data:      content,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.InsertPayload(payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	data, err := s.GetPayloadData("p1")
	if err != nil {
		t.Fatalf("get data: %v", err)
	}

	if string(data) != string(content) {
		t.Errorf("data: got %q, want %q", data, content)
	}
}

func TestUpsertTurnPayloadReplacesExistingPayload(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	original := Payload{
		ID:        "payload-1",
		Kind:      "diff",
		Meta:      `{"preview":"old"}`,
		Data:      []byte("old diff"),
		CreatedAt: 1000,
	}
	if err := s.InsertPayload(original); err != nil {
		t.Fatalf("insert original payload: %v", err)
	}

	item := Item{
		ID:        "item-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		Summary:   "old diff",
		PayloadID: original.ID,
		CreatedAt: 1000,
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	replacement := Payload{
		ID:        "payload-2",
		Kind:      "diff",
		Meta:      `{"preview":"new"}`,
		Data:      []byte("new diff"),
		CreatedAt: 2000,
	}
	if err := s.UpsertTurnPayload("t1", 0, "diff", replacement); err != nil {
		t.Fatalf("upsert turn payload: %v", err)
	}

	metas, err := s.ListPayloadMetas("t1")
	if err != nil {
		t.Fatalf("list payload metas: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 payload meta after replace, got %d", len(metas))
	}
	if metas[0].ID != original.ID {
		t.Fatalf("expected existing payload id %q to be reused, got %q", original.ID, metas[0].ID)
	}
	if metas[0].Meta != replacement.Meta {
		t.Fatalf("expected payload meta %q, got %q", replacement.Meta, metas[0].Meta)
	}

	data, err := s.GetPayloadData(original.ID)
	if err != nil {
		t.Fatalf("get payload data: %v", err)
	}
	if string(data) != "new diff" {
		t.Fatalf("expected updated payload data, got %q", string(data))
	}
}

// TestUpsertTurnPayloadLinksUnlinkedItem covers the orphan-payload bug where
// UpsertTurnPayload previously inserted a new payload row without updating
// the item's payload_id, leaving the payload unreachable from any item.
func TestUpsertTurnPayloadLinksUnlinkedItem(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Seed an item with payload_id = NULL — this is the case where the
	// router has inserted a summary-only item and then follows up with a
	// heavy payload via UpsertTurnPayload.
	unlinked := Item{
		ID:        "item-unlinked",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		Summary:   "summary only",
		CreatedAt: 1000,
	}
	if err := s.InsertItem(unlinked); err != nil {
		t.Fatalf("insert unlinked item: %v", err)
	}

	payload := Payload{
		ID:        "payload-fresh",
		Kind:      "diff",
		Meta:      `{"preview":"new"}`,
		Data:      []byte("new diff"),
		CreatedAt: 2000,
	}
	if err := s.UpsertTurnPayload("t1", 0, "diff", payload); err != nil {
		t.Fatalf("upsert turn payload: %v", err)
	}

	got, found, err := s.GetItem("item-unlinked")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !found {
		t.Fatal("item should still exist after upsert")
	}
	if got.PayloadID != payload.ID {
		t.Fatalf("item payload_id: got %q, want %q", got.PayloadID, payload.ID)
	}

	// Assert no orphan payloads — every payload must be reachable from some item.
	var orphans int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM payloads p
		 WHERE NOT EXISTS (SELECT 1 FROM items i WHERE i.payload_id = p.id)`,
	).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("expected 0 orphan payloads, got %d", orphans)
	}
}

// TestUpsertTurnPayloadReplaceDoesNotDuplicatePayload calls upsert twice with
// distinct data against the same item and confirms exactly one payload row
// survives. Guards against a regression where each call would insert a fresh
// row for the same (thread, turn, kind).
func TestUpsertTurnPayloadReplaceDoesNotDuplicatePayload(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	original := Payload{
		ID:        "payload-1",
		Kind:      "diff",
		Meta:      `{"preview":"v1"}`,
		Data:      []byte("v1"),
		CreatedAt: 1000,
	}
	if err := s.InsertPayload(original); err != nil {
		t.Fatalf("insert original: %v", err)
	}
	if err := s.InsertItem(Item{
		ID:        "item-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		Summary:   "v1",
		PayloadID: original.ID,
		CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	v2 := Payload{ID: "payload-new-a", Kind: "diff", Meta: `{"v":2}`, Data: []byte("v2"), CreatedAt: 2000}
	v3 := Payload{ID: "payload-new-b", Kind: "diff", Meta: `{"v":3}`, Data: []byte("v3"), CreatedAt: 3000}
	if err := s.UpsertTurnPayload("t1", 0, "diff", v2); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	if err := s.UpsertTurnPayload("t1", 0, "diff", v3); err != nil {
		t.Fatalf("upsert v3: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads`).Scan(&count); err != nil {
		t.Fatalf("count payloads: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 payload after two upserts, got %d", count)
	}

	data, err := s.GetPayloadData(original.ID)
	if err != nil {
		t.Fatalf("get data: %v", err)
	}
	if string(data) != "v3" {
		t.Fatalf("expected latest data 'v3', got %q", string(data))
	}
}

// TestUpsertTurnPayloadConcurrentWritesNoOrphans hammers UpsertTurnPayload
// from many goroutines against the same (thread, turn, kind) tuple. The
// store serialises via SetMaxOpenConns(1), so nothing should crash and no
// orphan payloads should remain.
func TestUpsertTurnPayloadConcurrentWritesNoOrphans(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Seed one unlinked item for the concurrent writers to race to link.
	if err := s.InsertItem(Item{
		ID:        "item-concurrent",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		Summary:   "summary",
		CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	var wg sync.WaitGroup
	writers := 32
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p := Payload{
				ID:        fmt.Sprintf("payload-%d", n),
				Kind:      "diff",
				Meta:      `{"n":0}`,
				Data:      []byte("data"),
				CreatedAt: int64(1000 + n),
			}
			if err := s.UpsertTurnPayload("t1", 0, "diff", p); err != nil {
				t.Errorf("goroutine %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	var orphans int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM payloads p
		 WHERE NOT EXISTS (SELECT 1 FROM items i WHERE i.payload_id = p.id)`,
	).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("concurrent writers left %d orphan payloads", orphans)
	}

	// Item must be linked to exactly one payload and exactly one payload must exist.
	var payloads int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads`).Scan(&payloads); err != nil {
		t.Fatalf("count payloads: %v", err)
	}
	if payloads != 1 {
		t.Fatalf("expected exactly 1 payload after concurrent writers, got %d", payloads)
	}

	item, ok, err := s.GetItem("item-concurrent")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok || item.PayloadID == "" {
		t.Fatalf("item should be linked; got payload_id=%q ok=%v", item.PayloadID, ok)
	}
}

func TestFindTurnItemReturnsFalseWhenMissing(t *testing.T) {
	s := newTestStore(t)

	item, found, err := s.FindTurnItem("missing-thread", 0, "diff")
	if err != nil {
		t.Fatalf("find turn item: %v", err)
	}
	if found {
		t.Fatal("expected missing turn item lookup to return found=false")
	}
	if item.ID != "" {
		t.Fatalf("expected zero item for missing lookup, got %+v", item)
	}
}

func TestUpdateItemPayloadUpdatesLinkedItem(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	payload := Payload{
		ID:        "payload-1",
		Kind:      "diff",
		Meta:      `{"preview":"before"}`,
		Data:      []byte("before"),
		CreatedAt: 1000,
	}
	if err := s.InsertPayload(payload); err != nil {
		t.Fatalf("insert payload: %v", err)
	}

	item := Item{
		ID:        "item-1",
		ThreadID:  "t1",
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "diff",
		Role:      "assistant",
		Summary:   "before",
		PayloadID: payload.ID,
		CreatedAt: 1000,
	}
	if err := s.InsertItem(item); err != nil {
		t.Fatalf("insert item: %v", err)
	}

	if err := s.UpdateItemPayload(item.ID, "payload-1", "after", 2000); err != nil {
		t.Fatalf("update item payload: %v", err)
	}

	updated, found, err := s.FindTurnItem("t1", 0, "diff")
	if err != nil {
		t.Fatalf("find turn item: %v", err)
	}
	if !found {
		t.Fatal("expected updated item to be found")
	}
	if updated.Summary != "after" {
		t.Fatalf("expected updated summary, got %q", updated.Summary)
	}
	if updated.CreatedAt != 2000 {
		t.Fatalf("expected updated created_at, got %d", updated.CreatedAt)
	}
}

// TestUpdateItemPayloadReturnsErrorForMissingItem covers the previously
// silent-failure branch: UpdateItemPayload used to log, not return, when
// the item-update's thread-touch step ran against a nonexistent item.
// Now both steps fail loudly so callers can't silently mismatch.
func TestUpdateItemPayloadReturnsErrorForMissingItem(t *testing.T) {
	s := newTestStore(t)

	err := s.UpdateItemPayload("not-a-real-item", "some-payload", "x", 100)
	if err == nil {
		t.Fatal("expected error for nonexistent item")
	}
}

// TestUpdateItemPayloadAtomicThreadTouch proves the thread's updated_at
// is revised in the SAME transaction as the item's payload link. Before
// A8, the thread touch ran outside any tx — a failure there was logged
// and swallowed. We verify commit-order by mutating createdAt and
// confirming both rows moved together.
func TestUpdateItemPayloadAtomicThreadTouch(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-atomic", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.InsertPayload(Payload{
		ID: "p-1", Kind: "diff", Meta: "{}", Data: []byte("x"), CreatedAt: 100,
	}); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i-1", ThreadID: "t-atomic", TurnIndex: 0, ItemIndex: 0,
		Kind: "diff", Role: "assistant", PayloadID: "p-1", CreatedAt: 100,
	}); err != nil {
		t.Fatalf("item: %v", err)
	}

	if err := s.UpdateItemPayload("i-1", "p-1", "updated", 999); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetThread("t-atomic")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.UpdatedAt != 999 {
		t.Errorf("thread updated_at: got %d, want 999", got.UpdatedAt)
	}
	item, ok, err := s.GetItem("i-1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatal("item missing after update")
	}
	if item.CreatedAt != 999 {
		t.Errorf("item created_at: got %d, want 999", item.CreatedAt)
	}
}

// TestUpdateItemPayloadConcurrentCallsSerialise drives many concurrent
// updates at the same item; with single-connection serialisation all
// calls must succeed and final state must be coherent (whichever
// created_at wrote last wins for both item and thread).
func TestUpdateItemPayloadConcurrentCallsSerialise(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t-conc", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.InsertPayload(Payload{
		ID: "p-1", Kind: "diff", Meta: "{}", Data: []byte("x"), CreatedAt: 100,
	}); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := s.InsertItem(Item{
		ID: "i-1", ThreadID: "t-conc", TurnIndex: 0, ItemIndex: 0,
		Kind: "diff", Role: "assistant", PayloadID: "p-1", CreatedAt: 100,
	}); err != nil {
		t.Fatalf("item: %v", err)
	}

	const writers = 20
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := s.UpdateItemPayload("i-1", "p-1",
				fmt.Sprintf("v%d", n), int64(1000+n)); err != nil {
				t.Errorf("update %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	// Thread.updated_at must match some call's createdAt, and item.created_at
	// must match a call's createdAt — both must agree (both within the same tx).
	thread, err := s.GetThread("t-conc")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	item, ok, err := s.GetItem("i-1")
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if !ok {
		t.Fatal("item missing")
	}
	// Both must be in the range of writes.
	if thread.UpdatedAt < 1000 || thread.UpdatedAt > int64(1000+writers-1) {
		t.Errorf("thread.UpdatedAt out of range: %d", thread.UpdatedAt)
	}
	if item.CreatedAt < 1000 || item.CreatedAt > int64(1000+writers-1) {
		t.Errorf("item.CreatedAt out of range: %d", item.CreatedAt)
	}
	// Because the fix made both updates atomic, the final item.created_at
	// and thread.updated_at must match exactly — they were committed from
	// the same transaction with the same createdAt.
	if item.CreatedAt != thread.UpdatedAt {
		t.Errorf("item.CreatedAt (%d) and thread.UpdatedAt (%d) must be equal after atomic update",
			item.CreatedAt, thread.UpdatedAt)
	}
}

func TestListPayloadMetas(t *testing.T) {
	s := newTestStore(t)

	// Create two threads.
	thr1 := makeThread("t1", "claude")
	thr2 := makeThread("t2", "codex")
	if err := s.CreateThread(thr1); err != nil {
		t.Fatalf("create t1: %v", err)
	}
	if err := s.CreateThread(thr2); err != nil {
		t.Fatalf("create t2: %v", err)
	}

	now := time.Now().UnixMilli()
	// Insert payloads.
	for _, p := range []Payload{
		{ID: "p1", Kind: "diff", Meta: `{"file":"a.go"}`, Data: []byte("diff1"), CreatedAt: now},
		{ID: "p2", Kind: "diff", Meta: `{"file":"b.go"}`, Data: []byte("diff2"), CreatedAt: now},
		{ID: "p3", Kind: "diff", Meta: `{"file":"c.go"}`, Data: []byte("diff3"), CreatedAt: now},
	} {
		if err := s.InsertPayload(p); err != nil {
			t.Fatalf("insert payload %s: %v", p.ID, err)
		}
	}

	// Insert items linking payloads to threads.
	items := []Item{
		{ID: "i1", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0, Kind: "diff", Role: "assistant", PayloadID: "p1", CreatedAt: now},
		{ID: "i2", ThreadID: "t1", TurnIndex: 0, ItemIndex: 1, Kind: "diff", Role: "assistant", PayloadID: "p2", CreatedAt: now},
		{ID: "i3", ThreadID: "t2", TurnIndex: 0, ItemIndex: 0, Kind: "diff", Role: "assistant", PayloadID: "p3", CreatedAt: now},
	}
	for _, it := range items {
		if err := s.InsertItem(it); err != nil {
			t.Fatalf("insert item %s: %v", it.ID, err)
		}
	}

	// List for t1 should return p1, p2 only.
	metas, err := s.ListPayloadMetas("t1")
	if err != nil {
		t.Fatalf("list payload metas t1: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 metas for t1, got %d", len(metas))
	}
	if metas[0].ID != "p1" || metas[1].ID != "p2" {
		t.Errorf("metas for t1: got [%s, %s], want [p1, p2]", metas[0].ID, metas[1].ID)
	}

	// List for t2 should return p3 only.
	metas2, err := s.ListPayloadMetas("t2")
	if err != nil {
		t.Fatalf("list payload metas t2: %v", err)
	}
	if len(metas2) != 1 {
		t.Fatalf("expected 1 meta for t2, got %d", len(metas2))
	}
	if metas2[0].ID != "p3" {
		t.Errorf("meta for t2: got %q, want %q", metas2[0].ID, "p3")
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

// TestDeleteThreadCascadesPayloadGC verifies that deleting a thread also
// removes every heavy payload attached to any item in that thread. Before
// v9 the items CASCADEd but payloads stuck around forever, quietly
// bloating the database over time.
func TestDeleteThreadCascadesPayloadGC(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	other := makeThread("t-survivor", "codex")
	if err := s.CreateThread(other); err != nil {
		t.Fatalf("create survivor: %v", err)
	}

	// Two payloads owned by items on t1, plus one owned by an item on
	// t-survivor. Only the t1 payloads should be swept.
	if err := s.InsertPayload(Payload{ID: "p1", Kind: "diff", Meta: "{}", Data: []byte("a"), CreatedAt: 1}); err != nil {
		t.Fatalf("p1: %v", err)
	}
	if err := s.InsertPayload(Payload{ID: "p2", Kind: "diff", Meta: "{}", Data: []byte("b"), CreatedAt: 2}); err != nil {
		t.Fatalf("p2: %v", err)
	}
	if err := s.InsertPayload(Payload{ID: "p3", Kind: "diff", Meta: "{}", Data: []byte("c"), CreatedAt: 3}); err != nil {
		t.Fatalf("p3: %v", err)
	}
	if err := s.InsertItem(Item{ID: "i1", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0, Kind: "diff", Role: "assistant", PayloadID: "p1", CreatedAt: 1}); err != nil {
		t.Fatalf("i1: %v", err)
	}
	if err := s.InsertItem(Item{ID: "i2", ThreadID: "t1", TurnIndex: 0, ItemIndex: 1, Kind: "diff", Role: "assistant", PayloadID: "p2", CreatedAt: 2}); err != nil {
		t.Fatalf("i2: %v", err)
	}
	if err := s.InsertItem(Item{ID: "i-keep", ThreadID: "t-survivor", TurnIndex: 0, ItemIndex: 0, Kind: "diff", Role: "assistant", PayloadID: "p3", CreatedAt: 3}); err != nil {
		t.Fatalf("i-keep: %v", err)
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	for _, id := range []string{"p1", "p2"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if n != 0 {
			t.Errorf("payload %s should be swept after thread delete, got %d rows", id, n)
		}
	}

	var survivor int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'p3'`).Scan(&survivor); err != nil {
		t.Fatalf("count p3: %v", err)
	}
	if survivor != 1 {
		t.Errorf("survivor payload should be untouched, got %d rows", survivor)
	}
}

// TestDeleteThreadCascadesPayloadGCScale seeds 1000 items with 1000
// payloads and deletes the thread; guards against regression where the
// trigger or sweep would no-op or crash at scale.
func TestDeleteThreadCascadesPayloadGCScale(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	const n = 1000
	for i := 0; i < n; i++ {
		pid := fmt.Sprintf("p-%04d", i)
		iid := fmt.Sprintf("i-%04d", i)
		if err := s.InsertPayload(Payload{
			ID: pid, Kind: "diff", Meta: "{}",
			Data: []byte{byte(i), byte(i >> 8)}, CreatedAt: int64(i),
		}); err != nil {
			t.Fatalf("payload %d: %v", i, err)
		}
		if err := s.InsertItem(Item{
			ID: iid, ThreadID: "t1",
			TurnIndex: i / 10, ItemIndex: i % 10,
			Kind: "diff", Role: "assistant",
			PayloadID: pid, CreatedAt: int64(i),
		}); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 payloads after thread delete at scale, got %d", remaining)
	}
}

// TestPayloadGCIgnoresSharedPayload covers a subtle case: if two items
// share the same payload_id, deleting ONE item must not drop the payload
// — the other item still references it. Without the guard, the trigger
// could nuke a payload that is still wanted.
func TestPayloadGCIgnoresSharedPayload(t *testing.T) {
	s := newTestStore(t)
	thr := makeThread("t1", "codex")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.InsertPayload(Payload{ID: "shared", Kind: "diff", Meta: "{}", Data: []byte("x"), CreatedAt: 1}); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := s.InsertItem(Item{ID: "a", ThreadID: "t1", TurnIndex: 0, ItemIndex: 0, Kind: "diff", Role: "assistant", PayloadID: "shared", CreatedAt: 1}); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := s.InsertItem(Item{ID: "b", ThreadID: "t1", TurnIndex: 0, ItemIndex: 1, Kind: "diff", Role: "assistant", PayloadID: "shared", CreatedAt: 2}); err != nil {
		t.Fatalf("b: %v", err)
	}

	// Delete one item; payload must survive.
	if _, err := s.db.Exec(`DELETE FROM items WHERE id = 'a'`); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'shared'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("shared payload should survive while item b still references it, got %d", n)
	}

	// Delete the last remaining referencer; now it must go.
	if _, err := s.db.Exec(`DELETE FROM items WHERE id = 'b'`); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'shared'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("shared payload should be swept once last referencer is gone, got %d", n)
	}
}
