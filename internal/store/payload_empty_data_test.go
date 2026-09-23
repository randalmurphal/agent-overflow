package store

import (
	"context"
	"strings"
	"testing"
)

// requireEmptyBlob asserts the payload row holds a zero-length blob.
func requireEmptyBlob(t *testing.T, s *Store, table, where string, args ...any) {
	t.Helper()
	var kind string
	var length int
	if err := s.db.QueryRow(`SELECT typeof(data), length(data) FROM `+table+` WHERE `+where, args...).Scan(&kind, &length); err != nil {
		t.Fatalf("read %s %v: %v", table, args, err)
	}
	if kind != "blob" || length != 0 {
		t.Fatalf("%s %v data = %s of %d bytes, want an empty blob", table, args, kind, length)
	}
}

func emptyPayloadItem(thread, id string, itemIndex int) Item {
	return Item{
		ID: id, ThreadID: thread, TurnIndex: 0, ItemIndex: itemIndex,
		Kind: "command_result", Role: "assistant", Status: "completed",
		Meta: "{}", CreatedAt: 1, UpdatedAt: 1,
	}
}

// The driver binds a nil slice as NULL and scans a zero-length blob back as
// nil, and payload data is BLOB NOT NULL. Every payload writer stores an
// empty payload as an empty blob, including one read back from the database.
func TestEmptyPayloadDataIsStoredAsAnEmptyBlob(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatal(err)
	}

	item := emptyPayloadItem("t", "a", 0)
	item.PayloadID = "pa"
	if err := s.InsertItemWithPayload(item, Payload{ID: "pa", Kind: "command_output", Meta: "{}", CreatedAt: 1}); err != nil {
		t.Fatalf("insert empty payload: %v", err)
	}
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", "t", "pa")

	readBack, err := s.GetPayloadData("t", "pa")
	if err != nil || len(readBack) != 0 {
		t.Fatalf("read back = %q, %v", readBack, err)
	}
	if _, err := s.UpsertItem(emptyPayloadItem("t", "b", 1), &Payload{ID: "pb", Kind: "command_output", Meta: "{}", Data: readBack, CreatedAt: 1}); err != nil {
		t.Fatalf("upsert a read-back empty payload: %v", err)
	}
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", "t", "pb")

	if err := s.ReplacePayloadData("t", "pa", nil, "{}", 2); err != nil {
		t.Fatalf("replace with empty data: %v", err)
	}
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", "t", "pa")

	batchItem := emptyPayloadItem("t", "c", 2)
	batchItem.PayloadID = "pc"
	if err := s.InsertThreadHistory("t", ThreadHistoryBatch{Rows: []HistoryRow{{
		Item: batchItem, Payload: &Payload{ID: "pc", Kind: "command_output", Meta: "{}", CreatedAt: 1},
	}}}); err != nil {
		t.Fatalf("insert empty payload in a history batch: %v", err)
	}
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", "t", "pc")
}

// An empty payload goes into a sealed chunk and comes back out through v119's
// deferred phase as an empty blob, with nothing left behind or reported.
// Sealing read the bytes back through the driver, so this is the path that
// failed with "NOT NULL constraint failed" on a live database.
func TestEmptyPayloadRoundTripsThroughSealingAndTheV119Phase(t *testing.T) {
	s := openStoreAt(t)
	ids := localHistoryFixture(t, s, "t", 10)
	empty := emptyPayloadItem("t", "empty", 20)
	if _, err := s.UpsertItem(empty, &Payload{ID: "p-empty", Kind: "tool_call_result", Meta: "{}", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	chunk := sealItemsForTest(t, s, "t", append(ids, "empty")...)
	requireEmptyBlob(t, s, "import_history_payloads", "chunk_id = ? AND id = ?", chunk, "p-empty")
	mustExec(t, s.db, `PRAGMA user_version = 118`)
	s = reopenStore(t, s)
	logged := captureLog(t)

	if err := s.RunDeferredMigrations(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if deferredPending(t, s) {
		t.Fatal("the phase did not finish")
	}
	if strings.Contains(logged(), "left in place") || strings.Contains(logged(), "keeps the rest") {
		t.Fatalf("the phase skipped work:\n%s", logged())
	}
	requireNoImportedHistory(t, s)
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", "t", "p-empty")
	if data, err := s.GetPayloadData("t", "p-empty"); err != nil || len(data) != 0 {
		t.Fatalf("folded empty payload = %q, %v", data, err)
	}
}
