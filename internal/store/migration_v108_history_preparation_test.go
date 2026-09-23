package store

import "testing"

func TestMigrationV108PreparedChunkAdmissionAndIndexes(t *testing.T) {
	db := migrateThrough(t, 107)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES('t','p','t','claude','/p',1,1)`)
	migrateFrom(t, db, 107)
	assertPlanUses(t, db, "idx_items_history_preparation", `EXPLAIN QUERY PLAN SELECT DISTINCT items.thread_id FROM items WHERE `+historyPreparationPredicate+` AND items.thread_id>? ORDER BY items.thread_id LIMIT 32`, "")
	assertPlanUses(t, db, "idx_import_history_items_id", `EXPLAIN QUERY PLAN SELECT chunk_id FROM import_history_items WHERE id=?`, "same")
	assertPlanUses(t, db, "idx_import_history_payloads_id", `EXPLAIN QUERY PLAN SELECT chunk_id FROM import_history_payloads WHERE id=?`, "payload")
	chunk := func(id, item, payload string, turn, index int) {
		t.Helper()
		mustExec(t, db, `INSERT INTO import_history_chunks VALUES(?,1,?,?)`, id, turn, turn)
		mustExec(t, db, `INSERT INTO import_history_payloads(chunk_id,id,kind,data,created_at) VALUES(?,?,'text',X'61',1)`, id, payload)
		mustExec(t, db, `INSERT INTO import_history_items(chunk_id,id,turn_index,item_index,kind,role,summary,payload_id,created_at,updated_at) VALUES(?,?,?,?,'assistant_text','assistant','text',?,1,1)`, id, item, turn, index, payload)
	}
	chunk("first", "same", "payload", 0, 0)
	mustExec(t, db, `INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id) VALUES('t',0,'first')`)
	chunk("same-turn", "other", "other-payload", 0, 1)
	mustExec(t, db, `INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id) VALUES('t',1,'same-turn')`)
	for _, fixture := range []struct {
		id, item, payload string
		turn, index       int
	}{
		{"duplicate-id", "same", "new", 8, 0}, {"duplicate-payload", "new", "payload", 9, 0}, {"duplicate-position", "newer", "newer-payload", 0, 0},
	} {
		chunk(fixture.id, fixture.item, fixture.payload, fixture.turn, fixture.index)
		if _, err := db.Exec(`INSERT INTO thread_import_chunks(thread_id,chunk_order,chunk_id) VALUES('t',2,?)`, fixture.id); err == nil {
			t.Fatalf("accepted %s", fixture.id)
		}
	}
}
