package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotForkFixture(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	for _, id := range []string{"source", "fork", "second"} {
		thread := makeThread(id, "claude")
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	item := Item{ID: "item", ThreadID: "source", Kind: "assistant_text", Role: "assistant", Status: "completed", PayloadID: "payload", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	if err := s.InsertItemWithPayload(item, Payload{ID: "payload", Kind: "text", Meta: "{}", Data: []byte("base"), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPayloadData("source", "payload", []byte(" chunk"), "{}", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEditFileSnapshot("source", "payload", "file", "original", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireSnapshotPayload(t *testing.T, s *Store, thread, want string) {
	t.Helper()
	got, err := s.GetPayloadData(thread, "payload")
	if err != nil || string(got) != want {
		t.Fatalf("%s payload=%q, %v; want %q", thread, got, err, want)
	}
	for offset := 0; offset <= len(want); offset++ {
		part, total, _, err := s.GetPayloadChunk(thread, "payload", offset, 3)
		if err != nil || total != len(want) || !bytes.Equal(part, []byte(want[offset:min(offset+3, len(want))])) {
			t.Fatalf("%s chunk %d=%q total=%d: %v", thread, offset, part, total, err)
		}
	}
}

func TestForkPayloadSnapshotSharesBytesAndFlattensForks(t *testing.T) {
	s := snapshotForkFixture(t)
	if _, err := s.CloneThreadHistoryThroughTurn("fork", "second", nil); err != nil {
		t.Fatal(err)
	}
	var snapshots, refs, bytesCopied int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM payload_snapshots),(SELECT count(*) FROM payload_snapshot_refs),(SELECT sum(length(data)) FROM payloads WHERE thread_id IN ('fork','second'))`).Scan(&snapshots, &refs, &bytesCopied); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 || refs != 2 || bytesCopied != 0 {
		t.Fatalf("snapshots=%d refs=%d copied=%d", snapshots, refs, bytesCopied)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("fork"); err != nil {
		t.Fatal(err)
	}
	requireSnapshotPayload(t, s, "second", "base chunk")
	content, found, err := s.GetEditFileSnapshot("second", "payload", "file")
	if err != nil || !found || content != "original" {
		t.Fatalf("edit=%q found=%v: %v", content, found, err)
	}
	if err := s.DeleteThread("second"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"payload_snapshots", "payload_snapshot_refs", "payload_snapshot_chunks", "payload_snapshot_edits"} {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retains %d rows", table, count)
		}
	}
}

func TestForkPayloadSnapshotIndependentMutations(t *testing.T) {
	for _, change := range []string{"append source", "replace source", "edit source", "append fork", "replace fork", "edit fork"} {
		t.Run(change, func(t *testing.T) {
			s := snapshotForkFixture(t)
			var err error
			switch change {
			case "append source":
				err = s.AppendPayloadData("source", "payload", []byte(" new"), "{}", 3)
			case "replace source":
				err = s.ReplacePayloadData("source", "payload", []byte("replacement"), "{}", 3)
			case "edit source":
				err = s.PutEditFileSnapshot("source", "payload", "file", "changed", 3)
			case "append fork":
				err = s.AppendPayloadData("fork", "payload", []byte(" new"), "{}", 3)
			case "replace fork":
				err = s.ReplacePayloadData("fork", "payload", []byte("replacement"), "{}", 3)
			case "edit fork":
				err = s.PutEditFileSnapshot("fork", "payload", "file", "changed", 3)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, thread := range []string{"source", "fork"} {
				want := "base chunk"
				if change == "append "+thread {
					want += " new"
				}
				if change == "replace "+thread {
					want = "replacement"
				}
				requireSnapshotPayload(t, s, thread, want)
				// ReplacePayloadData preserves the existing edit snapshot.
				edit := "original"
				if change == "edit "+thread {
					edit = "changed"
				}
				got, found, err := s.GetEditFileSnapshot(thread, "payload", "file")
				if err != nil || !found || got != edit {
					t.Fatalf("%s edit=%q found=%v: %v", thread, got, found, err)
				}
			}
		})
	}
}

func TestForkPayloadSnapshotRestore(t *testing.T) {
	s := snapshotForkFixture(t)
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := s.SnapshotTo(path); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplacePayloadData("source", "payload", []byte("new"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(path); err != nil {
		t.Fatal(err)
	}
	requireSnapshotPayload(t, s, "fork", "base chunk")
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	requireSnapshotPayload(t, s, "fork", "base chunk")
}

func TestForkPayloadSnapshotImportedHistory(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"source", "fork"} {
		newImportTargetThread(t, s, id)
	}
	if err := s.ApplyImportBatch("source", importBatchFixture("source")); err != nil {
		t.Fatal(err)
	}
	want, err := s.GetPayloadData("source", "payload-out")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPayloadData("fork", "payload-out")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("fork=%q want=%q err=%v", got, want, err)
	}
	if err := s.AppendPayloadData("fork", "payload-out", []byte(" extra"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetPayloadData("fork", "payload-out")
	if err != nil || string(got) != string(want)+" extra" {
		t.Fatalf("appended=%q: %v", got, err)
	}
	if err := s.DeleteThread("fork"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM import_history_chunks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained imports=%d: %v", count, err)
	}
}

func TestForkPayloadSnapshotRawMutationAndRollback(t *testing.T) {
	for _, query := range []string{
		`UPDATE payloads SET data=x'6e6577' WHERE thread_id='source'`,
		`UPDATE payload_chunks SET data=x'6e6577' WHERE thread_id='source'`,
		`DELETE FROM payload_chunks WHERE thread_id='source'`,
		`DELETE FROM edit_file_snapshots WHERE thread_id='source'`,
	} {
		t.Run(query, func(t *testing.T) {
			s := snapshotForkFixture(t)
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			requireSnapshotPayload(t, s, "source", "base chunk")
			requireSnapshotPayload(t, s, "fork", "base chunk")
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			requireSnapshotPayload(t, s, "fork", "base chunk")
			got, found, err := s.GetEditFileSnapshot("fork", "payload", "file")
			if err != nil || !found || got != "original" {
				t.Fatalf("edit=%q found=%v: %v", got, found, err)
			}
		})
	}
}

func TestForkPayloadSnapshotQueriesStayIndexed(t *testing.T) {
	s := snapshotForkFixture(t)
	for _, query := range []string{
		`SELECT data FROM timeline_payloads WHERE thread_id=? AND id=?`,
		`SELECT data FROM timeline_payload_chunks WHERE thread_id=? AND payload_id=? ORDER BY chunk_index`,
		`SELECT content FROM timeline_edit_file_snapshots WHERE thread_id=? AND payload_id=? AND path='file'`,
	} {
		for _, row := range explainPlan(t, s, query, "fork", "payload") {
			// A scan of a UNION coroutine is bounded by its indexed arms. Physical
			// payloads, refs and chunks must never be scanned across threads.
			if strings.HasPrefix(row.detail, "SCAN ") && !strings.Contains(row.detail, "timeline_") {
				t.Errorf("unbounded plan for %s: %s", query, row.detail)
			}
		}
	}
}

func TestForkPayloadSnapshotMigrationPreservesExistingData(t *testing.T) {
	db := migrateThrough(t, 105)
	mustExec(t, db, `INSERT INTO threads(id,title,provider,workspace_path,model,created_at,updated_at,mode) VALUES('old','old','claude','/tmp','',1,1,'chat')`)
	mustExec(t, db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('old','payload','text','{}',x'616263',1)`)
	migrateFrom(t, db, 105)
	var data []byte
	var preparing bool
	if err := db.QueryRow(`SELECT p.data,t.fork_preparing FROM timeline_payloads p JOIN threads t ON t.id=p.thread_id WHERE p.thread_id='old' AND p.id='payload'`).Scan(&data, &preparing); err != nil || string(data) != "abc" || preparing {
		t.Fatalf("data=%q preparing=%v: %v", data, preparing, err)
	}
	if _, err := db.Exec(`UPDATE threads SET fork_preparing=2 WHERE id='old'`); err == nil {
		t.Fatal("invalid preparation state accepted")
	}
}

func TestForkPayloadSnapshotTransferUsesInheritedBytes(t *testing.T) {
	s := snapshotForkFixture(t)
	destination := newTestStore(t)
	thread, err := s.GetThread("fork")
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := s.ExportThreadHistoryWith(t.Context(), thread.ID, &data, ThreadHistoryExport{}); err != nil {
		t.Fatal(err)
	}
	thread.ID = "received"
	if err := destination.ImportThreadHistory(t.Context(), thread, &data); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	requireSnapshotPayload(t, destination, thread.ID, "base chunk")
}

func TestProviderIDRemapIsAtomic(t *testing.T) {
	s := snapshotForkFixture(t)
	err := s.RemapProviderIDs("source", []ItemMetaUpdate{{ItemID: "item", Meta: `{"provider_item_id":"new"}`}, {ItemID: "missing", Meta: `{}`}}, nil)
	if err == nil {
		t.Fatal("missing row remap succeeded")
	}
	item, found, err := s.GetThreadItem("source", "item")
	if err != nil || !found || item.Meta != "{}" {
		t.Fatalf("partial remap: %+v %v", item, err)
	}
}

func TestForkPayloadSnapshotDiffReaders(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"source", "fork"} {
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			t.Fatal(err)
		}
	}
	patch := []byte("--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n")
	item := Item{ThreadID: "source", ID: "edit", Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: "patch", Meta: "{}"}
	if err := s.InsertItemWithPayload(item, Payload{ID: "patch", Kind: "diff", Meta: "{}", Data: patch}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloneThreadHistoryThroughTurn("source", "fork", nil); err != nil {
		t.Fatal(err)
	}
	for _, deleteSource := range []bool{false, true} {
		if deleteSource {
			if err := s.DeleteThread("source"); err != nil {
				t.Fatal(err)
			}
		}
		list, err := s.ListEditDiffItems("fork")
		if err != nil || len(list) != 1 {
			t.Fatalf("list=%+v: %v", list, err)
		}
		patches, err := s.ListTurnEditDiffPatches("fork", 0)
		if err != nil || len(patches) != 1 || !bytes.Equal(patches[0].Data, patch) {
			t.Fatalf("patches=%+v: %v", patches, err)
		}
	}
}

func TestUserMessageMetadataIncludesImportedAndLocalRows(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "source")
	if err := s.ApplyImportBatch("source", importBatchFixture("source")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItem(Item{ThreadID: "source", ID: "local", TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "next", Meta: `{"provider_item_id":"new"}`}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUserMessageMetadata("source")
	if err != nil || len(rows) != 2 {
		t.Fatalf("metadata=%+v: %v", rows, err)
	}
	items, err := s.ListItems("source")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		found := false
		for _, item := range items {
			if row.ItemID == item.ID {
				found = true
				if row.Meta != item.Meta || item.Kind != "user_text" || item.Role != "user" {
					t.Fatalf("mismatched metadata=%+v item=%+v", row, item)
				}
			}
		}
		if !found {
			t.Fatalf("unknown metadata row: %+v", row)
		}
	}
}

func TestForkPayloadSnapshotImportedDiffJoinIsBounded(t *testing.T) {
	s := snapshotForkFixture(t)
	for _, row := range explainPlan(t, s, `SELECT COALESCE(p.data, original.data) FROM thread_import_chunks refs JOIN import_history_items i ON i.chunk_id=refs.chunk_id LEFT JOIN resolved_payloads p ON p.thread_id=refs.thread_id AND p.id=i.payload_id LEFT JOIN import_history_payloads original ON original.chunk_id=i.chunk_id AND original.id=i.payload_id WHERE refs.thread_id=?`, "fork") {
		if strings.HasPrefix(row.detail, "SCAN p") {
			t.Errorf("unbounded imported diff join: %s", row.detail)
		}
	}
}

// SQLite must answer preview byte counts from record headers. Evaluating
// length(COALESCE(blob,...)) instead reads entire tool outputs first.
func TestForkPayloadLengthsNeverMaterializeBlobs(t *testing.T) {
	s := snapshotForkFixture(t)
	type physicalBlob struct {
		name   string
		column int
	}
	roots := map[int]physicalBlob{}
	for table, column := range map[string]int{"payloads": 4, "import_history_payloads": 4, "payload_chunks": 4, "payload_snapshots": 4, "payload_snapshot_chunks": 3} {
		var root int
		if err := s.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&root); err != nil {
			t.Fatal(err)
		}
		roots[root] = physicalBlob{table, column}
	}
	rows, err := s.db.Query("EXPLAIN "+payloadLengthsSQL, "fork", "payload")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cursors := map[int]physicalBlob{}
	checked := 0
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment any
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		if opcode == "OpenRead" {
			if blob, ok := roots[p2]; ok {
				cursors[p1] = blob
			} else {
				delete(cursors, p1)
			}
		}
		if opcode == "Column" {
			if blob, ok := cursors[p1]; ok && p2 == blob.column {
				checked++
				// OPFLAG_LENGTHARG tells OP_Column to retain the byte count without
				// reading the blob's overflow pages. SQLite's EXPLAIN exposes it in P5.
				if p5&0x40 == 0 {
					t.Errorf("%s blob loaded for a length: instruction %d flags=%d", blob.name, addr, p5)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no physical length reads checked")
	}
}
