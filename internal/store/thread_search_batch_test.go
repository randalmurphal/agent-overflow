package store

import (
	"fmt"
	"reflect"
	"testing"
)

func TestBatchedForkSearchMatchesSingleRowIndexAndReplacesOldText(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"single", "batch"} {
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			t.Fatal(err)
		}
	}
	var items []Item
	for i := 0; i < 270; i++ {
		kind, status := "assistant_text", "completed"
		if i%7 == 0 {
			kind = "thinking"
		}
		if i%11 == 0 {
			status = "streaming"
		}
		item := Item{ThreadID: "single", ID: fmt.Sprint(i), Kind: kind, Status: status, Summary: "identical searchable"}
		items = append(items, item)
	}
	write := func(batch bool, word string) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		copied := append([]Item(nil), items...)
		for i := range copied {
			copied[i].Summary = word
			if batch {
				copied[i].ThreadID = "batch"
			} else if err := indexSettledItemTx(tx, "single", copied[i].ID, copied[i].Kind, copied[i].Status, word); err != nil {
				t.Fatal(err)
			}
		}
		if batch {
			if err := indexSettledItemsTx(tx, copied, ThreadSearchSourceItem); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// Search joins history rows, so seed the same timelines on each side.
	for _, thread := range []string{"single", "batch"} {
		for i, item := range items {
			item.ThreadID = thread
			item.TurnIndex = i
			item.Role = "assistant"
			item.Meta = "{}"
			item.CreatedAt = 1
			item.UpdatedAt = 1
			if err := s.InsertItem(item); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, word := range []string{"original", "replacement"} {
		write(false, word)
		write(true, word)
		a := mustSearch(t, s, word, ThreadSearchFilter{ThreadIDs: []string{"single"}, Limit: 500})
		b := mustSearch(t, s, word, ThreadSearchFilter{ThreadIDs: []string{"batch"}, Limit: 500})
		if len(a) == 0 || len(a) != len(b) {
			t.Fatalf("hit counts %d/%d", len(a), len(b))
		}
		for i := range a {
			if a[i].ItemID != b[i].ItemID || a[i].Kind != b[i].Kind || a[i].Source != b[i].Source {
				t.Fatalf("search order diverged at %d: %+v / %+v", i, a[i], b[i])
			}
		}
		if !reflect.DeepEqual(searchIndexRows(t, s, "single"), searchIndexRows(t, s, "batch")) {
			t.Fatal("indexable kinds or statuses changed")
		}
	}
	if hits := mustSearch(t, s, "original", ThreadSearchFilter{Limit: 500}); len(hits) != 0 {
		t.Fatalf("upsert kept %d stale documents", len(hits))
	}
	// A contentless FTS document can outlive a removed mapping until cleanup.
	mustExec(t, s.db, `DELETE FROM thread_search_rows WHERE thread_id='batch'`)
	write(true, "fresh")
	if hits := mustSearch(t, s, "replacement", ThreadSearchFilter{ThreadIDs: []string{"batch"}, Limit: 500}); len(hits) != 0 {
		t.Fatal("reused mapping rowids retained old FTS tokens")
	}
}
