package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func snapshotTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedSnapshotFixture(t *testing.T, st *Store, threadID, title string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := st.CreateProject(Project{ID: "p1", Path: "/ws/" + threadID, Name: "P", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := st.CreateThread(Thread{
		ID: threadID, ProjectID: "p1", Title: title, Provider: "claude",
		Model: "claude-opus-4-7", WorkspacePath: "/ws/" + threadID, Mode: "chat",
		RuntimeMode: "approval-required", ContextWindow: 200000,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := st.InsertItem(Item{
		ID: threadID + "-i1", ThreadID: threadID, TurnIndex: 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Summary: "hello",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
}

func TestSnapshotAndRestoreRoundTrip(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "before snapshot")

	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	// Mutate after the snapshot: new thread + retitle.
	if err := st.UpdateTitle("t1", "mutated"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}
	now := time.Now().UnixMilli()
	if err := st.CreateThread(Thread{
		ID: "t2", ProjectID: "p1", Title: "extra", Provider: "claude",
		Model: "claude-opus-4-7", WorkspacePath: "/ws/t1", Mode: "chat",
		RuntimeMode: "approval-required", ContextWindow: 200000,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateThread t2: %v", err)
	}

	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	threads, err := st.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 1 || threads[0].ID != "t1" || threads[0].Title != "before snapshot" {
		t.Fatalf("threads after restore = %+v, want just t1 with original title", threads)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 1 || items[0].Summary != "hello" {
		t.Fatalf("items after restore = %+v", items)
	}

	// The store must remain fully usable after restore (FKs re-enabled,
	// transaction state clean).
	if err := st.UpdateTitle("t1", "post-restore write"); err != nil {
		t.Fatalf("write after restore: %v", err)
	}
}

func TestSnapshotToRefusesOverwrite(t *testing.T) {
	st := snapshotTestStore(t)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("first SnapshotTo: %v", err)
	}
	if err := st.SnapshotTo(snap); err == nil {
		t.Fatal("SnapshotTo overwrote an existing file")
	}
}

func TestRestoreFromMissingFile(t *testing.T) {
	st := snapshotTestStore(t)
	if _, err := st.RestoreFrom(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("RestoreFrom accepted a missing snapshot")
	}
}

func TestRestoreFromForeignKeysStayEnforced(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "x")
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	// threads.project_id FK must still be enforced after restore.
	now := time.Now().UnixMilli()
	err := st.CreateThread(Thread{
		ID: "orphan", ProjectID: "no-such-project", Title: "x", Provider: "claude",
		Model: "m", WorkspacePath: "/w", Mode: "chat",
		RuntimeMode: "approval-required", ContextWindow: 200000,
		CreatedAt: now, UpdatedAt: now,
	})
	if err == nil {
		t.Fatal("FK enforcement lost after restore")
	}
}

func TestSnapshotRestoreCannotErasePendingEditRecovery(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "thread", "Before")
	snapshot := filepath.Join(t.TempDir(), "saved.db")
	if err := st.SnapshotTo(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := st.StageThreadDraftRecovery(ThreadDraftRecovery{ThreadID: "thread", SendID: "edit", Content: "unsent edit", Attachments: "[]"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RestoreFrom(snapshot); err == nil {
		t.Fatal("restore discarded pending recovery")
	}
	rows, err := st.ListThreadDraftRecoveries()
	if err != nil || len(rows) != 1 || rows[0].Content != "unsent edit" {
		t.Fatalf("recovery lost: %+v %v", rows, err)
	}
}

// A history restore replaces the corpus the search index describes, and an
// FTS5 table's shadow tables cannot be copied row by row, so the index is
// rebuilt from empty and the background build runs again. Requests and
// receipts are the opposite case: they are promises made outside the history
// the snapshot describes and stay local.
func TestRestoreFromResetsSearchAndKeepsThreadRequests(t *testing.T) {
	st := snapshotTestStore(t)
	seedSnapshotFixture(t, st, "t1", "before snapshot")
	if err := st.BuildSearchIndex(context.Background()); err != nil {
		t.Fatalf("build search index: %v", err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := st.SnapshotTo(snap); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	if err := st.InsertThreadRequest(ThreadRequest{
		Token: "tok-live", CallerThreadID: "t1", Kind: ThreadRequestSend,
		State: ThreadRequestRunning, CreatedAt: 10, UpdatedAt: 10,
	}); err != nil {
		t.Fatalf("insert request: %v", err)
	}
	if _, _, err := st.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-receipt", OwnerDeviceID: "device-1", Kind: ThreadRequestSend,
		TargetThreadID: "t1", CreatedAt: 10, UpdatedAt: 10,
	}); err != nil {
		t.Fatalf("accept receipt: %v", err)
	}
	if err := st.UpdateTitle("t1", "mutated"); err != nil {
		t.Fatalf("UpdateTitle: %v", err)
	}

	if _, err := st.RestoreFrom(snap); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}

	if _, found, err := st.GetThreadRequest("tok-live"); err != nil || !found {
		t.Errorf("restore dropped a live request: found=%v err=%v", found, err)
	}
	if _, found, err := st.GetThreadRequestReceipt("tok-receipt"); err != nil || !found {
		t.Errorf("restore dropped a live receipt: found=%v err=%v", found, err)
	}

	indexing, err := st.SearchIndexing()
	if err != nil {
		t.Fatalf("probe indexing: %v", err)
	}
	if !indexing {
		t.Fatal("a restored database must rebuild its search index")
	}
	var indexed int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM thread_search_rows`).Scan(&indexed); err != nil {
		t.Fatalf("count index rows: %v", err)
	}
	if indexed != 0 {
		t.Errorf("index rows after restore = %d, want an empty index", indexed)
	}
	if err := st.BuildSearchIndex(context.Background()); err != nil {
		t.Fatalf("rebuild search index: %v", err)
	}
	hits, err := st.SearchThreads("hello", ThreadSearchFilter{})
	if err != nil {
		t.Fatalf("search after rebuild: %v", err)
	}
	if len(hits) != 1 || hits[0].ItemID != "t1-i1" {
		t.Fatalf("hits after rebuild = %+v", hits)
	}
	if got, err := st.SearchThreads("mutated", ThreadSearchFilter{}); err != nil || len(got) != 0 {
		t.Errorf("the discarded title still matches: %+v %v", got, err)
	}
}
