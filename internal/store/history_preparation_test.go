package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPreparedHistoryDeletedItemCannotBeMutated(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 20)
	prepareAllHistory(t, s, "source")
	if _, _, err := s.DeleteConversationFromTurn("source", 1); err != nil {
		t.Fatal(err)
	}
	// The retained prefix keeps this chunk attached. Deleted rows still exist
	// physically, but every mutation must see the same logical absence as reads.
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.UpdateItemMeta("source", "item-015", `{"late":true}`); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("late update of deleted row: %v, want ErrNoRows", err)
		}
	}
	if _, found, err := s.GetThreadItem("source", "item-015"); err != nil || found {
		t.Fatalf("deleted item reappeared: found=%v err=%v", found, err)
	}
}

func TestDeletePreparedItemDoesNotMaterializeAndCollectsLastReference(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 1)
	prepareAllHistory(t, s, "source")
	if err := s.CreateThread(makeThread("fork", "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `CREATE TRIGGER reject_delete_materialization BEFORE INSERT ON items BEGIN SELECT RAISE(ABORT,'delete copied immutable history'); END`)
	if err := s.DeleteThreadItem("source", "item-000"); err != nil {
		t.Fatal(err)
	}
	data, err := s.GetPayloadData("fork", "item-000")
	if err != nil || string(data) != "original chunk" {
		t.Fatalf("source deletion changed fork: %q %v", data, err)
	}
	if err := s.DeleteThreadItem("fork", "item-000"); err != nil {
		t.Fatal(err)
	}
	var chunks, refs, overrides int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM import_history_chunks), (SELECT count(*) FROM thread_import_chunks), (SELECT count(*) FROM thread_import_item_overrides)`).Scan(&chunks, &refs, &overrides); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 || refs != 0 || overrides != 0 {
		t.Fatalf("deleted history retained chunks=%d refs=%d overrides=%d", chunks, refs, overrides)
	}
	if err := s.DeleteThreadItem("fork", "item-000"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("repeat deletion: %v, want ErrNoRows", err)
	}
}

func preparedHistoryFixture(t *testing.T, s *Store, thread string, count int) {
	t.Helper()
	if err := s.CreateThread(makeThread(thread, "claude")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("item-%03d", i)
		item := Item{ThreadID: thread, ID: id, TurnIndex: i / 10, ItemIndex: i % 10, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "inherited searchable history", PayloadID: id, Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
		if err := s.InsertItemWithPayload(item, Payload{ID: id, Kind: "text", Meta: "{}", Data: []byte("original "), CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendPayloadData(thread, id, []byte("chunk"), "{}", 2); err != nil {
			t.Fatal(err)
		}
	}
}

func prepareAllHistory(t *testing.T, s *Store, thread string) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		n, err := s.PrepareThreadHistory(context.Background(), thread)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if n > historyPreparationRows {
			t.Fatalf("preparation batch has %d rows", n)
		}
	}
	t.Fatal("history preparation did not finish")
}

func TestPreparedHistoryPreservesReadsSearchAndForkIsolation(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 140)
	before, err := s.ListItems("source")
	if err != nil {
		t.Fatal(err)
	}
	var rev, epoch int64
	if err := s.db.QueryRow(`SELECT history_rev,history_epoch FROM threads WHERE id='source'`).Scan(&rev, &epoch); err != nil {
		t.Fatal(err)
	}
	beforeSearch, err := s.SearchThreads("inherited", ThreadSearchFilter{ThreadIDs: []string{"source"}, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	prepareAllHistory(t, s, "source")
	after, err := s.ListItems("source")
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		before[i].Rev = 0
		after[i].Rev = 0
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("preparation changed logical history")
	}
	var gotRev, gotEpoch int64
	if err := s.db.QueryRow(`SELECT history_rev,history_epoch FROM threads WHERE id='source'`).Scan(&gotRev, &gotEpoch); err != nil {
		t.Fatal(err)
	}
	if gotRev != rev || gotEpoch != epoch {
		t.Fatalf("preparation changed stamps: %d/%d -> %d/%d", rev, epoch, gotRev, gotEpoch)
	}
	afterSearch, err := s.SearchThreads("inherited", ThreadSearchFilter{ThreadIDs: []string{"source"}, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for i := range beforeSearch {
		beforeSearch[i].Source = ""
		afterSearch[i].Source = ""
	}
	if !reflect.DeepEqual(beforeSearch, afterSearch) {
		t.Fatal("preparation changed search ranking or hits")
	}
	for _, pair := range [][2]string{{"source", "fork"}, {"fork", "grandchild"}} {
		if err := s.CreateThread(makeThread(pair[1], "claude")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloneThreadHistoryThroughTurn(pair[0], pair[1], nil); err != nil {
			t.Fatal(err)
		}
		var private int
		if err := s.db.QueryRow(`SELECT count(*) FROM items WHERE thread_id=?`, pair[1]).Scan(&private); err != nil {
			t.Fatal(err)
		}
		if private != 0 {
			t.Fatalf("fork copied %d prepared items", private)
		}
	}
	if err := s.BuildSearchIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	hits := mustSearch(t, s, "inherited", ThreadSearchFilter{ThreadIDs: []string{"source", "fork", "grandchild"}, Limit: 500})
	identities := make(map[[2]string]bool)
	for _, hit := range hits {
		identities[[2]string{hit.ThreadID, hit.ItemID}] = true
	}
	if len(hits) != 420 || len(identities) != 420 {
		t.Fatalf("search rebuild changed fork identities: %d hits / %d identities", len(hits), len(identities))
	}
	if err := s.UpdateItemMeta("source", "item-000", `{"changed":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplacePayloadData("source", "item-000", []byte("changed"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"fork", "grandchild"} {
		data, err := s.GetPayloadData(thread, "item-000")
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "original chunk" {
			t.Fatalf("%s observed source mutation: %q", thread, data)
		}
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("fork"); err != nil {
		t.Fatal(err)
	}
	data, err := s.GetPayloadData("grandchild", "item-139")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original chunk" {
		t.Fatalf("last branch lost payload: %q", data)
	}
	if err := s.DeleteThread("grandchild"); err != nil {
		t.Fatal(err)
	}
	var chunks int
	if err := s.db.QueryRow(`SELECT count(*) FROM import_history_chunks`).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Fatalf("last branch retained %d chunks", chunks)
	}
}

func TestPreparedHistoryRollsBackAndSkipsLiveAndAnchoredRows(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 4)
	if _, err := s.db.Exec(`UPDATE items SET kind='user_text',role='user' WHERE id='item-000'; UPDATE items SET status='streaming' WHERE id='item-001'`); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMessageAnchor(MessageAnchor{ThreadID: "source", UserItemID: "item-000", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_prepared_attach BEFORE INSERT ON thread_import_chunks BEGIN SELECT RAISE(ABORT,'prepared attach failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareThreadHistory(context.Background(), "source"); err == nil {
		t.Fatal("injected failure succeeded")
	}
	var private, bulk, chunks int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM items WHERE thread_id='source'),history_bulk_load,(SELECT count(*) FROM import_history_chunks) FROM threads WHERE id='source'`).Scan(&private, &bulk, &chunks); err != nil {
		t.Fatal(err)
	}
	if private != 4 || bulk != 0 || chunks != 0 {
		t.Fatalf("partial preparation survived: items=%d bulk=%d chunks=%d", private, bulk, chunks)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_prepared_attach`); err != nil {
		t.Fatal(err)
	}
	prepareAllHistory(t, s, "source")
	if _, found, err := s.GetMessageAnchor("source", "item-000"); err != nil || !found {
		t.Fatalf("anchor lost: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM items WHERE thread_id='source'`).Scan(&private); err != nil {
		t.Fatal(err)
	}
	if private != 2 {
		t.Fatalf("private rows=%d, want user and active row", private)
	}
}

func TestPreparedHistoryCutsKeepSharedPrefix(t *testing.T) {
	for _, message := range []bool{false, true} {
		t.Run(fmt.Sprint("message=", message), func(t *testing.T) {
			s := newTestStore(t)
			preparedHistoryFixture(t, s, "source", 140)
			prepareAllHistory(t, s, "source")
			if err := s.CreateThread(makeThread("fork", "claude")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
				t.Fatal(err)
			}
			// Any materialization of the kept prefix makes this test fail.
			if _, err := s.db.Exec(`CREATE TRIGGER reject_history_materialization BEFORE INSERT ON items WHEN NEW.thread_id='fork' BEGIN SELECT RAISE(ABORT,'cut materialized history'); END`); err != nil {
				t.Fatal(err)
			}
			want := 70
			if message {
				kept, _, err := s.DeleteConversationFromItem("fork", "item-075")
				if err != nil {
					t.Fatal(err)
				}
				if len(kept) != 5 {
					t.Fatalf("kept anchor turn has %d rows", len(kept))
				}
				want = 75
			} else {
				if _, _, err := s.DeleteConversationFromTurn("fork", 7); err != nil {
					t.Fatal(err)
				}
			}
			items, err := s.ListItems("fork")
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != want {
				t.Fatalf("kept %d rows, want %d", len(items), want)
			}
			source, err := s.ListItems("source")
			if err != nil {
				t.Fatal(err)
			}
			if len(source) != 140 {
				t.Fatal("fork cut changed source")
			}
			hits, err := s.SearchThreads("inherited", ThreadSearchFilter{ThreadIDs: []string{"fork"}, Limit: 200})
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != want {
				t.Fatalf("search retained %d rows, want %d", len(hits), want)
			}
			if _, err := s.db.Exec(`DROP TRIGGER reject_history_materialization`); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateThread(makeThread("grandchild", "claude")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CloneThreadHistoryThroughTurn("fork", "grandchild", nil); err != nil {
				t.Fatal(err)
			}
			items, err = s.ListItems("grandchild")
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != want {
				t.Fatalf("fork after cut restored deleted history: %d", len(items))
			}
		})
	}
}

func TestPreparedHistoryForkCopiesPayloadOverrides(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 3)
	prepareAllHistory(t, s, "source")
	// Payload mutation can create a private overlay without localizing its item.
	if err := s.ReplacePayloadData("source", "item-000", []byte("new bytes"), `{"new":true}`, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(makeThread("fork", "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
		t.Fatal(err)
	}
	data, err := s.GetPayloadData("fork", "item-000")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new bytes" {
		t.Fatalf("fork ignored private payload override: %q", data)
	}
}

func TestPreparedHistorySnapshotRestoreWithPrivateOverride(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 80)
	prepareAllHistory(t, s, "source")
	if err := s.UpdateItemMeta("source", "item-000", `{"override":true}`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(makeThread("fork", "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source", "fork"} {
		items, err := s.ListItems(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 80 {
			t.Fatalf("restored %s has %d rows", id, len(items))
		}
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	data, err := s.GetPayloadData("fork", "item-079")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original chunk" {
		t.Fatalf("restored fork lost data: %q", data)
	}
}

func TestPreparedHistoryBoundsCancellationAndPlanLifetime(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 70)
	// A plan can acquire its dependent state after its item is persisted.
	mustExec(t, s.db, `UPDATE payloads SET kind='proposed_plan' WHERE thread_id='source' AND id='item-000'`)
	mustExec(t, s.db, `UPDATE payloads SET data=zeroblob(?) WHERE thread_id='source' AND id='item-001'`, historyPreparationBytes+1)
	for _, id := range []string{"item-002", "item-003", "item-004"} {
		mustExec(t, s.db, `UPDATE payloads SET data=zeroblob(?) WHERE thread_id='source' AND id=?`, historyPreparationBytes/2, id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.PrepareThreadHistory(ctx, "source"); err == nil {
		t.Fatal("canceled preparation succeeded")
	}
	var total int
	mustCount := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := mustCount(`SELECT count(*) FROM items WHERE thread_id='source'`); n != 70 {
		t.Fatalf("cancellation changed %d rows", n)
	}
	for {
		n, err := s.PrepareThreadHistory(context.Background(), "source")
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
		if n > historyPreparationRows {
			t.Fatalf("unbounded batch: %d", n)
		}
	}
	if total != 68 {
		t.Fatalf("prepared %d rows, want 68", total)
	}
	if n := mustCount(`SELECT count(*) FROM import_history_chunks c WHERE c.item_count>? OR (SELECT sum(length(data)) FROM import_history_payloads p WHERE p.chunk_id=c.id)>?`, historyPreparationRows, historyPreparationBytes); n != 0 {
		t.Fatalf("%d chunks exceed the budget", n)
	}
	for _, id := range []string{"item-000", "item-001"} {
		if n := mustCount(`SELECT count(*) FROM items WHERE thread_id='source' AND id=?`, id); n != 1 {
			t.Fatalf("protected row %s moved", id)
		}
	}
	if _, err := s.EnsureProposedPlanState("source", "item-000", 3); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedDescendantsKeepWireDecorationAndAncestorRevisions(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 4)
	mustExec(t, s.db, `UPDATE items SET kind='tool_call',tool_name='Agent' WHERE thread_id='source' AND id='item-000'`)
	mustExec(t, s.db, `UPDATE items SET parent_id='item-000' WHERE thread_id='source' AND id<>'item-000'`)
	before, err := s.ListWireItems("source", []string{"item-000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatal(before)
	}
	prepareAllHistory(t, s, "source")
	needs, err := s.ItemReadNeedsDecoration(before[0])
	if err != nil || !needs {
		t.Fatalf("prepared descendants need decoration=%v err=%v", needs, err)
	}
	after, err := s.ListWireItems("source", []string{"item-000"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("decoration changed: before=%+v after=%+v", before, after)
	}
	// A seal between the write and its emission must still refresh its parent.
	behind, err := s.ListWireItemsBehind("source", map[string]int64{"item-003": 0})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range behind {
		found = found || item.ID == "item-000"
	}
	if !found {
		t.Fatalf("sealed write lost parent refresh: %+v", behind)
	}
	if _, _, err := s.DeleteConversationFromItem("source", "item-002"); err != nil {
		t.Fatal(err)
	}
	after, err = s.ListWireItems("source", []string{"item-000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Rev <= before[0].Rev {
		t.Fatalf("cut did not stamp retained parent: %+v", after)
	}
}

func TestPreparedHistoryCutCollectsPrivatePayloadOverrides(t *testing.T) {
	s := newTestStore(t)
	preparedHistoryFixture(t, s, "source", 12)
	prepareAllHistory(t, s, "source")
	if err := s.ReplacePayloadData("source", "item-011", []byte("private override"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateThread(makeThread("fork", "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DeleteConversationFromItem("source", "item-011"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM payloads WHERE thread_id='source' AND id='item-011'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("cut leaked a private payload override")
	}
	data, err := s.GetPayloadData("fork", "item-011")
	if err != nil || string(data) != "private override" {
		t.Fatalf("cut changed fork snapshot: %q %v", data, err)
	}
}
