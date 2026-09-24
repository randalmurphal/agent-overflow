package store

import (
	"slices"
	"testing"
)

// TestMigrationV123ForkCopies: a record names a row its thread stores,
// once, with the ancestor it came from, indexed by that ancestor, and
// leaves with the row, whether the row or its thread is deleted.
func TestMigrationV123ForkCopies(t *testing.T) {
	db := migrateThrough(t, 122)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	for _, id := range []string{"F", "K"} {
		mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES(?,'p',?,'claude','/p',1,1)`, id, id)
		for index, item := range []string{"a", "b"} {
			mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at)
				VALUES(?,?,0,?,'assistant_text','assistant','completed','row','{}',1,1)`, id, item, index)
		}
	}
	migrateFrom(t, db, 122)

	var withoutRowID bool
	if err := db.QueryRow(`SELECT sql LIKE '%WITHOUT ROWID%' FROM sqlite_master WHERE type = 'table' AND name = 'thread_fork_copied'`).Scan(&withoutRowID); err != nil || !withoutRowID {
		t.Fatalf("thread_fork_copied is a WITHOUT ROWID table: %v, %v", withoutRowID, err)
	}
	for _, record := range [][2]string{{"F", "a"}, {"F", "b"}, {"K", "a"}} {
		mustExec(t, db, `INSERT INTO thread_fork_copied(thread_id,item_id,source_id) VALUES(?,?,'S')`, record[0], record[1])
	}
	if _, err := db.Exec(`INSERT INTO thread_fork_copied(thread_id,item_id,source_id) VALUES('F','a','T')`); err == nil {
		t.Fatal("a row was recorded twice")
	}
	if _, err := db.Exec(`INSERT INTO thread_fork_copied(thread_id,item_id,source_id) VALUES('F','missing','S')`); err == nil {
		t.Fatal("a row the thread does not store was recorded")
	}
	records := func() []string {
		t.Helper()
		ids, err := queryIDs(db, `SELECT thread_id || '/' || item_id FROM thread_fork_copied ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	var indexed bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pragma_index_info('idx_thread_fork_copied_source') WHERE seqno = 1 AND name = 'source_id')`).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("idx_thread_fork_copied_source leads with thread_id, source_id: %v, %v", indexed, err)
	}
	mustExec(t, db, `DELETE FROM items WHERE thread_id = 'F' AND id = 'a'`)
	if got, want := records(), []string{"F/b", "K/a"}; !slices.Equal(got, want) {
		t.Fatalf("records after an item delete = %v, want %v", got, want)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id = 'K'`)
	if got, want := records(), []string{"F/b"}; !slices.Equal(got, want) {
		t.Fatalf("records after a thread delete = %v, want %v", got, want)
	}
}
