package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestMigrationV120PointerForks: the migration copies every payload a thread
// borrowed through a v107 snapshot back into its own rows, whichever of the
// three places the snapshot held the bytes in, retires the snapshot
// triggers, and installs the pointer-fork schema whose views read a fork's
// history through its lineage.
func TestMigrationV120PointerForks(t *testing.T) {
	db := migrateThrough(t, 119)
	mustExec(t, db, `INSERT INTO threads(id,provider,workspace_path,created_at,updated_at) VALUES
		('src','claude','/tmp',1,1),('borrower','claude','/tmp',1,1),('keeper','claude','/tmp',1,1),('importer','claude','/tmp',1,1)`)
	// Borrowed from a live source payload, with its chunk and edit.
	mustExec(t, db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('src','p','text','{}',CAST('base' AS BLOB),1)`)
	mustExec(t, db, `INSERT INTO payload_chunks(thread_id,payload_id,chunk_index,start_offset,data,created_at) VALUES('src','p',0,4,CAST(' chunk' AS BLOB),1)`)
	mustExec(t, db, `INSERT INTO edit_file_snapshots(thread_id,payload_id,path,content,created_at) VALUES('src','p','file','original',1)`)
	mustExec(t, db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('borrower','p','text','{}',x'',1)`)
	mustExec(t, db, `INSERT INTO payload_snapshots(id,payload_id,source_thread_id) VALUES('live','p','src')`)
	mustExec(t, db, `INSERT INTO payload_snapshot_refs(thread_id,payload_id,snapshot_id) VALUES('borrower','p','live')`)
	// Preserved in the snapshot by a later write of its source.
	mustExec(t, db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('keeper','q','text','{}',x'',1)`)
	mustExec(t, db, `INSERT INTO payload_snapshots(id,payload_id,data) VALUES('kept','q',CAST('kept' AS BLOB))`)
	mustExec(t, db, `INSERT INTO payload_snapshot_chunks(snapshot_id,chunk_index,start_offset,data,created_at) VALUES('kept',0,4,CAST(' tail' AS BLOB),1)`)
	mustExec(t, db, `INSERT INTO payload_snapshot_edits(snapshot_id,path,content,created_at) VALUES('kept','path','kept edit',1)`)
	mustExec(t, db, `INSERT INTO payload_snapshot_refs(thread_id,payload_id,snapshot_id) VALUES('keeper','q','kept')`)
	// Borrowed from imported history no thread reads any more.
	mustExec(t, db, `INSERT INTO import_history_chunks(id,item_count,min_turn_index,max_turn_index) VALUES('orphan',1,0,0)`)
	mustExec(t, db, `INSERT INTO import_history_payloads(chunk_id,id,kind,meta,data,created_at) VALUES('orphan','r','text','{}',CAST('imported' AS BLOB),1)`)
	mustExec(t, db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('importer','r','text','{}',x'',1)`)
	mustExec(t, db, `INSERT INTO payload_snapshots(id,payload_id,chunk_id) VALUES('imp','r','orphan')`)
	mustExec(t, db, `INSERT INTO payload_snapshot_refs(thread_id,payload_id,snapshot_id) VALUES('importer','r','imp')`)

	want := []string{
		"borrower/p=base chunks=[ chunk] edits=[file:original]",
		"importer/r=imported chunks=[] edits=[]",
		"keeper/q=kept chunks=[ tail] edits=[path:kept edit]",
		"src/p=base chunks=[ chunk] edits=[file:original]",
	}
	requireIDs(t, "payloads before", v120PayloadState(t, db, "timeline_payloads", "timeline_payload_chunks", "timeline_edit_file_snapshots"), want)
	migrateFrom(t, db, 119)
	requireIDs(t, "payloads through the views", v120PayloadState(t, db, "timeline_payloads", "timeline_payload_chunks", "timeline_edit_file_snapshots"), want)
	requireIDs(t, "physical payloads", v120PayloadState(t, db, "payloads", "payload_chunks", "edit_file_snapshots"), want)

	var leftovers int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM payload_snapshots) + (SELECT count(*) FROM payload_snapshot_refs)
		+ (SELECT count(*) FROM import_history_chunks)
		+ (SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND (name LIKE 'trg_snapshot_%' OR name LIKE 'trg_payload_snapshot%'))`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("%d snapshot rows, orphaned chunks or triggers survived", leftovers)
	}
	for _, object := range []string{
		"thread_fork_lineage", "thread_fork_hidden", "idx_thread_fork_lineage_ancestor", "idx_threads_fork_source", "idx_items_unsettled",
		"trg_threads_fork_history", "trg_threads_fork_source_delete", "trg_items_fork_position", "trg_items_fork_position_update",
		"trg_items_fork_snapshot", "trg_items_fork_snapshot_move", "timeline_items", "timeline_payloads", "timeline_payload_chunks",
		"timeline_edit_file_snapshots", "timeline_turns",
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, object).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s missing: %d %v", object, n, err)
		}
	}
	var cut int
	if err := db.QueryRow(`SELECT fork_cut_turn_index + fork_cut_item_index + length(fork_source_thread_id) + length(fork_source_title)
		FROM threads WHERE id = 'src'`).Scan(&cut); err != nil || cut != 0 {
		t.Fatalf("existing thread fork columns = %d, %v", cut, err)
	}

	// The migrated schema reads a fork through its lineage and guards it.
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,payload_id,meta,created_at,updated_at) VALUES
		('src','u0',0,0,'user_text','user','completed','',NULL,'{}',1,1),
		('src','a0',0,1,'assistant_text','assistant','completed','','p','{}',1,1),
		('src','u1',1,0,'user_text','user','completed','',NULL,'{}',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,provider,workspace_path,created_at,updated_at,fork_source_thread_id,fork_cut_turn_index,fork_cut_item_index)
		VALUES('fork','claude','/tmp',1,1,'src',0,2)`)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('fork',1,'src',0,2)`)
	var ids []string
	rows, err := db.Query(`SELECT id FROM timeline_items WHERE thread_id = 'fork' ORDER BY turn_index, item_index`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	requireIDs(t, "fork rows", ids, []string{"u0", "a0"})
	var data []byte
	if err := db.QueryRow(`SELECT data FROM timeline_payloads WHERE thread_id = 'fork' AND id = 'p'`).Scan(&data); err != nil || string(data) != "base" {
		t.Fatalf("fork payload = %q, %v", data, err)
	}
	for _, refused := range []struct{ statement, reason string }{
		{`DELETE FROM threads WHERE id = 'src'`, "detach them before deleting it"},
		{`INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,meta,created_at,updated_at) VALUES('fork','early',0,0,'user_text','user','completed','{}',1,1)`, "precedes the fork cut"},
		{`INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('fork',33,'src',0,0)`, "CHECK constraint failed"},
	} {
		if _, err := db.Exec(refused.statement); err == nil || !strings.Contains(err.Error(), refused.reason) {
			t.Fatalf("%s: %v, want %q", refused.statement, err, refused.reason)
		}
	}
}

// v120PayloadState renders every payload of the fixture threads with its
// chunks and edit snapshots, read from the named tables or views.
func v120PayloadState(t *testing.T, db *sql.DB, payloads, chunks, edits string) []string {
	t.Helper()
	collect := func(query string, args ...any) []string {
		t.Helper()
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			out = append(out, value)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var out []string
	for _, key := range collect(`SELECT thread_id || '/' || id FROM ` + payloads + ` WHERE thread_id IN ('src','borrower','keeper','importer')`) {
		thread, id, _ := strings.Cut(key, "/")
		data := collect(`SELECT CAST(data AS TEXT) FROM `+payloads+` WHERE thread_id = ? AND id = ?`, thread, id)
		chunkData := collect(`SELECT CAST(data AS TEXT) FROM `+chunks+` WHERE thread_id = ? AND payload_id = ? ORDER BY chunk_index`, thread, id)
		editData := collect(`SELECT path || ':' || CAST(content AS TEXT) FROM `+edits+` WHERE thread_id = ? AND payload_id = ? ORDER BY path`, thread, id)
		out = append(out, fmt.Sprintf("%s=%s chunks=%v edits=%v", key, strings.Join(data, ","), chunkData, editData))
	}
	slices.Sort(out)
	return out
}
