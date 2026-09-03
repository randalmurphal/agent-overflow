package store

import (
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
