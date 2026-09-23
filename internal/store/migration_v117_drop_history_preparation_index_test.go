package store

import "testing"

func TestMigrationV117DropsHistoryPreparationIndex(t *testing.T) {
	db := migrateThrough(t, 116)
	indexCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_items_history_preparation'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if indexCount() != 1 {
		t.Fatal("v108 index missing before v117")
	}
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES('t','p','t','claude','/p',1,1)`)
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES('before','t',0,0,'assistant_text','assistant','completed','kept',1,1)`)
	migrateFrom(t, db, 116)
	if indexCount() != 0 {
		t.Fatal("v117 kept idx_items_history_preparation")
	}
	mustExec(t, db, `INSERT INTO items(id,thread_id,turn_index,item_index,kind,role,status,summary,created_at,updated_at) VALUES('after','t',0,1,'assistant_text','assistant','completed','new',1,1)`)
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM items WHERE thread_id='t'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("items after v117 = %d, want 2", rows)
	}
}
