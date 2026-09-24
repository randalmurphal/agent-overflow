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

func TestImportedHistoryDeletedItemCannotBeMutated(t *testing.T) {
	s := newTestStore(t)
	importedHistoryFixture(t, s, "source", 20)
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

func TestDeleteImportedItemDoesNotMaterializeAndCollectsLastReference(t *testing.T) {
	s := newTestStore(t)
	importedHistoryFixture(t, s, "source", 1)
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

// importedHistoryFixture imports count completed rows, ten per turn, each
// with its own payload. One batch keeps them in one shared chunk.
func importedHistoryFixture(t *testing.T, s *Store, thread string, count int) {
	t.Helper()
	newImportTargetThread(t, s, thread)
	var batch ImportBatch
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("item-%03d", i)
		batch.Rows = append(batch.Rows, ImportRow{
			Item:    Item{ID: id, TurnIndex: i / 10, ItemIndex: i % 10, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "inherited searchable history", PayloadID: id, Meta: "{}", CreatedAt: 1, UpdatedAt: 1},
			Payload: &Payload{ID: id, Kind: "text", Meta: "{}", Data: []byte("original chunk"), CreatedAt: 1},
		})
	}
	if err := s.ApplyImportBatch(thread, batch); err != nil {
		t.Fatal(err)
	}
}

func TestImportedHistoryForkIsolationSearchAndLastReference(t *testing.T) {
	s := newTestStore(t)
	importedHistoryFixture(t, s, "source", 140)
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
			t.Fatalf("fork copied %d imported items", private)
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

func TestImportedHistoryCutsKeepSharedPrefix(t *testing.T) {
	for _, message := range []bool{false, true} {
		t.Run(fmt.Sprint("message=", message), func(t *testing.T) {
			s := newTestStore(t)
			importedHistoryFixture(t, s, "source", 140)
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

func TestImportedHistoryForkCopiesPayloadOverrides(t *testing.T) {
	s := newTestStore(t)
	importedHistoryFixture(t, s, "source", 3)
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

func TestImportedHistorySnapshotRestoreWithPrivateOverride(t *testing.T) {
	s := newTestStore(t)
	importedHistoryFixture(t, s, "source", 80)
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

// A local launch whose children are imported decorates exactly as it does
// with local children, and a held child still refreshes it.
func TestImportedDescendantsKeepWireDecorationAndAncestorRevisions(t *testing.T) {
	s := newTestStore(t)
	launch := Item{ID: "item-000", Kind: "tool_call", ToolName: "Agent", Role: "assistant", Status: "completed", Summary: "agent", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	children := func(thread string) []Item {
		var rows []Item
		for i := 1; i < 4; i++ {
			rows = append(rows, Item{ThreadID: thread, ID: fmt.Sprintf("item-%03d", i), ItemIndex: i, ParentID: launch.ID, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "child", Meta: "{}", CreatedAt: 1, UpdatedAt: 1})
		}
		return rows
	}
	newImportTargetThread(t, s, "source")
	mustCreateThread(t, s, "local")
	for _, thread := range []string{"source", "local"} {
		row := launch
		row.ThreadID = thread
		if err := insertCarded(s, row); err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range children("local") {
		if err := insertCarded(s, child); err != nil {
			t.Fatal(err)
		}
	}
	var batch ImportBatch
	for _, child := range children("source") {
		batch.Rows = append(batch.Rows, ImportRow{Item: child})
	}
	if err := s.ApplyImportBatch("source", batch); err != nil {
		t.Fatal(err)
	}
	local, err := s.ListWireItems("local", []string{launch.ID})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := s.ListWireItems("source", []string{launch.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 || len(imported) != 1 {
		t.Fatalf("launch reads: local=%+v imported=%+v", local, imported)
	}
	// The bulk load stamped the launch, so its stored row is its read.
	needs, err := s.ItemReadNeedsDecoration(imported[0])
	if err != nil || needs {
		t.Fatalf("stamped launch over imported descendants needs decoration=%v err=%v", needs, err)
	}
	local[0].ThreadID, local[0].Rev = "", 0
	before := imported[0]
	imported[0].ThreadID, imported[0].Rev = "", 0
	if !reflect.DeepEqual(local[0], imported[0]) {
		t.Fatalf("decoration differs: local=%+v imported=%+v", local[0], imported[0])
	}
	behind, err := s.ListWireItemsBehind("source", map[string]int64{"item-003": 0})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range behind {
		found = found || item.ID == launch.ID
	}
	if !found {
		t.Fatalf("held imported child lost parent refresh: %+v", behind)
	}
	if _, _, err := s.DeleteConversationFromItem("source", "item-002"); err != nil {
		t.Fatal(err)
	}
	after, err := s.ListWireItems("source", []string{launch.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Rev <= before.Rev {
		t.Fatalf("cut did not stamp retained parent: %+v", after)
	}
}

func TestImportedHistoryCutCollectsPrivatePayloadOverrides(t *testing.T) {
	s := newTestStore(t)
	importedHistoryFixture(t, s, "source", 12)
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
