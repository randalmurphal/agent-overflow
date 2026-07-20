package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func insertSnapshotTestPayload(t *testing.T, s *Store, threadID, itemID, payloadID string, itemIndex, turnIndex int) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := s.InsertItemWithPayload(Item{
		ID:        itemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "diff",
		PayloadID: payloadID,
		CreatedAt: now,
		UpdatedAt: now,
	}, Payload{
		ID:        payloadID,
		Kind:      "tool_result",
		Meta:      "{}",
		Data:      []byte("diff --git a/a b/a"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert item+payload: %v", err)
	}
}

func TestEditFileSnapshotRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSnapshotTestPayload(t, s, "t1", "item-1", "p1", 0, 0)

	content := strings.Repeat("package main\n\nfunc main() {}\n", 200)
	now := time.Now().UnixMilli()
	if err := s.PutEditFileSnapshot("p1", "main.go", content, now); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}

	got, found, err := s.GetEditFileSnapshot("t1", "p1", "main.go")
	if err != nil || !found {
		t.Fatalf("get snapshot: %v found=%v", err, found)
	}
	if got != content {
		t.Fatalf("snapshot content mismatch: got %d bytes, want %d", len(got), len(content))
	}

	// The stored blob is gzip (magic bytes), and compresses repetitive
	// source well below the raw size — the storage win the table exists
	// for.
	var blob []byte
	if err := s.db.QueryRow(
		`SELECT content FROM edit_file_snapshots WHERE payload_id = 'p1' AND path = 'main.go'`,
	).Scan(&blob); err != nil {
		t.Fatalf("read raw blob: %v", err)
	}
	if len(blob) < 2 || blob[0] != 0x1f || blob[1] != 0x8b {
		t.Fatalf("stored blob is not gzip, leading bytes % x", blob[:min(4, len(blob))])
	}
	if len(blob) >= len(content) {
		t.Fatalf("blob (%d bytes) not smaller than content (%d bytes)", len(blob), len(content))
	}
}

func TestEditFileSnapshotOverwriteAndMiss(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSnapshotTestPayload(t, s, "t1", "item-1", "p1", 0, 0)
	now := time.Now().UnixMilli()

	// Re-persisting the same payload replaces the row.
	if err := s.PutEditFileSnapshot("p1", "a.txt", "first", now); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	if err := s.PutEditFileSnapshot("p1", "a.txt", "second", now+1); err != nil {
		t.Fatalf("overwrite snapshot: %v", err)
	}
	got, found, err := s.GetEditFileSnapshot("t1", "p1", "a.txt")
	if err != nil || !found {
		t.Fatalf("get snapshot: %v found=%v", err, found)
	}
	if got != "second" {
		t.Fatalf("snapshot = %q, want %q", got, "second")
	}

	// Absent rows are a state, not an error — callers fall back to
	// workspace verification.
	if _, found, err = s.GetEditFileSnapshot("t1", "p1", "other.txt"); err != nil || found {
		t.Fatalf("missing path: err=%v found=%v, want nil/false", err, found)
	}
	if _, found, err = s.GetEditFileSnapshot("t1", "p-missing", "a.txt"); err != nil || found {
		t.Fatalf("missing payload: err=%v found=%v, want nil/false", err, found)
	}
	// Thread containment: a payload id from another thread is a miss.
	if _, found, err = s.GetEditFileSnapshot("t-other", "p1", "a.txt"); err != nil || found {
		t.Fatalf("cross-thread payload: err=%v found=%v, want nil/false", err, found)
	}

	// Writing against a deleted payload is the async-worker deletion
	// race: wrapped sql.ErrNoRows, never a foreign-key violation.
	err = s.PutEditFileSnapshot("p-missing", "a.txt", "content", now)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("put for missing payload: err = %v, want sql.ErrNoRows", err)
	}
}

func TestGetLatestTurnEditFileSnapshot(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	// Turn 3 touches a.go twice (items 2 and 5) and b.go once; turn 4
	// touches a.go again — it must not leak into turn 3's answer.
	insertSnapshotTestPayload(t, s, "t1", "item-1", "p1", 2, 3)
	insertSnapshotTestPayload(t, s, "t1", "item-2", "p2", 5, 3)
	insertSnapshotTestPayload(t, s, "t1", "item-3", "p3", 4, 3)
	insertSnapshotTestPayload(t, s, "t1", "item-4", "p4", 0, 4)
	now := time.Now().UnixMilli()
	for _, put := range []struct{ payloadID, path, content string }{
		{"p1", "a.go", "a v1"},
		{"p2", "a.go", "a v2"},
		{"p3", "b.go", "b v1"},
		{"p4", "a.go", "a turn4"},
	} {
		if err := s.PutEditFileSnapshot(put.payloadID, put.path, put.content, now); err != nil {
			t.Fatalf("put %s %s: %v", put.payloadID, put.path, err)
		}
	}

	got, found, err := s.GetLatestTurnEditFileSnapshot("t1", 3, "a.go")
	if err != nil || !found {
		t.Fatalf("latest a.go: %v found=%v", err, found)
	}
	if got != "a v2" {
		t.Fatalf("latest a.go = %q, want %q", got, "a v2")
	}

	got, found, err = s.GetLatestTurnEditFileSnapshot("t1", 3, "b.go")
	if err != nil || !found {
		t.Fatalf("latest b.go: %v found=%v", err, found)
	}
	if got != "b v1" {
		t.Fatalf("latest b.go = %q, want %q", got, "b v1")
	}

	if _, found, err = s.GetLatestTurnEditFileSnapshot("t1", 5, "a.go"); err != nil || found {
		t.Fatalf("turn without edits: err=%v found=%v, want nil/false", err, found)
	}
}

func TestEditFileSnapshotCascadesWithPayload(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSnapshotTestPayload(t, s, "t1", "item-1", "p1", 0, 0)
	if err := s.PutEditFileSnapshot("p1", "a.go", "content", time.Now().UnixMilli()); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}

	// Deleting the item fires the payload GC trigger; the snapshot must
	// ride the cascade so orphan rows can't accumulate.
	if _, err := s.db.Exec(`DELETE FROM items WHERE thread_id = 't1' AND id = 'item-1'`); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	var payloads int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'p1'`).Scan(&payloads); err != nil {
		t.Fatalf("count payloads: %v", err)
	}
	if payloads != 0 {
		t.Fatalf("payload GC trigger did not fire, %d rows remain", payloads)
	}
	var snapshots int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM edit_file_snapshots`).Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapshots != 0 {
		t.Fatalf("snapshot did not cascade with payload delete, %d rows remain", snapshots)
	}
}
