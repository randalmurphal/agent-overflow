package store

import (
	"strings"
	"testing"
)

// TestMigrationV131ShownHistoryFix: after v131 the payload guards refuse a
// write to payload content a fork shows, and let the same write through
// while shown_history_fix holds its row, which it holds one of at most.
func TestMigrationV131ShownHistoryFix(t *testing.T) {
	db := migrateThrough(t, 130)
	mustExec(t, db, `INSERT INTO threads(id,title,provider,workspace_path,created_at,updated_at) VALUES
		('S','S','claude','/p',1,1),('F','F','claude','/p',1,1)`)
	mustExec(t, db, `INSERT INTO payloads(thread_id,id,kind,meta,data,created_at) VALUES('S','p','text','{}',CAST('base' AS BLOB),1)`)
	mustExec(t, db, `INSERT INTO payload_chunks(thread_id,payload_id,chunk_index,start_offset,data,created_at) VALUES('S','p',0,4,CAST(' chunk' AS BLOB),1)`)
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,payload_id,meta,created_at,updated_at) VALUES
		('S','tool',0,0,'tool_call','assistant','completed','tool','p','{}',1,1)`)
	mustExec(t, db, `INSERT INTO thread_fork_lineage(thread_id,depth,ancestor_id,cut_turn_index,cut_item_index) VALUES('F',1,'S',1,0)`)
	migrateFromThrough(t, db, 130, 131)

	writes := []string{
		`UPDATE payloads SET data = X'', spans = '' WHERE thread_id = 'S' AND id = 'p'`,
		`UPDATE payload_chunks SET data = X'' WHERE thread_id = 'S' AND payload_id = 'p'`,
		`INSERT INTO payload_chunks(thread_id,payload_id,chunk_index,start_offset,data,created_at) VALUES('S','p',1,10,CAST('more' AS BLOB),2)`,
		`DELETE FROM payload_chunks WHERE thread_id = 'S' AND payload_id = 'p'`,
	}
	for _, write := range writes {
		if _, err := db.Exec(write); err == nil || !strings.Contains(err.Error(), shownHistoryImmutable) {
			t.Fatalf("%s = %v, want refused", write, err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO shown_history_fix(id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	for _, write := range writes {
		if _, err := tx.Exec(write); err != nil {
			t.Fatalf("%s with the row = %v, want allowed", write, err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO shown_history_fix(id) VALUES (2)`); err == nil {
		t.Fatal("shown_history_fix took a second row")
	}
	if _, err := tx.Exec(`DELETE FROM shown_history_fix`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var data string
	var chunks int
	if err := db.QueryRow(`SELECT CAST(data AS TEXT), (SELECT count(*) FROM payload_chunks WHERE thread_id = 'S') FROM payloads WHERE thread_id = 'S' AND id = 'p'`).
		Scan(&data, &chunks); err != nil || data != "" || chunks != 0 {
		t.Fatalf("payload = %q with %d chunks, %v; want it emptied", data, chunks, err)
	}
}
