package store

import (
	"database/sql"
	"encoding/binary"
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
	if err := s.PutEditFileSnapshot("t1", "p1", "main.go", content, now); err != nil {
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
	if err := s.PutEditFileSnapshot("t1", "p1", "a.txt", "first", now); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	if err := s.PutEditFileSnapshot("t1", "p1", "a.txt", "second", now+1); err != nil {
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
	err = s.PutEditFileSnapshot("t1", "p-missing", "a.txt", "content", now)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("put for missing payload: err = %v, want sql.ErrNoRows", err)
	}
}

func TestEditFileSnapshotCascadesWithPayload(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSnapshotTestPayload(t, s, "t1", "item-1", "p1", 0, 0)
	if err := s.PutEditFileSnapshot("t1", "p1", "a.go", "content", time.Now().UnixMilli()); err != nil {
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

// The write API enforces the capture limit itself.
func TestPutEditFileSnapshotRefusesContentPastTheCaptureLimit(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSnapshotTestPayload(t, s, "t1", "item-1", "p1", 0, 0)
	if err := s.PutEditFileSnapshot("t1", "p1", "big.svg", strings.Repeat("x", MaxEditFileSnapshotBytes+1), time.Now().UnixMilli()); err == nil {
		t.Fatal("content past the capture limit was stored")
	}
	if err := s.PutEditFileSnapshot("t1", "p1", "max.svg", strings.Repeat("x", MaxEditFileSnapshotBytes), time.Now().UnixMilli()); err != nil {
		t.Fatalf("content at the capture limit: %v", err)
	}
}

// DecompressEditFileSnapshot sizes the content before it decompresses
// any, asks for exactly that buffer, and refuses a row whose content is not
// its recorded size or is past the capture limit.
func TestDecompressEditFileSnapshotFillsTheBufferItAskedFor(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t1", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	insertSnapshotTestPayload(t, s, "t1", "item-1", "p1", 0, 0)
	content := strings.Repeat("<g/>", 1000)
	if err := s.PutEditFileSnapshot("t1", "p1", "a.svg", content, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var asked []int64
	var buf []byte
	take := func(n int64) ([]byte, error) {
		asked = append(asked, n)
		buf = make([]byte, n)
		return buf, nil
	}
	blob, found, err := s.GetEditFileSnapshotBlob("t1", "p1", "a.svg")
	if err != nil || !found {
		t.Fatalf("blob: found %v, err %v", found, err)
	}
	size, err := DecompressEditFileSnapshot(blob, take)
	if err != nil || size != int64(len(content)) || string(buf) != content || len(asked) != 1 || asked[0] != int64(len(content)) {
		t.Fatalf("read %d bytes (err %v), asked %v", size, err, asked)
	}
	if _, found, err := s.GetEditFileSnapshotBlob("t-other", "p1", "a.svg"); err != nil || found {
		t.Fatalf("cross-thread payload: found %v, err %v", found, err)
	}
	if _, found, err := s.GetEditFileSnapshotBlob("t1", "p1", "missing.svg"); err != nil || found {
		t.Fatalf("missing path: found %v, err %v", found, err)
	}

	for name, blob := range map[string][]byte{
		"a recorded size past the limit":           withISize(gzipped(t, "abc"), MaxEditFileSnapshotBytes+1),
		"a recorded size shorter than its content": withISize(gzipped(t, "abcdef"), 3),
		"a recorded size longer than its content":  withISize(gzipped(t, "abc"), 6),
	} {
		asked = nil
		if _, err := DecompressEditFileSnapshot(blob, take); err == nil {
			t.Errorf("%s was read", name)
		}
		if name == "a recorded size past the limit" && len(asked) != 0 {
			t.Errorf("%s asked for a buffer of %v", name, asked)
		}
	}
}

func gzipped(t *testing.T, content string) []byte {
	t.Helper()
	blob, err := gzipSnapshotContent(content)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func withISize(blob []byte, size uint32) []byte {
	out := append([]byte(nil), blob...)
	binary.LittleEndian.PutUint32(out[len(out)-4:], size)
	return out
}
