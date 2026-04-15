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
