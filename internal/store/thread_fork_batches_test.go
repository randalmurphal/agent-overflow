package store

import (
	"fmt"
	"testing"
)

func TestForkBatchesRollbackTogetherAndKeepSearchAndParents(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"source", "fork"} {
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			t.Fatal(err)
		}
	}
	const count = 260
	for i := range count {
		id := fmt.Sprintf("row-%d", i)
		parent := ""
		if i > 0 {
			parent = "row-0"
		}
		item := Item{ID: id, ThreadID: "source", TurnIndex: 0, ItemIndex: i, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "searchableforkhistory", ParentID: parent, PayloadID: id, Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
		if err := s.InsertItemWithPayload(item, Payload{ID: id, Kind: "text", Data: []byte("history"), Meta: "{}", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_late_fork_batch BEFORE INSERT ON items
 WHEN NEW.thread_id='fork' AND NEW.item_index=200
 BEGIN SELECT RAISE(ABORT,'late batch failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err == nil {
		t.Fatal("injected late batch failure succeeded")
	}
	for _, query := range []string{
		`SELECT count(*) FROM items WHERE thread_id='fork'`,
		`SELECT count(*) FROM payloads WHERE thread_id='fork'`,
		`SELECT count(*) FROM thread_search_rows WHERE thread_id='fork' AND source='item' AND item_id<>''`,
		`SELECT count(*) FROM payload_snapshots`,
		`SELECT count(*) FROM payload_snapshot_refs`,
	} {
		var n int
		if err := s.db.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("failed fork retained %d rows: %s", n, query)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_late_fork_batch`); err != nil {
		t.Fatal(err)
	}
	ids, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListItems("fork")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != count {
		t.Fatalf("rows=%d want=%d", len(items), count)
	}
	for _, item := range items {
		if item.ItemIndex > 0 && item.ParentID != ids["row-0"] {
			t.Fatalf("parent across batch boundary: %+v", item)
		}
		if item.Rev <= 0 {
			t.Fatalf("unstamped item: %+v", item)
		}
	}
	var indexed int
	if err := s.db.QueryRow(`SELECT count(*) FROM thread_search_rows WHERE thread_id='fork' AND source='item' AND item_id<>''`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != count {
		t.Fatalf("indexed=%d want=%d", indexed, count)
	}
	hits, err := s.SearchThreads("searchableforkhistory", ThreadSearchFilter{ThreadIDs: []string{"fork"}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range hits {
		if hit.ThreadID == "fork" {
			found = true
		}
	}
	if !found {
		t.Fatal("fork not searchable after batched clone")
	}
}
