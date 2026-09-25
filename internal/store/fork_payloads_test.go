package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// payloadForkFixture is a source with one row whose payload has an append
// chunk and an edit snapshot, and a pointer fork of it.
func payloadForkFixture(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	mustCreateThread(t, s, "source")
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
	mustPointerFork(t, s, "source", "fork", ForkCut{})
	return s
}

// requireForkPayload reads the payload whole and through every chunked
// window, the two shapes the payload readers take.
func requireForkPayload(t *testing.T, s *Store, thread, want string) {
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

// TestPointerForkReadsPayloadsThroughEveryLevel: a fork and a fork of it
// read the source's payload, chunks and snapshots without a byte copied.
func TestPointerForkReadsPayloadsThroughEveryLevel(t *testing.T) {
	s := payloadForkFixture(t)
	mustPointerFork(t, s, "fork", "second", ForkCut{})
	var copied int
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM payloads WHERE thread_id IN ('fork','second'))
		+ (SELECT count(*) FROM payload_chunks WHERE thread_id IN ('fork','second'))
		+ (SELECT count(*) FROM edit_file_snapshots WHERE thread_id IN ('fork','second'))`).Scan(&copied); err != nil {
		t.Fatal(err)
	}
	if copied != 0 {
		t.Fatalf("forks store %d payload rows, want none", copied)
	}
	for _, thread := range []string{"fork", "second"} {
		requireForkPayload(t, s, thread, "base chunk")
		content, found, err := s.GetEditFileSnapshot(thread, "payload", "file")
		if err != nil || !found || content != "original" {
			t.Fatalf("%s edit=%q found=%v: %v", thread, content, found, err)
		}
	}
}

// TestPointerForkPayloadMutationsStayOnTheirSide: a content write on either
// side leaves the other's history as it was; an edit snapshot is a cache of
// the edit and is shared where the payload is.
func TestPointerForkPayloadMutationsStayOnTheirSide(t *testing.T) {
	for _, change := range []string{"append source", "replace source", "edit source", "append fork", "replace fork", "edit fork"} {
		t.Run(change, func(t *testing.T) {
			s := payloadForkFixture(t)
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
				requireForkPayload(t, s, thread, want)
				edit := "original"
				if strings.HasPrefix(change, "edit ") {
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

// TestPointerForkPayloadRestore: a snapshot restores the fork's pointer, and
// the fork's history then belongs to the restored source again.
func TestPointerForkPayloadRestore(t *testing.T) {
	s := payloadForkFixture(t)
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
	requireForkPayload(t, s, "fork", "base chunk")
	if n := ownRowCount(t, s, "fork"); n != 1 {
		t.Fatalf("restored fork stores %d rows, want only its divider", n)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPayloadData("fork", "payload"); err == nil {
		t.Fatal("fork reads the deleted source's payload")
	}
}

// TestPointerForkImportedPayloads: a fork reads the source's imported
// history, and its own write copies the rows that reference the payload.
func TestPointerForkImportedPayloads(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "source")
	if err := s.ApplyImportBatch("source", importBatchFixture("source")); err != nil {
		t.Fatal(err)
	}
	want, err := s.GetPayloadData("source", "payload-out")
	if err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "source", "fork", ForkCut{})
	if got, err := s.GetPayloadData("fork", "payload-out"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("fork=%q want=%q err=%v", got, want, err)
	}
	if err := s.AppendPayloadData("fork", "payload-out", []byte(" extra"), "{}", 3); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetPayloadData("fork", "payload-out"); err != nil || string(got) != string(want)+" extra" {
		t.Fatalf("appended=%q: %v", got, err)
	}
	if got, err := s.GetPayloadData("source", "payload-out"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("source=%q: %v", got, err)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetPayloadData("fork", "payload-out"); err != nil || string(got) != string(want)+" extra" {
		t.Fatalf("fork lost the row it owns: %q %v", got, err)
	}
	if err := s.DeleteThread("fork"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM import_history_chunks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained imports=%d: %v", count, err)
	}
}

// TestPointerForkPayloadQueriesStayIndexed: the payload views reach an
// ancestor's rows by index; nothing scans payloads across threads.
func TestPointerForkPayloadQueriesStayIndexed(t *testing.T) {
	s := payloadForkFixture(t)
	for _, query := range []string{
		`SELECT data FROM timeline_payloads WHERE thread_id=? AND id=?`,
		`SELECT data FROM timeline_payload_chunks WHERE thread_id=? AND payload_id=? ORDER BY chunk_index`,
		`SELECT content FROM timeline_edit_file_snapshots WHERE thread_id=? AND payload_id=? AND path='file'`,
	} {
		for _, row := range explainPlan(t, s, query, "fork", "payload") {
			if strings.HasPrefix(row.detail, "SCAN ") && !strings.Contains(row.detail, "timeline_") {
				t.Errorf("unbounded plan for %s: %s", query, row.detail)
			}
		}
	}
}

func TestPayloadViewsMigrationPreservesExistingData(t *testing.T) {
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

// TestPointerForkTransferCarriesInheritedBytes: exporting a fork carries the
// history it reads, so the received copy outlives the source.
func TestPointerForkTransferCarriesInheritedBytes(t *testing.T) {
	s := payloadForkFixture(t)
	destination := newTestStore(t)
	thread, err := s.GetThread("fork")
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := s.ExportThreadHistoryWith(t.Context(), thread.ID, &data, ThreadHistoryExport{}); err != nil {
		t.Fatal(err)
	}
	// The install clears links to threads the destination does not have.
	thread.ID, thread.ForkedFromThreadID = "received", ""
	if err := destination.ImportThreadHistory(t.Context(), thread, &data); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	requireForkPayload(t, destination, thread.ID, "base chunk")
	requireForkPayload(t, s, "fork", "base chunk")
}

// TestPointerForkDiffReaders: the edit-diff readers resolve a fork's
// inherited diff rows and their patch payloads, and stop showing them once
// the source, which owns them, is deleted.
func TestPointerForkDiffReaders(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "source")
	patch := []byte("--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n")
	item := Item{ThreadID: "source", ID: "edit", Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: "patch", Meta: "{}"}
	if err := s.InsertItemWithPayload(item, Payload{ID: "patch", Kind: "diff", Meta: "{}", Data: patch}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "source", "fork", ForkCut{})
	list, err := s.ListEditDiffItems("fork")
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%+v: %v", list, err)
	}
	patches, err := s.ListTurnEditDiffPatches("fork", 0)
	if err != nil || len(patches) != 1 || !bytes.Equal(patches[0].Data, patch) {
		t.Fatalf("patches=%+v: %v", patches, err)
	}
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListEditDiffItems("fork"); err != nil || len(list) != 0 {
		t.Fatalf("list after source deletion=%+v: %v", list, err)
	}
}

// SQLite must answer preview byte counts from record headers. Evaluating
// length(COALESCE(blob,...)) instead reads entire tool outputs first. The
// fork's read goes through the lineage arms.
func TestPointerForkPayloadLengthsNeverMaterializeBlobs(t *testing.T) {
	s := payloadForkFixture(t)
	type physicalBlob struct {
		name   string
		column int
	}
	roots := map[int]physicalBlob{}
	for table, column := range map[string]int{"payloads": 4, "import_history_payloads": 4, "payload_chunks": 4} {
		var root int
		if err := s.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&root); err != nil {
			t.Fatal(err)
		}
		roots[root] = physicalBlob{table, column}
	}
	query, args, err := payloadLengthsQuery(s.db, "fork", "payload")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("EXPLAIN "+query, args...)
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
	if base, _, err := s.payloadLengths("fork", "payload"); err != nil || base != len("base") {
		t.Fatalf("fork payload length = %d err=%v", base, err)
	}
}
